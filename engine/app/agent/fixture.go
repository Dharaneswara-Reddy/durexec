package agent

import (
	"github.com/dharaneswar339/durexec/internal/faultinj"
)

// The double-booking scenario.
//
// This is the failure the whole project is about, written down once so the
// evaluation, the unit tests and the live demonstration all drive the same
// thing rather than three slightly different things.
//
// What happens, in order:
//
//  1. A user asks for a flight. The planner decides to book it and phrases the
//     booking as "BLR to DEL Friday morning".
//  2. BookFlight runs. A seat is really taken. The result is written to the
//     event log, so it is now a durable fact.
//  3. The process is killed while the *next* planning call is still in flight.
//     That call was never journaled, so it simply did not happen.
//  4. Another node picks up the shard and replays the history. It gets the
//     first plan and the booking result back from the log, exactly as before.
//     Then it has to make the next plan for real, because that one was lost.
//  5. The model is asked the same question again and answers with the same
//     intent in different words: "Bangalore to Delhi, Friday AM". It reissues
//     the booking.
//
// An exact idempotency key is a hash of the argument bytes. "BLR to DEL Friday
// morning" and "Bangalore to Delhi, Friday AM" hash to different values, so
// the engine sees a brand new request and books a second seat. The user is
// charged twice for a flight they asked for once.
//
// A semantic key compares what the two requests mean. They mean the same
// thing, so the engine can recognise the repeat and reuse the first booking.
// That is the contribution, and this fixture is the case it has to pass.
const (
	// DoubleBookingGoal is what the user actually asked for.
	DoubleBookingGoal = "Book me a flight from Bangalore to Delhi on Friday morning"

	// IntentBeforeCrash is the phrasing the planner used before the crash.
	IntentBeforeCrash = "BLR to DEL Friday morning"

	// IntentAfterCrash is the phrasing the recovered planner used. Same
	// meaning, different bytes.
	IntentAfterCrash = "Bangalore to Delhi, Friday AM"
)

// DoubleBooking is the reusable fixture. Build it with NewDoubleBooking.
type DoubleBooking struct {
	// Goal is the user's request.
	Goal string

	// BeforeCrash drives the first incarnation of the process.
	BeforeCrash *ScriptedPlanner

	// AfterCrash drives the recovered incarnation. It is a separate planner
	// because a real recovered process starts with an empty model context: it
	// has the transcript from the log, but none of the first process's state.
	AfterCrash *ScriptedPlanner

	// CrashPoint is where the injector must be armed to reproduce step 3.
	CrashPoint faultinj.CrashPoint

	// CrashAfterLivePlanCalls is how many planning calls must complete in the
	// first incarnation before the crash fires. One: the plan that issues the
	// booking completes, and the plan after it does not.
	CrashAfterLivePlanCalls int
}

// NewDoubleBooking returns a fresh fixture. Call it per run; the planners hold
// a cursor into their script, so sharing one across runs would replay from
// wherever the last run stopped.
func NewDoubleBooking() *DoubleBooking {
	return &DoubleBooking{
		Goal: DoubleBookingGoal,

		// The first incarnation gets exactly one plan. Its second planning
		// call is the one the crash interrupts, so the script ends there.
		BeforeCrash: NewScriptedPlanner("before-crash",
			Plan{
				Thought: "The user wants the Friday morning Bangalore to Delhi flight. Book it.",
				Tool:    ActivityBookFlight,
				Args:    IntentBeforeCrash,
			},
		),

		// The recovered incarnation replays the plan and the booking above
		// from the event log without calling the planner at all. The planner
		// is asked only for the step that was lost, and it phrases the same
		// intent differently.
		AfterCrash: NewScriptedPlanner("after-crash",
			Plan{
				Thought: "I still need to book the Friday morning flight to Delhi.",
				Tool:    ActivityBookFlight,
				Args:    IntentAfterCrash,
			},
			Plan{
				Thought: "The flight is booked. Nothing left to do.",
				Done:    true,
				Answer:  "Booked the Friday morning Bangalore to Delhi flight.",
			},
		),

		CrashPoint:              faultinj.BeforeNextPlanCompletes,
		CrashAfterLivePlanCalls: 1,
	}
}

// Request is the workflow input that drives the fixture.
func (f *DoubleBooking) Request(requestID string) Request {
	return Request{
		RequestID:     requestID,
		UserID:        "demo-user",
		Goal:          f.Goal,
		MaxIterations: 3,
	}
}

// Planner returns the planner for a given process incarnation, counting from
// zero. Anything past the second incarnation reuses the recovered planner,
// because by then the booking is done and the script only has to finish.
func (f *DoubleBooking) Planner(incarnation int) *ScriptedPlanner {
	if incarnation <= 0 {
		return f.BeforeCrash
	}
	return f.AfterCrash
}

// Arm tells the fault injector to kill the first incarnation at the right
// moment. It is a no-op on an injector that configuration never enabled, which
// is what keeps this fixture harmless if it is ever linked into production.
func (f *DoubleBooking) Arm(inj *faultinj.Injector) {
	inj.CrashAt(f.CrashPoint)
}

// ExpectedCalls returns the two tool calls the scenario must produce: the one
// made before the crash and the one made after recovery. A harness that drives
// the real engine can assert against these without re-deriving them.
func (f *DoubleBooking) ExpectedCalls() (before, after ToolCall) {
	return ToolCall{Tool: ActivityBookFlight, Args: IntentBeforeCrash},
		ToolCall{Tool: ActivityBookFlight, Args: IntentAfterCrash}
}

// Trace is the observable outcome of running the fixture.
type Trace struct {
	// Before is the tool call the first incarnation made and journaled.
	Before ToolCall
	// After is the tool call the recovered incarnation made.
	After ToolCall
	// Result is the recovered run's output.
	Result Result
	// ReplayedCalls is how many activities the recovered run took from the
	// event log instead of executing. It proves recovery really did replay.
	ReplayedCalls int
	// LiveCalls is how many activities the recovered run actually executed.
	LiveCalls int
}

// ArgsAreByteEqual reports whether an exact idempotency key would treat the two
// bookings as the same request. It must be false: that is the bug.
func (t Trace) ArgsAreByteEqual() bool { return t.Before.Args == t.After.Args }

// ArgsAreCanonicallyEqual reports whether cheap normalisation — trimming and
// lowercasing — is enough to catch the repeat. It must also be false, or the
// scenario would not need an embedding at all.
func (t Trace) ArgsAreCanonicallyEqual() bool {
	return CanonicalIntent(t.Before.Args) == CanonicalIntent(t.After.Args)
}

// SameTool reports whether both calls hit the same tool, which they must: a
// repeat of a different tool would be a different problem.
func (t Trace) SameTool() bool { return t.Before.Tool == t.After.Tool }
