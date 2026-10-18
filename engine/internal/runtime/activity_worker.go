package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/taskq"
)

// ActivityWorker drives one shard's activity tasks: the only place in the
// engine where code is allowed to talk to the outside world.
//
// Its whole design follows from one fact it cannot change. The engine cannot
// guarantee that an activity is called at most once — a worker can succeed at
// a payment API and die before it writes that down, and nothing inside this
// process can tell the difference afterwards. So the guarantee is arranged
// elsewhere: the call carries a dedup key that is identical on every attempt
// and after every crash, and the result is journaled in the same transaction
// that deletes the task and wakes the workflow. Either the world's change and
// our record of it both stand, or the task comes back and the same key goes
// out again.
type ActivityWorker struct {
	Engine *Engine
	Shard  core.ShardID
}

// Run polls until ctx is cancelled or the shard is lost.
func (w *ActivityWorker) Run(ctx context.Context) {
	idle := taskq.IdleMin
	for {
		if ctx.Err() != nil {
			return
		}

		task, err := w.Engine.Tasks.Poll(ctx, w.Shard, core.TaskActivity)
		switch {
		case errors.Is(err, core.ErrNoTask):
			idle = taskq.NextIdle(idle, false)
			if !sleepCtx(ctx, idle) {
				return
			}
			continue
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			w.Engine.Runtime.countFailure(string(core.TaskActivity), reasonPoll)
			w.Engine.logger().Error(ctx, "poll activity task", "shard", int(w.Shard), "error", err)
			idle = taskq.NextIdle(idle, false)
			if !sleepCtx(ctx, idle) {
				return
			}
			continue
		}

		idle = taskq.NextIdle(idle, true)
		if err := w.process(ctx, task); errors.Is(err, errShardGone) {
			return
		}
	}
}

