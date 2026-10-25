package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
	"github.com/dharaneswar339/durexec/internal/faultinj"
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

	// Drain, when closed, stops this worker POLLING while leaving the activity
	// it is currently running alone. It matters most here: cancelling an
	// activity's context mid-call abandons an external effect whose outcome
	// nobody knows and guarantees it is invoked again after the restart, which
	// is what every rolling deployment used to do. It may be nil.
	Drain <-chan struct{}
}

// Run polls until ctx is cancelled or the shard is lost.
func (w *ActivityWorker) Run(ctx context.Context) {
	idle := taskq.IdleMin
	for {
		if ctx.Err() != nil {
			return
		}
		// Immediately before the poll, so a drain can only ever stop this
		// worker between activities and never in the middle of one. That is
		// the whole difference between draining and cancelling: an activity
		// cut off mid-call has performed an unknown amount of the world's
		// work and will be invoked again after the restart.
		if draining(w.Drain) {
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

	// The claim has to be renewed while the activity runs, because this is the
	// one step in the engine that can legitimately outlast the task lock. See
	// keepClaimed for what goes wrong without it.
	actx, settleClaim := w.keepClaimed(ctx, task)
	outcome := e.Executor.Execute(actx, activity.Request{
		ActivityType: sched.ActivityType,
		Input:        sched.Input,
		DedupKey:     sched.DedupKey,
	})
	if settleClaim() {
		// The claim lapsed while we were working, so another worker may already
		// hold this task and may already be running the same activity. The one
		// thing we must not do is commit: the result is no longer ours to
		// journal, and writing it would delete a task row we do not hold.
		//
		// The task is left exactly as it is. Its lock has expired, so the queue
		// will hand it to whoever polls next, and the dedup key means the call
		// that did land is not repeated in effect.
		e.Runtime.countFailure(string(core.TaskActivity), reasonLockLost)
		log.Warn(ctx, "activity task claim lost while running, abandoning the result",
			"activity", sched.ActivityType, "task_id", task.TaskID)
		return nil
	}

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
		// The activity worker had the same defect as the workflow worker: both
		// causes of lost ownership ended the worker for good, and only one of
		// them is a reason to stop.
		var race *eventlog.SeqRaceError
		if errors.As(err, &race) {
			// We still own the shard; another writer just reached this sequence
			// number first. Nothing was written, the task row is untouched, and
			// the activity's dedup key means re-running it does not repeat the
			// side effect. So hand the task back and keep polling.
			e.Runtime.countFailure(string(core.TaskActivity), reasonSeqRace)
			log.Debug(ctx, "activity result lost a sequence race, retrying task",
				"seq", int64(race.Seq))
			w.releaseForRetry(ctx, task)
			return nil
		}
		if errors.Is(err, core.ErrLostOwnership) {
			// Fenced out: another node owns the shard now. Our transaction was
			// refused and the task row is untouched, so that node will run it.
			// Expected, not exceptional.
			//
			// fencing_rejections_total is not incremented here; the committer
			// counts it where the token mismatch is actually seen, so that one
			// number stays evidence of fencing and nothing else.
			e.Runtime.countFailure(string(core.TaskActivity), reasonFenced)
			log.Debug(ctx, "activity result abandoned: fenced out")
			return errShardGone
		}
		e.Runtime.countFailure(string(core.TaskActivity), reasonCommit)
		log.Error(ctx, "commit activity result", "error", err)
		w.releaseForRetry(ctx, task)
		return nil
	}

	// AfterActivityJournaled. DURABLE AT THIS POINT, all in the one transaction
	// that just committed: the activity's result event, the workflow wake-up
	// task that will deliver it, and the DELETION of this activity task row.
	// NOT durable: anything the workflow decides next.
	//
	// A process that dies here must therefore lose nothing and must not call
	// the activity again. Recovery is the wake-up task: another worker polls
	// it, replays, and resolvedAt finds the result already in history. This is
	// the seam that proves that, and it had never been called from production
	// code — which is why the start-path orphan survived the whole test suite:
	// the tests killed nodes, not transactions.
	//
	// Inert unless explicitly armed; see internal/faultinj.
	if w.Engine.Faults.ShouldCrash(faultinj.AfterActivityJournaled) {
		log.Warn(ctx, "injected crash after the activity result was journaled",
			"activity", sched.ActivityType, "seq", int64(seq))
		return errCrashInjected
	}
	return nil
}

// errCrashInjected is how an armed crash point leaves a worker loop. It is the
// loop's private signal, like errShardGone, and it never escapes the package.
var errCrashInjected = errors.New("runtime: injected crash")

// claimRenewalFraction is the share of the lock duration between renewals.
//
// A third means two consecutive renewals can fail — a brief network stall, a
// primary failover — before the lock actually lapses, which is the point of
// renewing early rather than at the last moment.
const claimRenewalFraction = 3

// minClaimRenewal floors the interval so a misconfigured, very short lock
// cannot turn the heartbeat into a busy loop against PostgreSQL.
const minClaimRenewal = 10 * time.Millisecond

// claimRenewalEvery is how often a running activity renews its task claim.
//
// It is derived from the configured lock duration rather than written as a
// constant on purpose: the two numbers only mean anything relative to each
// other, and a deployment that shortens TASK_LOCK must shorten the heartbeat
// with it or it silently gets concurrent execution back.
func claimRenewalEvery(lock time.Duration) time.Duration {
	every := lock / claimRenewalFraction
	if every < minClaimRenewal {
		every = minClaimRenewal
	}
	return every
}

// keepClaimed renews the task's claim for as long as the activity is running.
//
// This is the fix for the one place where this engine's own promise worked
// against it. Poll takes a claim that lasts TASK_LOCK (30s by default). An
// activity that runs longer than that — and for "long-running workflows" that
// is the normal case, not the exceptional one, since the executor's in-process
// retries spend the same budget — had its claim lapse while it was still
// working. The row became pollable, a second worker claimed it, and the same
// activity ran CONCURRENTLY on a perfectly healthy node. Queue.Extend existed
// for exactly this and nothing outside the tests ever called it.
//
// Two things matter in the shape of this:
//
// The returned context is the one the activity must run under. When the claim
// is definitively gone the context is cancelled, so an activity sitting in an
// HTTP call stops there instead of returning a result this worker is no longer
// entitled to commit. Stopping work is the conservative direction: the task is
// already someone else's.
//
// Only taskq.ErrLockLost is treated as definitive. Any other error — the
// database briefly unreachable — is transient, and giving up the activity over
// one failed statement would turn a hiccup into abandoned work. Those are
// logged and retried on the next tick, which is why renewing at a third of the
// lock leaves room for two to fail.
//
// settleClaim must be called exactly once, before any other return from
// process: it stops the goroutine, waits for it, and reports whether the claim
// was lost. Waiting is what makes it leak-free — there is no path on which the
// heartbeat outlives the activity it was renewing.
func (w *ActivityWorker) keepClaimed(ctx context.Context, task core.Task) (context.Context, func() bool) {
	actx, cancel := context.WithCancel(ctx)
	if task.LockToken == "" {
		// Nothing to renew (a fake queue in a test, or a task handed over by
		// some path that holds no lock). Renewing would fail every tick and say
		// nothing useful, so do not start the goroutine at all.
		return actx, func() bool { cancel(); return false }
	}

	var lost atomic.Bool
	done := make(chan struct{})
	every := claimRenewalEvery(w.Engine.lockDuration())

	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-actx.Done():
				return
			case <-t.C:
			}
			err := w.Engine.Tasks.Extend(actx, w.Shard, task.TaskID, task.LockToken)
			switch {
			case err == nil:
			case errors.Is(err, taskq.ErrLockLost):
				// Definitive: the row is no longer ours. Stop the activity.
				lost.Store(true)
				cancel()
				return
			case actx.Err() != nil:
				// The activity finished (or the node is shutting down) while
				// the renewal was in flight. Not a lost claim.
				return
			default:
				w.Engine.logger().Warn(ctx, "renew activity task claim",
					"task_id", task.TaskID, "error", err)
			}
		}
	}()

	return actx, func() bool {
		cancel()
		<-done
		return lost.Load()
	}
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
