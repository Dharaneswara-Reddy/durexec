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
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/api"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/faultinj"
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

	// TaskLock is how long one poll holds a task, i.e. the queue's own
	// LockDuration. The activity worker needs it because it has to renew the
	// claim on a task it is still working — see keepClaimed — and the renewal
	// interval must be derived from this rather than guessed, or a deployment
	// that shortens TASK_LOCK silently reintroduces concurrent execution.
	// Zero takes the queue package's default.
	TaskLock time.Duration

	// TokenFor reports the fencing token this node currently holds for a
	// shard. It is wired to lease.Manager.Token. Every Decision carries the
	// token, and the commit re-verifies it FOR UPDATE, so a stale answer here
	// costs a rejected transaction rather than a corrupted log.
	TokenFor func(core.ShardID) (core.FencingToken, bool)

	// Faults is the deliberate-failure switch. It may be nil — a nil
	// *faultinj.Injector is a valid disabled injector — and it can never fire
	// unless configuration switched it on, which config.Validate refuses to do
	// implicitly.
	//
	// The crash points it serves sit at real transaction boundaries, and each
	// one documents at its call site exactly what has become durable there.
	// They exist because the engine's recovery tests used to kill whole nodes,
	// which only ever lands between transactions; the defect that cost 19
	// workflows lived INSIDE one.
	Faults *faultinj.Injector
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

// lockDuration reports how long a polled task stays claimed, defaulted the same
// way the queue defaults it, so the two cannot disagree.
func (e *Engine) lockDuration() time.Duration {
	if e.TaskLock > 0 {
		return e.TaskLock
	}
	return taskq.DefaultConfig().LockDuration
}

// codeVersion reports the workflow code version this node declares, taken from
// the dispatcher so there is one source of truth for it: the value recorded in
// a start event and the value AcceptVersion compares against must be the same
// string, or a node would refuse runs it started itself.
func (e *Engine) codeVersion() string {
	if e.Dispatcher == nil {
		return ""
	}
	return e.Dispatcher.CodeVersion
}

// ShardFor satisfies api.ShardResolver.
func (e *Engine) ShardFor(workflowID string) core.ShardID {
	return shard.ShardFor(workflowID, e.ShardCount)
}

