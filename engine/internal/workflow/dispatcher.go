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

	// CodeVersion identifies the workflow code in this binary. StartWorkflow
	// records it in the start event, so every run carries the version that
	// began it for the rest of its life.
	//
	// It is a free-form string — a git sha, a build number, a semantic version
	// — because the engine never interprets it. Only AcceptVersion does, and
	// that is the deployer's policy, not the engine's. Leaving it empty means
	// "unversioned", which is what every run written before this existed is.
	CodeVersion string

	// AcceptVersion decides whether this binary is willing to replay a run that
	// was started by the given recorded version. It is called once per workflow
	// task, before any workflow code runs.
	//
	// Nil means ACCEPT EVERYTHING, which is deliberately the default: turning
	// version checking on by itself would park every run in flight at the
	// moment of deployment, and that is a decision an operator has to take
	// knowingly. It MUST be a pure function — it runs on the replay path — and
	// it must accept "" if there are unversioned runs still in flight.
	//
	// A rejection is not a failure. See core.VersionIncompatibleError: the run
	// is left exactly as it was so that a compatible node, or a redeployment of
	// the old code, can carry it on. What this mechanism does NOT provide is a
	// way for new code to take a DIFFERENT path for an old run — the
	// ctx.Version(changeID, min, max) style of patch API that would need the
	// chosen branch journaled at its own k. Without it, an incompatible change
	// is a stall that needs an operator, not something the engine rides out,
	// and that limitation is stated rather than hidden.
	AcceptVersion func(recorded string) bool
}

// acceptsVersion reports whether this dispatcher will replay a run recorded at
// the given version.
func (d *Dispatcher) acceptsVersion(recorded string) bool {
	if d.AcceptVersion == nil {
		return true
	}
	return d.AcceptVersion(recorded)
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
	//
	// This is checked BEFORE the version gate on purpose: clearing the task row
	// of a finished run needs no workflow code to run, so refusing it over a
	// version mismatch would leave rows behind for no benefit.
	if h.terminal != "" {
		return Result{
			Status: h.terminal,
			Output: h.output,
			Err:    h.failure,
			Now:    h.last,
		}, nil
	}

	// The version gate. Refusing here, before the workflow function is even
	// looked up, is what keeps history untouched: no events, no commands, and
	// the caller is expected to put the task back.
	if !d.acceptsVersion(h.codeVersion) {
		return Result{}, &core.VersionIncompatibleError{
			Recorded: h.codeVersion,
			Running:  d.CodeVersion,
		}
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