func (w *ActivityWorker) process(ctx context.Context, task core.Task) error {
	e := w.Engine
	log := e.logger().WithShard(w.Shard).WithWorkflow(task.WorkflowID, task.RunID)

	tok, ok := e.TokenFor(w.Shard)
	if !ok {
		w.release(ctx, task, 0)
		log.Info(ctx, "stopping activity worker: shard no longer owned")
		return errShardGone
	}

	ex, err := e.Executions.Get(ctx, w.Shard, task.WorkflowID)
	if err != nil {
		e.Runtime.countFailure(string(core.TaskActivity), reasonLoad)
		log.Error(ctx, "load execution", "error", err)
		w.releaseForRetry(ctx, task)
		return nil
	}

	hist, err := e.Events.Load(ctx, w.Shard, task.WorkflowID, ex.RunID)
	if err != nil {
		e.Runtime.countFailure(string(core.TaskActivity), reasonLoad)
		log.Error(ctx, "load history", "error", err)
		w.releaseForRetry(ctx, task)
		return nil
	}

	// Everything the worker needs to run the activity came out of the log.
	// That is why it needs no memory of what it was doing before a crash.
	sched, ok := scheduledAt(hist, task.ScheduledEventSeq)
	if !ok {
		// A task pointing at an event that is not there cannot be repaired by
		// retrying it, and leaving it would make the queue poll it forever.
		log.Error(ctx, "activity task names no scheduling event", "seq", int64(task.ScheduledEventSeq))
		w.clear(ctx, task, tok, ex)
		return nil
	}

	if ex.Status.IsTerminal() || resolvedAt(hist, sched.K) {
		// Already answered. A duplicate task row survives a crash between the
		// commit and the delete, and running the activity again on the
		// strength of one would be the exact duplicate side effect this engine
		// exists to prevent.
		w.clear(ctx, task, tok, ex)
		return nil
	}

	outcome := e.Executor.Execute(ctx, activity.Request{
		ActivityType: sched.ActivityType,
		Input:        sched.Input,
		DedupKey:     sched.DedupKey,
	})

	if !outcome.OK() && w.shouldRetryLater(sched.ActivityType, outcome, task) {
		// The activity's own retry budget is spent for this attempt, but the
		// task still has attempts left. Handing the row back with a jittered
		// delay is strictly better than journaling a failure the workflow
		// would then have to compensate for: the outage may well be over by
		// the time the task becomes visible again, and the dedup key means a
		// call that did land is not repeated in effect.
		log.Warn(ctx, "activity failed, releasing task for another attempt",
			"activity", sched.ActivityType, "attempt", task.Attempt, "error", outcome.Err)
		w.releaseForRetry(ctx, task)
		return nil
	}

	now := e.now().Now()
	seq := highestSeq(ex.LastSeq, hist) + 1
	event, err := resultEvent(sched, outcome, seq, now)
	if err != nil {
		log.Error(ctx, "encode activity result", "error", err)
		w.releaseForRetry(ctx, task)
		return nil
	}
	event.ShardID, event.WorkflowID, event.RunID = w.Shard, task.WorkflowID, ex.RunID

	d := core.Decision{
		ShardID: w.Shard, WorkflowID: task.WorkflowID, RunID: ex.RunID, Token: tok,
		Events: []core.Event{event},
		// The workflow cannot notice a result on its own: it is not running.
		// Waking it is part of the same transaction as recording the result,
		// so a crash can never leave a journaled answer with nobody coming
		// back to read it.
		NewTasks: []core.Task{{
			ShardID: w.Shard, Type: core.TaskWorkflow,
			WorkflowID: task.WorkflowID, RunID: ex.RunID,
			ScheduledEventSeq: seq, VisibleAt: now,
		}},
		Status: ex.Status, LastSeq: seq, CompleteTaskID: task.TaskID,
	}
	if outcome.OK() {
		if in, ok := w.intent(ctx, sched, ex, task.ScheduledEventSeq); ok {
			d.Intents = append(d.Intents, in)
		}
	}

	if err := e.Committer.CommitDecision(ctx, d); err != nil {
		if errors.Is(err, core.ErrLostOwnership) {
			// Fenced out, or beaten to this sequence number by another writer.
			// Either way our transaction was refused and the task row is
			// untouched, so the shard's new owner will run it. Expected, not
			// exceptional.
			//
			// That ambiguity is exactly why fencing_rejections_total is not
			// incremented here: this branch cannot tell the fencing check
			// firing from a lost race for a sequence number, and only the first
			// is evidence the safety mechanism works. The committer counts it
			// where the token mismatch is actually seen.
			e.Runtime.countFailure(string(core.TaskActivity), reasonFenced)
			log.Debug(ctx, "activity result abandoned: fenced out")
			return errShardGone
		}
		e.Runtime.countFailure(string(core.TaskActivity), reasonCommit)
		log.Error(ctx, "commit activity result", "error", err)
		w.releaseForRetry(ctx, task)
		return nil
	}
	return nil
}

// intent builds the meaning-vector row for a semantic-mode activity that just
// succeeded.
//
// It is written here, on completion, and not when the activity was scheduled.
// activity_intents has no status column by design — every row in it is a
// successful invocation, which is what lets the candidate query stay narrow —
// so a call that later failed must never leave a row behind for a future check
// to match against.
//
// A failure to embed is not a failure of the activity. The row is skipped, the
// result still lands, and the only consequence is that a later check has one
// fewer candidate and answers DISTINCT — which is the same safe direction the
// checker already takes when the model is unreachable.
func (w *ActivityWorker) intent(ctx context.Context, sched core.ActivityScheduled,
	ex core.Execution, schedSeq core.Seq) (core.Intent, bool) {
	e := w.Engine
	if sched.Mode != core.ModeSemantic || e.Checker == nil || e.Checker.Embedder == nil {
		return core.Intent{}, false
	}
	text, err := intentText(sched)
	if err != nil {
		e.logger().Warn(ctx, "build intent text", "activity", sched.ActivityType, "error", err)
		return core.Intent{}, false
	}
	vec, err := e.Checker.Embedder.Embed(ctx, text)
	if err != nil {
		e.logger().Warn(ctx, "embed completed intent", "activity", sched.ActivityType, "error", err)
		return core.Intent{}, false
	}
	return core.Intent{
		ShardID:      w.Shard,
		WorkflowID:   ex.WorkflowID,
		RunID:        ex.RunID,
		ScheduledSeq: schedSeq,
		ActivityType: sched.ActivityType,
		IntentText:   text,
		Embedding:    vec,
	}, true
}

