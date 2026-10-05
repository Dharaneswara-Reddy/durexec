package workflow

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Result is everything one workflow task decided.
//
// It contains no identifiers and no timestamps of its own, because Execute is
// deliberately ignorant of which workflow it is running for. That ignorance is
// what makes the dispatcher testable with nothing but a slice of events, and it
// is BuildDecision's job to attach the identity afterwards.
type Result struct {
	// Commands are the intentions produced by this task, in program order.
	// They are not effects: nothing has happened until the committer turns
	// them into events inside a single transaction.
	Commands []core.Command

	// Status is what the run's status should become once those commands are
	// committed.
	Status core.ExecutionStatus

	// Output is the workflow's return value, set only when Status is COMPLETED.
	Output json.RawMessage

	// Err is the failure text, set only when the workflow returned an error.
	Err string

	// Now is the workflow's deterministic clock at the end of the task. Useful
	// for debugging a replay and for asserting on time in tests.
	Now time.Time

	// Blocking reports whether the task ended because the workflow suspended,
	// as opposed to returning. It is redundant with Status but reads better at
	// a call site than comparing against StatusRunning.
	Blocking bool
}

// Dispatcher runs workflow code against a history and collects its commands.
//
// One Dispatcher is shared by every worker goroutine in the process. It holds
// no per-run state — all of that lives in the wfContext created inside Execute
// and dies with it — so it is safe to use concurrently.
type Dispatcher struct {
	registry *Registry

	// ModeFor tells the dispatcher which idempotency mode to stamp on a
	// scheduled activity. It is optional; without it every activity is
	// core.ModeExact. Wire it to activity.Registry.Mode. It MUST be a pure
	// in-memory lookup: it runs on the replay path, so anything that could
	// return a different answer on a later run would break determinism.
	ModeFor func(activityType string) core.IdempotencyMode
}

func NewDispatcher(r *Registry) *Dispatcher { return &Dispatcher{registry: r} }

// Execute runs one workflow task.
//
// It re-executes the workflow function from its first line against the history
// given, and stops the moment the workflow can make no further progress. The
// two modes it moves between are the whole idea of the engine:
//
//   - REPLAY MODE. History already has an entry for the blocking call at this
//     k, so the answer is read out of the log. No command is emitted, because
//     the work was scheduled during some earlier task and doing it again would
//     duplicate it. This is how the workflow catches up to where it left off:
//     it re-runs code it has run before, but every external call is answered
//     from the log instead of from the world.
//
//   - NEW MODE. History has nothing at this k, so this is genuinely the first
//     time the workflow has reached this line. A command is emitted and an
//     unresolved future is returned; the first Get on such a future ends the
//     task.
//
// The transition between the two happens exactly once per task, at the point
// where history runs out, and needs no flag: it falls out of whether there is
// an entry at k.
//
// The only error Execute returns is one that means the run cannot continue at
// all — an unknown workflow type, corrupt history, or divergence. A workflow
// that fails is not an error here; it is a Result with StatusFailed, because a
// failed workflow is a perfectly normal outcome that must be journaled.
func (d *Dispatcher) Execute(history []core.Event, wfType string, input json.RawMessage) (Result, error) {
	fn, ok := d.registry.Get(wfType)
	if !ok {
		return Result{}, &NotRegisteredError{WorkflowType: wfType}
	}

	h, err := indexHistory(history)
	if err != nil {
		return Result{}, err
	}

	// A task for a run that has already finished is not a bug. A worker can
	// crash after committing the completion but before deleting its task row,
	// and the task then becomes visible again. Produce no commands and report
	// the status history already records.
	if h.terminal != "" {
		return Result{
			Status: h.terminal,
			Output: h.output,
			Err:    h.failure,
			Now:    h.last,
		}, nil
	}

	co := newCoroutine()
	// Always dispose of the goroutine, including on the divergence path. A
	// workflow that suspends is parked on a channel receive forever; without
	// this it would leak for the life of the process.
	defer co.destroy()

	ctx := newWFContext(co, h, d.ModeFor)
	co.start(func() ([]byte, error) {
		out, err := fn(ctx, input)
		return out, err
	})

	// The single handover. Control passes to the workflow and comes back when
	// it suspends or returns; see the long comment in coroutine.go.
	co.step()

	res := Result{Commands: ctx.commands, Now: ctx.now}

	if !co.finished {
		// The workflow suspended on an unresolved future. Everything it
		// decided before suspending is in res.Commands, and it will be
		// re-executed from line one when the answer has been journaled.
		res.Status = core.StatusRunning
		res.Blocking = true
		return res, nil
	}

	if co.err != nil {
		// Divergence is fatal to the task, not a workflow failure. Returning it
		// leaves history untouched, which is what lets an operator deploy the
		// old code back and resume the run.
		var nd *core.NonDeterminismError
		if errors.As(co.err, &nd) {
			return Result{}, nd
		}

		res.Status = core.StatusFailed
		// A rollback that could not be completed is a different, worse state
		// than an ordinary failure: money may have moved and not come back, so
		// it is flagged for a human rather than filed as just another failure.
		var cf *core.CompensationFailedError
		if errors.As(co.err, &cf) {
			res.Status = core.StatusCompensatingFailed
		}
		res.Err = co.err.Error()
		res.Commands = append(res.Commands, core.Command{
			Type: core.CmdFailWorkflow,
			K:    terminalK,
			Err:  res.Err,
		})
		return res, nil
	}

	out := json.RawMessage(co.result)
	if len(out) == 0 {
		out = jsonNull
	}
	res.Status = core.StatusCompleted
	res.Output = out
	res.Commands = append(res.Commands, core.Command{
		Type:   core.CmdCompleteWorkflow,
		K:      terminalK,
		Result: out,
	})
	return res, nil
}

// terminalK marks a command that is not a blocking call and therefore has no
// slot in the k sequence. -1 rather than 0 so it can never be mistaken for the
// first activity of the run.
const terminalK = -1
