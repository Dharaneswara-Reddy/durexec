// Package saga implements the rollback half of a long-running workflow.
//
// A workflow that spends three days moving money cannot hold a database
// transaction open for three days, so it cannot roll back by aborting one.
// Instead it does the work in steps, and for every step that succeeded it
// remembers how to undo it. If a later step fails, the remembered undo actions
// are run in reverse order. That is a saga: not a transaction, but a
// compensating sequence that returns the system to a *business*-consistent
// state.
//
// The word "compensating" is doing real work there. A compensation is not an
// undo in the database sense — the charge really happened, and the customer's
// statement will show both the charge and the refund. It is a second, opposite
// action that leaves the world in an acceptable state, and that is the strongest
// guarantee available once an effect has escaped into somebody else's system.
//
// # Why the stack is not journaled
//
// The stack lives in memory, is never written to the log, and is rebuilt from
// scratch every time the workflow is re-executed. That sounds fragile and is
// in fact the opposite.
//
// Workflow code is deterministic and re-executed from line one on every
// workflow task. A compensation is registered on the line immediately after a
// forward step succeeded, and whether that step succeeded is already recorded
// in history. So on replay the workflow walks exactly the same path, sees
// exactly the same successes, and pushes exactly the same compensations in
// exactly the same order. The stack is a pure function of history.
//
// Journaling it would therefore write down something replay already
// reconstructs perfectly — and would introduce a way for the two to disagree,
// which is a bug that could not otherwise exist. It would also cost an event
// per step, on the hot path, for no information.
//
// # Why registration happens after the step, never before
//
// AddCompensation must be called only once the forward step has actually
// returned successfully. Registering it beforehand would mean that a step which
// failed — or which crashed before it ever ran — still has an undo action on the
// stack, and the rollback would refund money that was never taken, or release an
// inventory hold that was never placed. The rule is simple: nothing goes on the
// stack that has not already happened.
package saga

import (
	"encoding/json"
	"errors"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Runner is the one capability a saga needs: the ability to run an activity and
// wait for its result.
//
// It is a local interface, not an import of the activity or workflow package,
// for two reasons. It keeps this package free of any dependency but core, so it
// can be tested against a fake in a few lines. And it makes the contract
// explicit: a saga does not run compensations itself, it asks the engine to run
// them. That distinction is the whole reliability story — see Compensate.
type Runner interface {
	Run(activityType string, input any) (json.RawMessage, error)
}

// RunnerFunc adapts a plain function to Runner. A workflow.Context is wired in
// with one line:
//
//	s := saga.New(workflowID, saga.RunnerFunc(workflow.SagaRunner(ctx)))
type RunnerFunc func(activityType string, input any) (json.RawMessage, error)

func (f RunnerFunc) Run(activityType string, input any) (json.RawMessage, error) {
	return f(activityType, input)
}

// Compensation is one remembered undo action.
type Compensation struct {
	ActivityType string
	Input        any
}

// Saga is a last-in-first-out stack of compensations.
//
// LIFO is not a stylistic choice. Later steps often depend on earlier ones — you
// cannot release the inventory you reserved before you have cancelled the order
// that reserved it — so undoing has to unwind in the reverse of the order that
// built it up, exactly as a call stack does.
//
// A Saga is used only from workflow code, which the engine guarantees runs on
// one goroutine at a time, so it needs no locking.
type Saga struct {
	// workflowID is carried purely so a failure can name the run that needs
	// human attention.
	workflowID string
	runner     Runner
	stack      []Compensation
}

func New(workflowID string, r Runner) *Saga {
	return &Saga{workflowID: workflowID, runner: r}
}

// AddCompensation pushes an undo action.
//
// Call it only AFTER the forward step has returned successfully. See the
// package comment: registering before the step means the rollback would undo
// something that never happened.
func (s *Saga) AddCompensation(activityType string, input any) {
	s.stack = append(s.stack, Compensation{ActivityType: activityType, Input: input})
}

// Len is how many compensations are outstanding.
func (s *Saga) Len() int { return len(s.stack) }

// Pending returns the outstanding compensations in the order they would run,
// top of the stack first. It returns a copy, so a caller inspecting the saga
// cannot corrupt it. Used by tests and by the admin view of a stuck rollback.
func (s *Saga) Pending() []Compensation {
	out := make([]Compensation, 0, len(s.stack))
	for i := len(s.stack) - 1; i >= 0; i-- {
		out = append(out, s.stack[i])
	}
	return out
}

// Compensate unwinds the saga.
//
// Each compensation is run as an ordinary activity through the Runner, which
// means it goes through the engine exactly as a forward step does: it is
// journaled before it runs and after it finishes, it is retried under its own
// retry policy, and if the worker dies halfway through the rollback, the next
// worker replays the workflow, rebuilds this identical stack, sees from history
// which compensations already completed, and carries on from there. A rollback
// written as a Go defer would have none of those properties — a crash would
// simply lose it, and there would be no record that it was ever owed.
//
// A compensation is popped only after it has succeeded. If one fails, the stack
// is left holding it and everything beneath it, so calling Compensate again
// resumes at the same point rather than starting over.
//
// Compensate stops at the first failure instead of pushing on through the rest.
// The compensations below the failed one undo earlier steps, and those earlier
// steps are the ones the failed step's own work was built on; running them while
// the layer above is still standing can leave the system in a state that is
// worse than the one we were trying to escape. Stopping keeps the damage
// bounded and legible for whoever has to look at it.
func (s *Saga) Compensate() error {
	for len(s.stack) > 0 {
		top := s.stack[len(s.stack)-1]

		if s.runner == nil {
			return &core.CompensationFailedError{
				WorkflowID: s.workflowID,
				Activity:   top.ActivityType,
				Attempts:   0,
				Cause:      errNoRunner,
			}
		}

		if _, err := s.runner.Run(top.ActivityType, top.Input); err != nil {
			// There is no automatic resolution from here, and pretending
			// otherwise would be the most dangerous thing this engine could
			// do. The run ends in core.StatusCompensatingFailed, which is a
			// terminal status distinct from an ordinary failure precisely so
			// that it can be alerted on: it means real-world state is known to
			// be inconsistent and a person has to decide what to do about it.
			return &core.CompensationFailedError{
				WorkflowID: s.workflowID,
				Activity:   top.ActivityType,
				Attempts:   attemptsOf(err),
				Cause:      err,
			}
		}

		s.stack = s.stack[:len(s.stack)-1]
	}
	return nil
}

// Cleanup discards the stack. Call it on the success path, once the workflow
// has committed to its outcome and nothing can ask for a rollback any more.
//
// It exists to state that intention in the code rather than to reclaim memory:
// the stack dies with the goroutine at the end of every workflow task anyway. A
// workflow that reaches its own return without calling Cleanup is not broken,
// it is just less clear about when its work became final.
func (s *Saga) Cleanup() { s.stack = nil }

var errNoRunner = errors.New("saga has compensations but no runner to execute them")

// attemptCounter is how a saga learns how many tries a compensation actually
// got without importing the activity package. activity.ActivityFailedError
// implements it.
type attemptCounter interface{ AttemptCount() int }

func attemptsOf(err error) int {
	var ac attemptCounter
	if errors.As(err, &ac) {
		return ac.AttemptCount()
	}
	// The runner told us nothing about retries, so all we can honestly report
	// is that it was tried.
	return 1
}
