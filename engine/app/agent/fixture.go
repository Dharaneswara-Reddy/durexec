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
	//
	// It is the Planner interface rather than *ScriptedPlanner so that the
	// same scenario can be driven by a real model. The scripted planner is
	// what the unit tests and the offline demo use, because they must be
	// deterministic; cmd/rephrase puts a live model on both sides, because the
	// project's claim is about wording the model chose and not wording we
	// wrote. One fixture, two drivers, no second copy of the scenario.
	BeforeCrash Planner

	// AfterCrash drives the recovered incarnation. It is a separate planner
	// because a real recovered process starts with an empty model context: it
	// has the transcript from the log, but none of the first process's state.
	AfterCrash Planner

	// CrashPoint is where the injector must be armed to reproduce step 3.
	CrashPoint faultinj.CrashPoint

	// CrashAfterLivePlanCalls is how many planning calls must complete in the
	// first incarnation before the crash fires. One: the plan that issues the
	// booking completes, and the plan after it does not.
	CrashAfterLivePlanCalls int

	// CrashAfterTool names the tool that must have executed for real before
	// the crash is allowed to fire, and whose repeat the scenario is about.
	// Empty means BookFlight.
	//
	// It exists because a plan-call count alone is not a crash point when a
	// live model is driving. The scripted planner books on its first plan, so
	// "crash on the second plan call" lands immediately after the booking. A
	// real model usually searches first and books second — which is correct
	// agent behaviour — so the same count would land the crash after the
	// SEARCH, and the step the crash destroyed would be one whose repeat costs
	// nothing. Requiring that the watched tool has actually run puts the crash
	// where the scenario needs it in both cases, with no need to tell the
	// model to skip searching: steering the model is exactly what this
	// experiment cannot do, because the wording has to be its own.
	CrashAfterTool string

	// Tools are extra activities both incarnations may call, handed straight
	// to Options.Tools. Empty leaves the scenario as the flight booking the
	// narrative above describes; a non-empty set makes it an action of some
	// other shape entirely, with the same crash and the same planner.
	Tools []Registration

	// MaxIterations bounds the plan/tool loop of both incarnations. Zero means
	// the workflow's own default.
	//
	// It is a knob because every planning call made by a live model costs a
	// request against a quota, and a measurement run of fifty trials has to be
	// able to say "two steps is all this scenario needs" rather than pay for
	// steps whose answers it will not read.
	MaxIterations int

	// LoseUncommittedJournal discards the crashed incarnation's journal
	// entirely, so the recovered incarnation starts from nothing.
	//
	// WHY THIS EXISTS, because it changes what the scenario measures.
	//
	// With it false — the default, and what the scripted fixture uses — the
	// recovered incarnation inherits the plan AND the booking result, replays
	// both, and is asked only for the step that was in flight. That is the
	// narrative at the top of this file, and a scripted planner re-issues the
	// booking there because its script says to.
	//
	// A real model does not. Handed a transcript that already contains a
	// CONFIRMED booking, it answers "done" — correctly. So that configuration
	// cannot produce two live bookings from a live model, and measuring
	// rephrasing with it would measure nothing.
	//
	// With it true, the crash models the case the engine actually has to
	// survive: a worker that executed its activity — the seat is really taken,
	// the card is really charged — and died before committing anything about
	// it. The events were in a transaction that never landed, so recovery
	// finds an empty history and re-plans the lost step from the same goal and
	// the same empty transcript the first incarnation had. The two bookings
	// are then two independent answers to one identical question, which is
	// exactly the condition the rephrasing claim is about, and the side effect
	// in the world is still counted by Simulate's booking list.
	LoseUncommittedJournal bool
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
		CrashAfterTool:          ActivityBookFlight,
	}
}

// Request is the workflow input that drives the fixture.
func (f *DoubleBooking) Request(requestID string) Request {
	iterations := f.MaxIterations
	if iterations <= 0 {
		// Three is what this scenario has always used: one step to book, one
		// step for the plan the crash interrupts, and one spare.
		iterations = 3
	}
	return Request{
		RequestID:     requestID,
		UserID:        "demo-user",
		Goal:          f.Goal,
		MaxIterations: iterations,
	}
}

// Planner returns the planner for a given process incarnation, counting from
// zero. Anything past the second incarnation reuses the recovered planner,
// because by then the booking is done and the script only has to finish.
func (f *DoubleBooking) Planner(incarnation int) Planner {
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
	// After is the first tool call the recovered incarnation executed for
	// real. It is the zero value when the recovered incarnation executed none,
	// which AfterCalls distinguishes from "made one that was empty".
	After ToolCall
	// AfterCalls is every tool call the recovered incarnation executed for
	// real, in order.
	//
	// It is a slice and not a count because a live model is free to do
	// something other than re-issue the one lost call, and which of those it
	// did is a measurement rather than a malfunction. An empty slice means the
	// recovered run decided no tool call was needed — for a real model handed
	// a transcript that already shows the booking, that is the correct
	// decision and a result worth reporting.
	AfterCalls []ToolCall
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
