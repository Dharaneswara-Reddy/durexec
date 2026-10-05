package saga

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

const testWFID = "order-42"

// fakeRunner records every compensation it is asked to run, in order, and can
// be told to fail on a chosen one. Nothing here touches a database or a queue:
// the saga's job is to decide what to run and in what order, and that decision
// is what is being tested.
type fakeRunner struct {
	calls  []string
	failOn string
	err    error
}

func (f *fakeRunner) Run(activityType string, input any) (json.RawMessage, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s(%v)", activityType, input))
	if activityType == f.failOn {
		return nil, f.err
	}
	return json.RawMessage("null"), nil
}

// retriedError mimics activity.ActivityFailedError, which reports how many
// attempts a compensation actually got. The saga reads it through a one-method
// interface so it never has to import the activity package.
type retriedError struct {
	attempts int
	cause    error
}

func (e *retriedError) Error() string     { return e.cause.Error() }
func (e *retriedError) Unwrap() error     { return e.cause }
func (e *retriedError) AttemptCount() int { return e.attempts }

// TestCompensateRunsInStrictLIFOOrder is the core of the package. Later steps
// depend on earlier ones — you cannot release the inventory you reserved before
// cancelling the order that reserved it — so undoing has to unwind in exactly
// the reverse of the order that built it up.
func TestCompensateRunsInStrictLIFOOrder(t *testing.T) {
	r := &fakeRunner{}
	s := New(testWFID, r)

	// Each of these is registered immediately AFTER its forward step succeeded.
	s.AddCompensation("ReleaseInventory", "hold-1")
	s.AddCompensation("RefundPayment", "txn-2")
	s.AddCompensation("CancelShipment", "ship-3")

	if err := s.Compensate(); err != nil {
		t.Fatalf("Compensate: %v", err)
	}

	want := []string{
		"CancelShipment(ship-3)",
		"RefundPayment(txn-2)",
		"ReleaseInventory(hold-1)",
	}
	assertCalls(t, r.calls, want)

	// A fully unwound saga owes nothing further.
	if s.Len() != 0 {
		t.Fatalf("stack still holds %d compensations", s.Len())
	}
}

// TestEmptySagaIsANoOp: a workflow that fails before its first forward step
// succeeded has nothing to undo, and must not invent something.
func TestEmptySagaIsANoOp(t *testing.T) {
	r := &fakeRunner{}
	s := New(testWFID, r)

	if err := s.Compensate(); err != nil {
		t.Fatalf("an empty saga must succeed silently, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("an empty saga ran %v", r.calls)
	}

	// And with no runner at all, which is the shape of a saga built before the
	// workflow context was wired in.
	if err := New(testWFID, nil).Compensate(); err != nil {
		t.Fatalf("an empty saga with no runner must still be a no-op, got %v", err)
	}
}

// TestFailingCompensationSurfacesForAHuman: there is no automatic resolution
// once a rollback itself fails. Real-world state is known to be inconsistent,
// and pretending otherwise would be the most dangerous thing the engine could
// do.
func TestFailingCompensationSurfacesForAHuman(t *testing.T) {
	cause := errors.New("payment gateway unreachable")
	r := &fakeRunner{failOn: "RefundPayment", err: &retriedError{attempts: 5, cause: cause}}
	s := New(testWFID, r)

	s.AddCompensation("ReleaseInventory", "hold-1")
	s.AddCompensation("RefundPayment", "txn-2")
	s.AddCompensation("CancelShipment", "ship-3")

	err := s.Compensate()

	var cf *core.CompensationFailedError
	if !errors.As(err, &cf) {
		t.Fatalf("expected *core.CompensationFailedError, got %v", err)
	}
	if cf.Activity != "RefundPayment" {
		t.Fatalf("Activity = %q, want the compensation that actually failed", cf.Activity)
	}
	if cf.WorkflowID != testWFID {
		t.Fatalf("WorkflowID = %q", cf.WorkflowID)
	}
	// The attempt count comes from the runner's own error, so an operator can
	// see the retry budget really was spent.
	if cf.Attempts != 5 {
		t.Fatalf("Attempts = %d, want 5", cf.Attempts)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("the original cause must survive unwrapping: %v", err)
	}

	// Unwinding stopped at the failure rather than pushing on through the
	// compensations beneath it: those undo earlier steps that the failed one's
	// work was built on, and running them now could leave the system worse off.
	assertCalls(t, r.calls, []string{
		"CancelShipment(ship-3)",
		"RefundPayment(txn-2)",
	})
}

// TestCompensateResumesWhereItStopped: a compensation is popped only once it
// has succeeded, so a second call carries on from the same point rather than
// re-running the ones that already worked.
func TestCompensateResumesWhereItStopped(t *testing.T) {
	r := &fakeRunner{failOn: "RefundPayment", err: errors.New("gateway down")}
	s := New(testWFID, r)
	s.AddCompensation("ReleaseInventory", "hold-1")
	s.AddCompensation("RefundPayment", "txn-2")
	s.AddCompensation("CancelShipment", "ship-3")

	if err := s.Compensate(); err == nil {
		t.Fatal("expected the first pass to fail")
	}
	// CancelShipment succeeded and is gone; the failed one and the one beneath
	// it are still owed.
	if s.Len() != 2 {
		t.Fatalf("stack holds %d, want 2", s.Len())
	}
	if p := s.Pending(); p[0].ActivityType != "RefundPayment" {
		t.Fatalf("next compensation is %q, want the one that failed", p[0].ActivityType)
	}

	// The gateway comes back and an operator retries.
	r.failOn = ""
	r.calls = nil
	if err := s.Compensate(); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	assertCalls(t, r.calls, []string{
		"RefundPayment(txn-2)",
		"ReleaseInventory(hold-1)",
	})
	if s.Len() != 0 {
		t.Fatalf("stack still holds %d", s.Len())
	}
}

// TestAttemptsDefaultToOne: a runner that reports nothing about retries still
// produces an honest number.
func TestAttemptsDefaultToOne(t *testing.T) {
	r := &fakeRunner{failOn: "RefundPayment", err: errors.New("plain failure")}
	s := New(testWFID, r)
	s.AddCompensation("RefundPayment", "txn-2")

	var cf *core.CompensationFailedError
	if !errors.As(s.Compensate(), &cf) {
		t.Fatal("expected *core.CompensationFailedError")
	}
	if cf.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", cf.Attempts)
	}
}

