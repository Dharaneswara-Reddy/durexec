// Package runtime is the integration layer: it turns the independently-built
// packages into a running node. Nothing here invents behaviour — it wires the
// dispatcher to the queue, the queue to the committer, and the semantic checker
// into the one place it can legally run.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/api"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
	"github.com/dharaneswar339/durexec/internal/obs"
	"github.com/dharaneswar339/durexec/internal/shard"
	"github.com/dharaneswar339/durexec/internal/taskq"
	"github.com/dharaneswar339/durexec/internal/workflow"
	"github.com/google/uuid"
)

// Engine holds everything a node needs. It implements api.Starter,
// api.Signaller and api.Canceller so the HTTP layer can drive it without
// knowing any of this exists.
type Engine struct {
	Events     core.EventStore
	Executions core.ExecutionStore
	Committer  core.Committer
	Tasks      core.TaskQueue
	Leases     core.LeaseStore

	Dispatcher *workflow.Dispatcher
	Activities *activity.Registry
	Executor   *activity.Executor
	Checker    *semantic.Checker

	ShardCount int32
	NodeID     string
	Clock      core.Clock
	Metrics    *obs.Metrics
	Log        *obs.Logger

	// Runtime holds the counters that only the worker loops can produce. It
	// may be nil; every call through it is nil-safe.
	Runtime *Metrics

	// EscalationBlocks selects the stricter policy for the uncertain band:
	// false (the default) executes the activity, true holds it for a human.
	// The argument for the default is in dedup.go, where the choice is made.
	EscalationBlocks bool

	// Backoff spaces out a task that is being retried. Zero fields take the
	// queue package's defaults.
	Backoff taskq.Backoff

	// TokenFor reports the fencing token this node currently holds for a
	// shard. It is wired to lease.Manager.Token. Every Decision carries the
	// token, and the commit re-verifies it FOR UPDATE, so a stale answer here
	// costs a rejected transaction rather than a corrupted log.
	TokenFor func(core.ShardID) (core.FencingToken, bool)
}

var (
	_ api.Starter   = (*Engine)(nil)
	_ api.Signaller = (*Engine)(nil)
	_ api.Canceller = (*Engine)(nil)
)

func (e *Engine) now() core.Clock {
	if e.Clock != nil {
		return e.Clock
	}
	return core.SystemClock{}
}

// ShardFor satisfies api.ShardResolver.
func (e *Engine) ShardFor(workflowID string) core.ShardID {
	return shard.ShardFor(workflowID, e.ShardCount)
}

// StartWorkflow creates the execution row, writes the first event and enqueues
// the first workflow task — all in one Decision, so a start either fully
// happens or does not happen at all.
func (e *Engine) StartWorkflow(ctx context.Context, req api.StartRequest) (api.StartResult, error) {
	run := core.RunID(uuid.NewString())

	err := e.Executions.Create(ctx, req.ShardID, req.WorkflowID, run, req.WorkflowType)
	switch {
	case errors.Is(err, core.ErrWorkflowAlreadyOpen):
		// A duplicate start is success, not failure: the executions primary key
		// deliberately excludes run_id so at most one run is open per business
		// key. Return the run that already exists.
		ex, gerr := e.Executions.Get(ctx, req.ShardID, req.WorkflowID)
		if gerr != nil {
			return api.StartResult{}, gerr
		}
		return api.StartResult{RunID: ex.RunID, Status: ex.Status, AlreadyOpen: true}, nil
	case err != nil:
		return api.StartResult{}, err
	}

	tok, ok := e.TokenFor(req.ShardID)
	if !ok {
		lease, lerr := e.Leases.Get(ctx, req.ShardID)
		if lerr != nil {
			return api.StartResult{}, lerr
		}
		return api.StartResult{}, &core.NotOwnerError{ShardID: req.ShardID, CurrentOwner: lease.Owner}
	}

	now := e.now().Now()
	payload, _ := json.Marshal(map[string]any{
		"workflow_type": req.WorkflowType,
		"input":         req.Input,
	})
	d := core.Decision{
		ShardID:    req.ShardID,
		WorkflowID: req.WorkflowID,
		RunID:      run,
		Token:      tok,
		Events: []core.Event{{
			ShardID: req.ShardID, WorkflowID: req.WorkflowID, RunID: run,
			Seq: 1, Type: core.EvWorkflowStarted, Payload: payload, CreatedAt: now,
		}},
		NewTasks: []core.Task{{
			ShardID: req.ShardID, Type: core.TaskWorkflow,
			WorkflowID: req.WorkflowID, RunID: run,
			ScheduledEventSeq: 1, VisibleAt: now,
		}},
		Status:  core.StatusRunning,
		LastSeq: 1,
	}
	if err := e.Committer.CommitDecision(ctx, d); err != nil {
		return api.StartResult{}, err
	}
	return api.StartResult{RunID: run, Status: core.StatusRunning}, nil
}

// Signal appends the signal to history and wakes the workflow. Signals are
// durable BEFORE any workflow code runs, which is what lets one arrive before
// the workflow has reached its wait and still be delivered.
func (e *Engine) Signal(ctx context.Context, req api.SignalRequest) error {
	ex, err := e.Executions.Get(ctx, req.ShardID, req.WorkflowID)
	if err != nil {
		return err
	}
	if ex.Status.IsTerminal() {
		return fmt.Errorf("workflow %s is %s: %w", req.WorkflowID, ex.Status, core.ErrWorkflowNotFound)
	}
	tok, ok := e.TokenFor(req.ShardID)
	if !ok {
		return core.ErrLostOwnership
	}
	now := e.now().Now()
	payload, _ := json.Marshal(core.SignalReceived{Name: req.Name, Payload: req.Payload})
	seq := ex.LastSeq + 1
	return e.Committer.CommitDecision(ctx, core.Decision{
		ShardID: req.ShardID, WorkflowID: req.WorkflowID, RunID: ex.RunID, Token: tok,
		Events: []core.Event{{
			ShardID: req.ShardID, WorkflowID: req.WorkflowID, RunID: ex.RunID,
			Seq: seq, Type: core.EvSignalReceived, Payload: payload, CreatedAt: now,
		}},
		NewTasks: []core.Task{{
			ShardID: req.ShardID, Type: core.TaskWorkflow,
			WorkflowID: req.WorkflowID, RunID: ex.RunID,
			ScheduledEventSeq: seq, VisibleAt: now,
		}},
		Status: ex.Status, LastSeq: seq,
	})
}

// Cancel is delivered as a reserved signal so it flows through exactly the same
// durable path as any other external input, rather than being a second
// mechanism that also has to be crash-safe.
func (e *Engine) Cancel(ctx context.Context, req api.CancelRequest) error {
	reason, _ := json.Marshal(map[string]string{"reason": req.Reason})
	return e.Signal(ctx, api.SignalRequest{
		ShardID: req.ShardID, WorkflowID: req.WorkflowID,
		Name: "__cancel", Payload: reason, IdempotencyKey: req.IdempotencyKey,
	})
}
