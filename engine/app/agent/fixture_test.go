package agent

import (
	"testing"

	"github.com/dharaneswar339/durexec/internal/faultinj"
)

// This is the test the whole evaluation rests on.
//
// It drives the double-booking fixture through a crash and a recovery, and
// checks the one property that makes semantic deduplication necessary: the two
// booking requests mean the same thing and share no bytes. An exact
// idempotency key is a hash of those bytes, so it cannot possibly see that the
// second call is a repeat of the first.
func TestDoubleBookingFixtureProducesTwoDifferentlyWordedBookings(t *testing.T) {
	fixture := NewDoubleBooking()

	trace, bookings, err := fixture.Simulate()
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	if !trace.SameTool() {
		t.Fatalf("the two calls hit different tools: %q and %q", trace.Before.Tool, trace.After.Tool)
	}
	if trace.Before.Tool != ActivityBookFlight {
		t.Fatalf("the repeated tool is %q, want %q", trace.Before.Tool, ActivityBookFlight)
	}

	// The heart of it. Byte-equal keys cannot catch this.
	if trace.ArgsAreByteEqual() {
		t.Fatalf("both bookings used the same words (%q), so the fixture no longer "+
			"reproduces the failure that motivates semantic matching", trace.Before.Args)
	}

	// Nor can cheap normalisation. If lowercasing and trimming were enough,
	// no embedding would be needed and the contribution would be unnecessary.
	if trace.ArgsAreCanonicallyEqual() {
		t.Fatalf("trimming and lowercasing was enough to match %q with %q; the "+
			"fixture must need a meaning-aware comparison", trace.Before.Args, trace.After.Args)
	}

	if trace.Before.Args != IntentBeforeCrash {
		t.Errorf("pre-crash booking = %q, want %q", trace.Before.Args, IntentBeforeCrash)
	}
	if trace.After.Args != IntentAfterCrash {
		t.Errorf("post-crash booking = %q, want %q", trace.After.Args, IntentAfterCrash)
	}

	// Without a deduplication check, the engine takes two seats for one
	// request. That is the harm the mechanism prevents.
	if len(bookings) != 2 {
		t.Fatalf("the scenario took %d seats, want 2 — one before the crash and "+
			"one after, which is exactly the double booking", len(bookings))
	}
	if bookings[0].Call.Args == bookings[1].Call.Args {
		t.Fatal("the two seats were taken with identical arguments")
	}
}

// Recovery must genuinely replay: the seat taken before the crash is a durable
// fact and the tool must not be called again for it. If the recovered run
// re-executed the first booking, the scenario would be testing something else.
func TestRecoveredRunReplaysTheJournaledBooking(t *testing.T) {
	fixture := NewDoubleBooking()

	trace, _, err := fixture.Simulate()
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	if trace.ReplayedCalls == 0 {
		t.Fatal("the recovered run replayed nothing, so it did not recover from a journal")
	}
	if !trace.Result.Approved {
		t.Errorf("the recovered run did not reach approval: %+v", trace.Result)
	}
	if trace.Result.ConfirmationID == "" {
		t.Error("the recovered run did not finalise")
	}
}

// The fixture must be reusable: two constructions produce the same scenario,
// so a test, the evaluation and the demo all see identical inputs.
func TestFixtureIsReproducible(t *testing.T) {
	a, _, err := NewDoubleBooking().Simulate()
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	b, _, err := NewDoubleBooking().Simulate()
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if a.Before != b.Before || a.After != b.After {
		t.Fatalf("two runs of the fixture differed:\n  %+v\n  %+v", a, b)
	}
}

func TestFixtureExpectedCallsMatchWhatItProduces(t *testing.T) {
	fixture := NewDoubleBooking()
	wantBefore, wantAfter := fixture.ExpectedCalls()

	trace, _, err := fixture.Simulate()
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if trace.Before != wantBefore {
		t.Errorf("ExpectedCalls before = %+v, actual %+v", wantBefore, trace.Before)
	}
	if trace.After != wantAfter {
		t.Errorf("ExpectedCalls after = %+v, actual %+v", wantAfter, trace.After)
	}
}

// Arming is the fixture's only interaction with the injector, and it must be
// inert when configuration never enabled it.
func TestArmIsANoOpOnADisabledInjector(t *testing.T) {
	fixture := NewDoubleBooking()

	disabled := faultinj.Disabled()
	fixture.Arm(disabled)
	if disabled.Armed() || disabled.ShouldCrash(fixture.CrashPoint) {
		t.Fatal("a disabled injector was armed by the fixture")
	}

	enabled := faultinj.New(true)
	fixture.Arm(enabled)
	if !enabled.ShouldCrash(fixture.CrashPoint) {
		t.Fatalf("the fixture did not arm %s on an enabled injector", fixture.CrashPoint)
	}
}

func TestFixtureCrashPointIsTheOneTheScenarioNeeds(t *testing.T) {
	// The crash has to land while the *next* plan is in flight. A crash after
	// the plan was journaled would replay it verbatim and never re-word it.
	if got := NewDoubleBooking().CrashPoint; got != faultinj.BeforeNextPlanCompletes {
		t.Fatalf("CrashPoint = %q, want %q", got, faultinj.BeforeNextPlanCompletes)
	}
}