// TestMissingRunnerIsReportedNotIgnored: a saga that owes compensations and has
// no way to run them is a bug, and silently succeeding would hide real money
// that was never refunded.
func TestMissingRunnerIsReportedNotIgnored(t *testing.T) {
	s := New(testWFID, nil)
	s.AddCompensation("RefundPayment", "txn-2")

	var cf *core.CompensationFailedError
	if !errors.As(s.Compensate(), &cf) {
		t.Fatal("expected *core.CompensationFailedError")
	}
	if cf.Activity != "RefundPayment" {
		t.Fatalf("Activity = %q", cf.Activity)
	}
}

// TestCleanupDiscardsTheStack: called on the success path, once the workflow
// has committed to its outcome and nothing can ask for a rollback any more.
func TestCleanupDiscardsTheStack(t *testing.T) {
	r := &fakeRunner{}
	s := New(testWFID, r)
	s.AddCompensation("ReleaseInventory", "hold-1")
	s.AddCompensation("RefundPayment", "txn-2")

	s.Cleanup()

	if s.Len() != 0 {
		t.Fatalf("stack holds %d after Cleanup", s.Len())
	}
	if err := s.Compensate(); err != nil {
		t.Fatalf("Compensate after Cleanup: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("Cleanup did not prevent %v from running", r.calls)
	}
}

// TestPendingIsACopy: an operator inspecting a stuck rollback must not be able
// to corrupt it.
func TestPendingIsACopy(t *testing.T) {
	s := New(testWFID, &fakeRunner{})
	s.AddCompensation("A", 1)
	s.AddCompensation("B", 2)

	p := s.Pending()
	if len(p) != 2 || p[0].ActivityType != "B" || p[1].ActivityType != "A" {
		t.Fatalf("Pending() = %+v, want top of stack first", p)
	}
	p[0].ActivityType = "mutated"
	if s.Pending()[0].ActivityType != "B" {
		t.Fatal("Pending returned a live view of the stack")
	}
}

// TestRunnerFuncAdapts: the one-line wiring the integrator uses to hand a
// workflow.Context to a saga without either package importing the other.
func TestRunnerFuncAdapts(t *testing.T) {
	var got string
	s := New(testWFID, RunnerFunc(func(activityType string, input any) (json.RawMessage, error) {
		got = activityType
		return json.RawMessage(`"done"`), nil
	}))
	s.AddCompensation("RefundPayment", nil)

	if err := s.Compensate(); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if got != "RefundPayment" {
		t.Fatalf("runner saw %q", got)
	}
}

// TestStackIsRebuiltIdenticallyOnReplay is the argument for not journaling the
// stack. The stack is a pure function of which forward steps succeeded, and
// which forward steps succeeded is already in history, so re-executing the same
// deterministic workflow code rebuilds it exactly. Writing it down as well
// would add a second copy that could disagree with the first.
func TestStackIsRebuiltIdenticallyOnReplay(t *testing.T) {
	// buildFromHistory stands in for a workflow function: it registers a
	// compensation only for the steps history records as successful.
	buildFromHistory := func(succeeded []string) []Compensation {
		s := New(testWFID, &fakeRunner{})
		for _, step := range succeeded {
			switch step {
			case "ReserveInventory":
				s.AddCompensation("ReleaseInventory", "hold-1")
			case "ChargeCard":
				s.AddCompensation("RefundPayment", "txn-2")
			}
		}
		return s.Pending()
	}

	history := []string{"ReserveInventory", "ChargeCard"}
	first := buildFromHistory(history)
	for i := 0; i < 5; i++ {
		again := buildFromHistory(history)
		if len(again) != len(first) {
			t.Fatalf("replay %d produced %d compensations, want %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("replay %d differs at %d: %+v vs %+v", i, j, again[j], first[j])
			}
		}
	}

	// And a run that got one step further owes one more compensation, without
	// anything having been written down about the stack itself.
	if len(buildFromHistory([]string{"ReserveInventory"})) != 1 {
		t.Fatal("a shorter history must rebuild a shorter stack")
	}
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls =\n  %v\nwant\n  %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (full sequence %v)", i, got[i], want[i], got)
		}
	}
}