// StartWorkflow opens the execution row, writes the first event and enqueues
// the first workflow task in ONE Decision, so a start either fully happens or
// does not happen at all.
//
// It used to make that claim while doing it in two transactions, and the gap
// between them was a silent, permanent loss of accepted work. The order below
// is what makes the claim true, and each step is where it is for a reason:
//
//  1. Check ownership BEFORE writing anything. A node that does not hold the
//     shard tells the caller so and leaves the database untouched, rather than
//     inserting a row and then discovering it may not write the rest. The
//     check is advisory — the commit re-verifies the token FOR UPDATE — but
//     doing it here means the ordinary not-the-owner case costs no writes.
//  2. One CommitDecision carrying CreateExecution, the start event and the
//     first workflow task. The executions primary key decides concurrent
//     starts of the same business key inside that transaction, so the loser
//     rolls back whole: no row, no event, no task, nothing to abandon.
//
// Three outcomes a caller has to distinguish, and how each is reported:
//
//   - Success: the run is durable and a worker will pick it up.
//   - Already open: some other request won the business key. Reported as
//     success with AlreadyOpen set, because the engine's contract is one open
//     run per key — but only after checking the existing row really did start
//     (see startedRun), so a pre-fix orphan can never be answered this way.
//   - Ambiguous: the commit failed and may or may not have landed. Reported as
//     an error, never as success. A retry resolves it: the business key is
//     either taken, which means the commit did land, or free, which means it
//     did not. That is safe only because the whole start is one transaction.
func (e *Engine) StartWorkflow(ctx context.Context, req api.StartRequest) (api.StartResult, error) {
	tok, ok := e.TokenFor(req.ShardID)
	if !ok {
		lease, lerr := e.Leases.Get(ctx, req.ShardID)
		if lerr != nil {
			return api.StartResult{}, lerr
		}
		return api.StartResult{}, &core.NotOwnerError{ShardID: req.ShardID, CurrentOwner: lease.Owner}
	}

	run := core.RunID(uuid.NewString())
	now := e.now().Now()
	// code_version is stamped into the start event so a run carries the version
	// of the code that began it for the rest of its life. Recording it in
	// history rather than beside it is the point: a run that has outlived three
	// deployments can still say which code wrote its decisions, and
	// Dispatcher.AcceptVersion is what reads it back.
	payload, _ := json.Marshal(map[string]any{
		"workflow_type": req.WorkflowType,
		"input":         req.Input,
		"code_version":  e.codeVersion(),
	})
	d := core.Decision{
		ShardID:         req.ShardID,
		WorkflowID:      req.WorkflowID,
		RunID:           run,
		Token:           tok,
		CreateExecution: &core.NewExecution{WorkflowType: req.WorkflowType},
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

	err := e.Committer.CommitDecision(ctx, d)
	switch {
	case err == nil:
		return api.StartResult{RunID: run, Status: core.StatusRunning}, nil
	case errors.Is(err, core.ErrWorkflowAlreadyOpen):
		return e.startedRun(ctx, req)
	default:
		return api.StartResult{}, err
	}
}

// startedRun answers a start request whose business key was already taken.
//
// The read exists only to report the winner's run id and status, but it carries
// one load-bearing check: an execution row with last_seq 0 never started. The
// current engine cannot produce such a row — the row and the start event at
// seq 1 become durable in the same transaction — so it can only be residue
// from the two-transaction start path, and nineteen of them are sitting on the
// demonstration cluster. Reporting one as "already open, RUNNING" is precisely
// how they stayed invisible: the client was told its work had been accepted and
// kept being told so for two days. So that case is an explicit error instead,
// and the reconciler is what clears the row.
func (e *Engine) startedRun(ctx context.Context, req api.StartRequest) (api.StartResult, error) {
	ex, err := e.Executions.Get(ctx, req.ShardID, req.WorkflowID)
	if err != nil {
		return api.StartResult{}, err
	}
	if !ex.Status.IsTerminal() && ex.LastSeq == 0 {
		return api.StartResult{}, fmt.Errorf("workflow %s: %w", req.WorkflowID, core.ErrWorkflowOrphaned)
	}
	return api.StartResult{RunID: ex.RunID, Status: ex.Status, AlreadyOpen: true}, nil
}

// Signal appends the signal to history and wakes the workflow. Signals are
// durable BEFORE any workflow code runs, which is what lets one arrive before
// the workflow has reached its wait and still be delivered.
func (e *Engine) Signal(ctx context.Context, req api.SignalRequest) error {
	ex, err := e.Executions.Get(ctx, req.ShardID, req.WorkflowID)
	if err != nil {
		return err
	}

	// The one exception to "a terminal run accepts nothing". A rollback that
	// ran out of retries leaves real-world state known to be inconsistent and
	// compensations still owed, and until this existed there was no engine API
	// that could reach such a run at all: Signal refused every terminal status,
	// Cancel went through Signal, and a fresh start was answered "already
	// open". Recovery meant hand-written SQL against the append-only table.
	resume := ex.Status == core.StatusCompensatingFailed &&
		req.Name == core.SignalResumeCompensation
	if ex.Status.IsTerminal() && !resume {
		return fmt.Errorf("workflow %s is %s: %w", req.WorkflowID, ex.Status, core.ErrWorkflowNotFound)
	}

	tok, ok := e.TokenFor(req.ShardID)
	if !ok {
		return core.ErrLostOwnership
	}
	now := e.now().Now()

	// Deriving the sequence number from history rather than from the status
	// pointer. The pointer is only a cache of the log, and a stale one used to
	// make every subsequent signal collide on the events primary key, which
	// stranded any workflow parked on a signal wait permanently. The pointer is
	// now monotone in the commit, so this is belt and braces — and it is the
	// right source either way.
	seq, err := e.Events.LastSeq(ctx, req.ShardID, req.WorkflowID, ex.RunID)
	if err != nil {
		return err
	}
	if ex.LastSeq > seq {
		seq = ex.LastSeq
	}

	events := []core.Event{}
	status := ex.Status
	if resume {
		// Two events, in this order and in one transaction: the human's
		// decision as a fact in the log, then the signal itself so a workflow
		// that wants to observe the resume can. Indexing history treats the
		// first as clearing the terminal state, so replay will run the workflow
		// again, its compensation stack will be rebuilt from history, and it
		// will resume at the step that failed — which is exactly what the saga
		// already does on every replay. Nothing is rewritten.
		seq++
		events = append(events, core.Event{
			ShardID: req.ShardID, WorkflowID: req.WorkflowID, RunID: ex.RunID,
			Seq: seq, Type: core.EvCompensationResumed, Payload: req.Payload, CreatedAt: now,
		})
		// COMPENSATING at last means something. It was declared and never
		// assigned anywhere in the repository, so a workflow that was actively
		// rolling back reported RUNNING and an operator could not see a
		// rollback in progress at all. A resumed rollback is now visibly a
		// rollback.
		status = core.StatusCompensating
	}

	payload, _ := json.Marshal(core.SignalReceived{Name: req.Name, Payload: req.Payload})
	seq++
	events = append(events, core.Event{
		ShardID: req.ShardID, WorkflowID: req.WorkflowID, RunID: ex.RunID,
		Seq: seq, Type: core.EvSignalReceived, Payload: payload, CreatedAt: now,
	})

	return e.Committer.CommitDecision(ctx, core.Decision{
		ShardID: req.ShardID, WorkflowID: req.WorkflowID, RunID: ex.RunID, Token: tok,
		Events: events,
		NewTasks: []core.Task{{
			ShardID: req.ShardID, Type: core.TaskWorkflow,
			WorkflowID: req.WorkflowID, RunID: ex.RunID,
			ScheduledEventSeq: seq, VisibleAt: now,
		}},
		Status: status, LastSeq: seq,
	})
}

// ResumeCompensation is the operator action that reopens a half-rolled-back
// saga. It is a thin wrapper over Signal so the recovery travels the same
// durable path as every other external input rather than being a second
// mechanism that would also have to be made crash-safe.
//
// It is NOT automatic and must not become so. A failed compensation means the
// engine already tried and the world is in a state it could not undo; deciding
// to try again is a judgement about the world, not about the log.
func (e *Engine) ResumeCompensation(ctx context.Context, shard core.ShardID, workflowID string, reason json.RawMessage) error {
	if len(reason) == 0 {
		reason = json.RawMessage("null")
	}
	return e.Signal(ctx, api.SignalRequest{
		ShardID:    shard,
		WorkflowID: workflowID,
		Name:       core.SignalResumeCompensation,
		Payload:    reason,
	})
}

// Cancel is delivered as a reserved signal so it flows through exactly the same
// durable path as any other external input, rather than being a second
// mechanism that also has to be crash-safe.
func (e *Engine) Cancel(ctx context.Context, req api.CancelRequest) error {
	reason, _ := json.Marshal(map[string]string{"reason": req.Reason})
	return e.Signal(ctx, api.SignalRequest{
		ShardID: req.ShardID, WorkflowID: req.WorkflowID,
		Name: core.SignalCancel, Payload: reason, IdempotencyKey: req.IdempotencyKey,
	})
}
