package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/taskq"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// WorkflowWorker drives one shard's workflow tasks.
//
// One worker serves exactly one shard, and that is the whole concurrency
// model. Every write a workflow task makes is scoped to its shard and checked
// against that shard's fencing token, so two workers on different shards can
// never interfere; and if this node loses the shard, stopping this worker is
// all it takes to stop touching it.
type WorkflowWorker struct {
	Engine *Engine
	Shard  core.ShardID
}

// errShardGone is the loop's private signal that this node no longer holds the
// shard, so the loop should end rather than poll again. It never escapes the
// package: losing a shard is ordinary, and the node keeps running.
var errShardGone = errors.New("runtime: shard is no longer owned by this node")

// Run polls until ctx is cancelled or the shard is lost.
//
// The idle back-off comes from taskq.NextIdle so that a workflow worker and
// the queue's own poller cannot drift into two different policies: 10ms when
// there is work, doubling to a second when there is none.
func (w *WorkflowWorker) Run(ctx context.Context) {
	idle := taskq.IdleMin
	for {
		if ctx.Err() != nil {
			return
		}

		task, err := w.Engine.Tasks.Poll(ctx, w.Shard, core.TaskWorkflow)
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
			w.Engine.Runtime.countFailure(string(core.TaskWorkflow), reasonPoll)
			w.Engine.logger().Error(ctx, "poll workflow task", "shard", int(w.Shard), "error", err)
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

// process runs one workflow task from poll to commit.
func (w *WorkflowWorker) process(ctx context.Context, task core.Task) error {
	e := w.Engine
	log := e.logger().WithShard(w.Shard).WithWorkflow(task.WorkflowID, task.RunID)

	// Ownership first. The commit re-checks the token inside its own
	// transaction, so this early check is not the safety mechanism — it is
	// there so a fenced-out node stops doing pointless work and, importantly,
	// hands the task straight back instead of sitting on the lock until it
	// expires.
	tok, ok := e.TokenFor(w.Shard)
	if !ok {
		w.release(ctx, task, 0)
		log.Info(ctx, "stopping workflow worker: shard no longer owned")
		return errShardGone
	}

	ex, err := e.Executions.Get(ctx, w.Shard, task.WorkflowID)
	if err != nil {
		e.Runtime.countFailure(string(core.TaskWorkflow), reasonLoad)
		log.Error(ctx, "load execution", "error", err)
		w.releaseAfterFailure(ctx, task)
		return nil
	}
	if ex.Status.IsTerminal() {
		// The run finished and this task row outlived it — a worker can die
		// after committing a completion but before its task row is gone. There
		// is nothing to decide, so just clear the row.
		w.complete(ctx, task, tok, ex)
		return nil
	}

	hist, err := e.Events.Load(ctx, w.Shard, task.WorkflowID, ex.RunID)
	if err != nil {
		e.Runtime.countFailure(string(core.TaskWorkflow), reasonLoad)
		log.Error(ctx, "load history", "error", err)
		w.releaseAfterFailure(ctx, task)
		return nil
	}

	input, err := workflowInput(hist)
	if err != nil {
		// History with no usable start event is corrupt, and no amount of
		// retrying fixes it. Fail the run and say why.
		log.Error(ctx, "read workflow input", "error", err)
		return w.failTerminally(ctx, task, tok, ex, hist, err)
	}

	now := e.now().Now()

	// Facts that must be in history before the workflow runs again: a fired
	// timer, a human's answer to an escalation. See preface.go.
	lastSeq := highestSeq(ex.LastSeq, hist)
	pre := buildPreface(hist, lastSeq+1, now)
	pre.stamp(w.Shard, task.WorkflowID, ex.RunID)
	replayHist := hist
	if len(pre.events) > 0 {
		replayHist = make([]core.Event, 0, len(hist)+len(pre.events))
		replayHist = append(replayHist, hist...)
		replayHist = append(replayHist, pre.events...)
	}

	start := time.Now()
	res, err := e.Dispatcher.Execute(replayHist, ex.WorkflowType, input)
	replaySeconds := time.Since(start).Seconds()
	if e.Metrics != nil {
		e.Metrics.ObserveReplay(replaySeconds)
	}

	var nd *core.NonDeterminismError
	if errors.As(err, &nd) {
		// The code and the log tell two different stories about what this run
		// did, and the engine has no way to know which is true. Guessing could
		// charge a customer twice or skip a refund, so the run is failed and
		// the divergence is written down exactly as found. It is never
		// repaired: an operator redeploying the old code is the fix.
		log.Error(ctx, "non-determinism during replay",
			"k", nd.K, "seq", int64(nd.Seq), "recorded", nd.Recorded, "requested", nd.Requested)
		return w.failTerminally(ctx, task, tok, ex, hist, nd)
	}
	if err != nil {
		e.Runtime.countFailure(string(core.TaskWorkflow), reasonDispatch)
		log.Error(ctx, "execute workflow", "error", err)
		w.releaseAfterFailure(ctx, task)
		return nil
	}

	// The semantic duplicate check. It runs here — after the dispatcher has
	// decided what it wants to do and before any of it becomes an event —
	// because this is the only point where a verdict can be computed once and
	// committed in the same transaction as the decision it governs.
	out, err := e.resolveSemantic(ctx, replayHist, &res, semanticParams{
		ShardID: w.Shard, WorkflowID: task.WorkflowID, RunID: ex.RunID,
	})
	if err != nil {
		// A checker failure is an availability problem, not a correctness one.
		// Release the task and let the next attempt try again rather than
		// committing a decision whose dedup question is unanswered.
		e.Runtime.countFailure(string(core.TaskWorkflow), reasonDispatch)
		log.Warn(ctx, "semantic dedup check failed, retrying task", "error", err)
		w.releaseAfterFailure(ctx, task)
		return nil
	}
	if len(out.verdicts) > 0 {
		// Verdicts first, so the log reads in the order the reasoning
		// happened: the engine decided this was a repeat, and then acted.
		res.Commands = append(append([]core.Command{}, out.verdicts...), res.Commands...)
	}

	d, err := workflow.BuildDecision(workflow.DecisionParams{
		ShardID:        w.Shard,
		WorkflowID:     task.WorkflowID,
		RunID:          ex.RunID,
		Token:          tok,
		LastSeq:        pre.lastSeq(lastSeq),
		CompleteTaskID: task.TaskID,
		Now:            now,
	}, res)
	if err != nil {
		log.Error(ctx, "build decision", "error", err)
		return w.failTerminally(ctx, task, tok, ex, hist, err)
	}

	// applyOutcome is what makes a DUPLICATE verdict actually skip the side
	// effect, and a held ESCALATE actually wait.
	if err := applyOutcome(&d, out, now.UnixNano()); err != nil {
		log.Error(ctx, "apply dedup outcome", "error", err)
		w.releaseAfterFailure(ctx, task)
		return nil
	}

	if !pre.empty() {
		d.Events = append(pre.events, d.Events...)
		d.NewTasks = append(d.NewTasks, pre.tasks...)
	}
	if d.Status == "" {
		d.Status = ex.Status
	}

	if err := e.Committer.CommitDecision(ctx, d); err != nil {
		if errors.Is(err, core.ErrLostOwnership) {
			// Being fenced is expected, not exceptional. Another node owns the
			// shard now, our transaction was refused, and the task row is
			// untouched for that node to pick up. There is nothing to log as
			// an error and nothing to retry.
			e.Runtime.countFailure(string(core.TaskWorkflow), reasonFenced)
			if e.Metrics != nil {
				e.Metrics.FencingRejection(w.Shard)
			}
			log.Debug(ctx, "workflow decision abandoned: fenced out")
			return errShardGone
		}
		e.Runtime.countFailure(string(core.TaskWorkflow), reasonCommit)
		log.Error(ctx, "commit workflow decision", "error", err)
		w.releaseAfterFailure(ctx, task)
		return nil
	}

	if res.Status.IsTerminal() && e.Metrics != nil {
		e.Metrics.WorkflowFinished(ex.WorkflowType, res.Status)
	}
	if res.Status.IsTerminal() {
		log.Info(ctx, "workflow finished", "status", string(res.Status), "last_seq", int64(d.LastSeq))
	}
	return nil
}

// failTerminally writes the failure the engine refuses to work around.
func (w *WorkflowWorker) failTerminally(
	ctx context.Context, task core.Task, tok core.FencingToken,
	ex core.Execution, hist []core.Event, cause error,
) error {
	e := w.Engine
	now := e.now().Now()
	seq := highestSeq(ex.LastSeq, hist) + 1
	payload, merr := json.Marshal(core.WorkflowFailed{Error: cause.Error()})
	if merr != nil {
		payload = json.RawMessage(`{"error":"unencodable failure"}`)
	}
	d := core.Decision{
		ShardID: w.Shard, WorkflowID: task.WorkflowID, RunID: ex.RunID, Token: tok,
		Events: []core.Event{{
			ShardID: w.Shard, WorkflowID: task.WorkflowID, RunID: ex.RunID,
			Seq: seq, Type: core.EvWorkflowFailed, Payload: payload, CreatedAt: now,
		}},
		Status: core.StatusFailed, LastSeq: seq, CompleteTaskID: task.TaskID,
	}
	if err := e.Committer.CommitDecision(ctx, d); err != nil {
		if errors.Is(err, core.ErrLostOwnership) {
			return errShardGone
		}
		e.Runtime.countFailure(string(core.TaskWorkflow), reasonCommit)
		e.logger().Error(ctx, "journal terminal workflow failure", "error", err)
		return nil
	}
	if e.Metrics != nil {
		e.Metrics.WorkflowFinished(ex.WorkflowType, core.StatusFailed)
	}
	return nil
}

// complete clears a task for a run that is already finished.
func (w *WorkflowWorker) complete(ctx context.Context, task core.Task, tok core.FencingToken, ex core.Execution) {
	err := w.Engine.Committer.CommitDecision(ctx, core.Decision{
		ShardID: w.Shard, WorkflowID: task.WorkflowID, RunID: ex.RunID, Token: tok,
		Status: ex.Status, LastSeq: ex.LastSeq, CompleteTaskID: task.TaskID,
	})
	if err != nil && !errors.Is(err, core.ErrLostOwnership) {
		w.Engine.logger().Error(ctx, "clear task for finished run", "error", err)
	}
}

// release hands a task back immediately, without a retry delay.
func (w *WorkflowWorker) release(ctx context.Context, task core.Task, visibleIn int64) {
	// A fresh context, because the usual reason to release is that ours is
	// being cancelled, and a cancelled context cannot issue the statement. The
	// task would eventually time out anyway; releasing just makes it instant.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := w.Engine.Tasks.Release(rctx, w.Shard, task.TaskID, task.LockToken, visibleIn); err != nil {
		w.Engine.logger().Debug(ctx, "release task", "task_id", task.TaskID, "error", err)
	}
}

// releaseAfterFailure pushes a task's visibility out by the jittered backoff
// for its attempt count, so a task that fails fast does not spin the fleet.
func (w *WorkflowWorker) releaseAfterFailure(ctx context.Context, task core.Task) {
	w.Engine.Runtime.countFailure(string(task.Type), reasonRetry)
	w.release(ctx, task, w.Engine.backoff().Jitter(task.Attempt, randFraction()).Milliseconds())
}

// workflowInput digs the workflow's argument out of the start event.
//
// StartWorkflow writes {"workflow_type":..., "input":...}, and the input is
// handed back as the exact bytes that were stored. Re-marshalling a decoded
// value would risk a different byte sequence on a later Go version, and the
// workflow's determinism rests on seeing identical input every time.
func workflowInput(hist []core.Event) (json.RawMessage, error) {
	for i := range hist {
		if hist[i].Type != core.EvWorkflowStarted {
			continue
		}
		var p struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(hist[i].Payload, &p); err != nil {
			return nil, fmt.Errorf("runtime: undecodable %s payload: %w", core.EvWorkflowStarted, err)
		}
		if len(p.Input) == 0 {
			return json.RawMessage("null"), nil
		}
		return p.Input, nil
	}
	return nil, fmt.Errorf("runtime: history has no %s event", core.EvWorkflowStarted)
}

// highestSeq reconciles the execution row's pointer with what history actually
// holds. They agree in every normal case; taking the larger means a stale
// pointer produces a rejected commit rather than a silently overwritten event.
func highestSeq(fromExecution core.Seq, hist []core.Event) core.Seq {
	last := fromExecution
	for i := range hist {
		if hist[i].Seq > last {
			last = hist[i].Seq
		}
	}
	return last
}

// sleepCtx waits, and reports false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
