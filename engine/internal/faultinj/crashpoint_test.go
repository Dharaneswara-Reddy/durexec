package faultinj

import "testing"

// TestDisarmedInjectorNeverCrashes is the safety regression. The property it
// pins is the one a production deployment relies on: an injector that
// configuration did not switch on ignores every arming call, so a CrashAt
// left behind in code — or reached through a configuration mistake — cannot
// make a real node abandon a transaction.
func TestDisarmedInjectorNeverCrashes(t *testing.T) {
	for _, inj := range map[string]*Injector{
		"nil":       nil,
		"disabled":  Disabled(),
		"new false": New(false),
	} {
		for _, p := range []CrashPoint{
			AfterActivityJournaled, BeforeDecisionCommit, BeforeStartCommit,
			BeforeNextPlanCompletes, AfterLeaseRenew,
		} {
			inj.CrashAt(p)
			if inj.ShouldCrash(p) {
				t.Fatalf("a disarmed injector reported a crash at %s", p)
			}
		}
		if inj.Armed() {
			t.Fatal("a disarmed injector reported itself armed after CrashAt calls")
		}
		if inj.Fired() != 0 {
			t.Fatalf("a disarmed injector counted %d firings", inj.Fired())
		}
	}
}

// TestEnabledInjectorCrashesExactlyOnce pins determinism: a point armed once
// fires once and then stops, so a node does not sit in a loop dying at the
// same boundary and a test can assert a count rather than "it failed
// sometimes".
func TestEnabledInjectorCrashesExactlyOnce(t *testing.T) {
	inj := New(true)
	inj.CrashAt(BeforeStartCommit)

	if !inj.Armed() {
		t.Fatal("armed point did not set the fast-path flag")
	}
	if !inj.ShouldCrash(BeforeStartCommit) {
		t.Fatal("first ShouldCrash did not fire")
	}
	if inj.ShouldCrash(BeforeStartCommit) {
		t.Fatal("second ShouldCrash fired; the crash was not consumed")
	}
	if inj.Armed() {
		t.Fatal("injector still armed after its only crash was consumed")
	}
	if got := inj.Fired(); got != 1 {
		t.Fatalf("Fired() = %d, want 1", got)
	}
}

// TestCrashPointIsolation proves the two commit-path boundaries are
// independent. Arming the creation boundary must not make an ordinary
// decision commit abandon its transaction, or every workflow in a run with
// the variable set would fail instead of exactly one start.
func TestCrashPointIsolation(t *testing.T) {
	inj := New(true)
	inj.CrashAt(BeforeStartCommit)
	if inj.ShouldCrash(BeforeDecisionCommit) {
		t.Fatal("arming BeforeStartCommit also fired BeforeDecisionCommit")
	}
	if !inj.ShouldCrash(BeforeStartCommit) {
		t.Fatal("BeforeStartCommit did not fire after an unrelated point was checked")
	}
}

// TestParseCrashPointRejectsUnwiredAndUnknown is the guard against a run that
// silently never crashes. A declared-but-unwired point and a typo must both
// be refused, because either would produce a report that read as if the
// boundary had been tested and survived.
func TestParseCrashPointRejectsUnwiredAndUnknown(t *testing.T) {
	for _, good := range WiredCrashPoints() {
		if p, ok := ParseCrashPoint(good); !ok || p != CrashPoint(good) {
			t.Fatalf("ParseCrashPoint(%q) = (%q,%v), want it accepted", good, p, ok)
		}
	}
	for _, bad := range []string{
		"",                        // no point
		"AfterLeaseRenew",         // declared but no caller
		"BeforeNextPlanCompletes", // fixture-only
		"beforestartcommit",       // wrong case
		"BeforeStartCommitt",      // typo
		"; DROP TABLE events",     // not a point at all
	} {
		if p, ok := ParseCrashPoint(bad); ok {
			t.Fatalf("ParseCrashPoint(%q) accepted it as %q", bad, p)
		}
	}
}

// TestWiredMatchesList keeps WiredCrashPoints and Wired from drifting apart,
// since configuration validates against one and reports the other.
func TestWiredMatchesList(t *testing.T) {
	listed := map[string]bool{}
	for _, s := range WiredCrashPoints() {
		listed[s] = true
	}
	for _, p := range []CrashPoint{
		CrashNone, AfterActivityJournaled, BeforeNextPlanCompletes,
		BeforeDecisionCommit, BeforeStartCommit, AfterLeaseRenew,
	} {
		if p.Wired() != listed[string(p)] {
			t.Fatalf("%q: Wired()=%v but listed=%v", p, p.Wired(), listed[string(p)])
		}
	}
}

// TestKillProcessIfArmedIsInertWhenDisarmed is the safety regression for the
// one seam that can actually terminate the engine.
//
// It cannot assert "the process did not die" directly — a test that died
// would simply fail the run — so it asserts the observable proxy that makes
// death impossible: the `before` callback is the last thing to execute before
// the signal, so if it is never invoked, the kill was never reached. A nil
// injector, a disabled one, and an enabled-but-unarmed one are all covered,
// because those are the three states a real deployment is ever in.
func TestKillProcessIfArmedIsInertWhenDisarmed(t *testing.T) {
	for name, inj := range map[string]*Injector{
		"nil":                 nil,
		"disabled":            Disabled(),
		"enabled but unarmed": New(true),
	} {
		called := false
		if inj.KillProcessIfArmed(BeforeResultJournal, func() { called = true }) {
			t.Fatalf("%s: KillProcessIfArmed reported a kill", name)
		}
		if called {
			t.Fatalf("%s: the pre-kill callback ran, so the kill path was reached", name)
		}
	}
}

// TestKillProcessPointIsDeclaredAsKilling keeps the advertised contract
// honest. A test harness decides whether it must disarm immediately — to stop
// the restarted pod dying again on the redelivered task — by asking
// KillsProcess, so a point that kills while denying it would crash-loop a
// node with no explanation.
func TestKillProcessPointIsDeclaredAsKilling(t *testing.T) {
	if !BeforeResultJournal.KillsProcess() {
		t.Fatal("BeforeResultJournal does not declare that it kills the process")
	}
	for _, p := range []CrashPoint{
		AfterActivityJournaled, BeforeDecisionCommit, BeforeStartCommit,
		BeforeNextPlanCompletes, AfterLeaseRenew, CrashNone,
	} {
		if p.KillsProcess() {
			t.Fatalf("%q claims to kill the process but returns an error", p)
		}
	}
	// It must also be armable from configuration, or the scenario cannot run.
	if _, ok := ParseCrashPoint(string(BeforeResultJournal)); !ok {
		t.Fatal("BeforeResultJournal is not accepted by ParseCrashPoint")
	}
}