// shouldRetryLater decides between handing the task back and journaling a
// terminal failure.
//
// There are two retry budgets and they are not the same thing. The executor's
// is in-process: it retries the implementation a few times over a few seconds,
// which covers a flaky call. The task's is durable: the row comes back with a
// larger delay, survives the process dying, and covers an outage that outlasts
// this worker. A failure declared non-retryable — a validation error — skips
// both, because no amount of waiting makes bad input good.
func (w *ActivityWorker) shouldRetryLater(activityType string, out activity.Outcome, task core.Task) bool {
	if out.NonRetryable || core.IsNonRetryable(out.Err) {
		return false
	}
	policy := activity.DefaultRetryPolicy()
	if w.Engine.Activities != nil {
		if def, ok := w.Engine.Activities.Get(activityType); ok && !def.Retry.IsZero() {
			policy = def.Retry
		}
	}
	return policy.AttemptsRemaining(task.Attempt)
}

// resultEvent turns an outcome into the single event that will be journaled.
func resultEvent(sched core.ActivityScheduled, out activity.Outcome, seq core.Seq, now time.Time) (core.Event, error) {
	if out.OK() {
		payload, err := json.Marshal(core.ActivityCompleted{K: sched.K, Result: out.Result})
		if err != nil {
			return core.Event{}, err
		}
		return core.Event{Seq: seq, Type: core.EvActivityCompleted, Payload: payload, CreatedAt: now}, nil
	}
	payload, err := json.Marshal(core.ActivityFailed{
		K:            sched.K,
		Error:        out.Err.Error(),
		Attempt:      out.Attempts,
		NonRetryable: out.NonRetryable,
	})
	if err != nil {
		return core.Event{}, err
	}
	return core.Event{Seq: seq, Type: core.EvActivityFailed, Payload: payload, CreatedAt: now}, nil
}

// clear deletes a task whose work is already done or can never be done.
func (w *ActivityWorker) clear(ctx context.Context, task core.Task, tok core.FencingToken, ex core.Execution) {
	err := w.Engine.Committer.CommitDecision(ctx, core.Decision{
		ShardID: w.Shard, WorkflowID: task.WorkflowID, RunID: ex.RunID, Token: tok,
		Status: ex.Status, LastSeq: ex.LastSeq, CompleteTaskID: task.TaskID,
	})
	if err != nil && !errors.Is(err, core.ErrLostOwnership) {
		w.Engine.logger().Error(ctx, "clear activity task", "error", err)
	}
}

func (w *ActivityWorker) release(ctx context.Context, task core.Task, visibleIn int64) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := w.Engine.Tasks.Release(rctx, w.Shard, task.TaskID, task.LockToken, visibleIn); err != nil {
		w.Engine.logger().Debug(ctx, "release task", "task_id", task.TaskID, "error", err)
	}
}

func (w *ActivityWorker) releaseForRetry(ctx context.Context, task core.Task) {
	w.Engine.Runtime.countFailure(string(core.TaskActivity), reasonRetry)
	w.release(ctx, task, w.Engine.backoff().Jitter(task.Attempt, randFraction()).Milliseconds())
}

// scheduledAt finds the ActivityTaskScheduled event a task names.
func scheduledAt(hist []core.Event, seq core.Seq) (core.ActivityScheduled, bool) {
	for i := range hist {
		ev := &hist[i]
		if ev.Seq != seq {
			continue
		}
		if ev.Type != core.EvActivityScheduled && ev.Type != core.EvCompensationScheduled {
			return core.ActivityScheduled{}, false
		}
		var p core.ActivityScheduled
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return core.ActivityScheduled{}, false
		}
		return p, true
	}
	return core.ActivityScheduled{}, false
}

// resolvedAt reports whether an activity at this index already has an answer.
func resolvedAt(hist []core.Event, k int) bool {
	for i := range hist {
		ev := &hist[i]
		switch ev.Type {
		case core.EvActivityCompleted, core.EvCompensationCompleted:
			var p core.ActivityCompleted
			if json.Unmarshal(ev.Payload, &p) == nil && p.K == k {
				return true
			}
		case core.EvActivityFailed, core.EvCompensationFailed:
			var p core.ActivityFailed
			if json.Unmarshal(ev.Payload, &p) == nil && p.K == k {
				return true
			}
		}
	}
	return false
}
