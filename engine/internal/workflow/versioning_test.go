package workflow

// Regression tests for findings F-03 and F-04: replay across a code change.
//
// F-04 was the dangerous one because it was silent. The replay signature was
// "activity:" plus the activity type and nothing else, so new code calling the
// SAME activity at the SAME k with DIFFERENT arguments matched, replay returned
// the old result, and the workflow carried on believing the new call had
// happened. ChargePayment(100) could resolve as if 200 had been charged with
// nothing in the log to show it.
//
// F-03 was the loud one: when divergence WAS detected the engine committed a
// terminal failure, which skipped every compensation. The worker-side half of
// that fix is in internal/runtime; what is tested here is that the dispatcher
// reports the problem and writes nothing.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---- F-04: the input is part of the call's identity ----------------------

// THE F-04 REGRESSION TEST. Before the fix this replayed "cleanly" and the
// workflow completed holding a result for a charge of 100 while asking for 200.
func TestSameActivityWithADifferentInputIsADivergence(t *testing.T) {
	charge := func(amount int) Fn {
		return func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
			return ctx.ExecuteActivity("ChargePayment",
				map[string]any{"amount": amount, "currency": "GBP"}).Get()
		}
	}

	// History recorded a charge of 100 and its result.
	h := newHistory(t).activityDoneWith(0, "ChargePayment",
		`{"amount":100,"currency":"GBP"}`, `{"charged":100}`)

	// The same code replays cleanly...
	if _, err := dispatcherFor(t, charge(100)).Execute(h.done(), "test", nil); err != nil {
		t.Fatalf("the code that wrote this history must replay cleanly: %v", err)
	}

	// ...and code that now asks for 200 must NOT be handed the old result.
	_, err := dispatcherFor(t, charge(200)).Execute(h.done(), "test", nil)
	var nd *core.NonDeterminismError
	if !errors.As(err, &nd) {
		t.Fatalf("a changed amount at a recorded k silently reused the old result: err = %v", err)
	}
	if nd.K != 0 {
		t.Fatalf("divergence at k=%d, want 0", nd.K)
	}
	if nd.Recorded == nd.Requested {
		t.Fatalf("recorded and requested signatures are identical (%q), so the input is not in the signature", nd.Recorded)
	}
}

// The other half of the same property, and the reason the input cannot simply
// be compared byte for byte: a difference of FORMATTING must not be reported as
// a divergence, or correct code would be refused on a later Go version.
func TestTheSameInputWrittenDifferentlyIsNotADivergence(t *testing.T) {
	code := func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		// Marshalling a Go map: key order is Go's business, not the test's.
		return ctx.ExecuteActivity("ChargePayment", map[string]any{
			"amount":   1,
			"currency": "GBP",
			"note":     "A",
		}).Get()
	}

	// Every one of these is the same JSON value written differently: reordered
	// keys, a number spelled as a float, a number in exponent form, an escaped
	// string, and added whitespace.
	for _, stored := range []string{
		`{"amount":1,"currency":"GBP","note":"A"}`,
		`{"note":"A","currency":"GBP","amount":1}`,
		`{"amount":1.0,"currency":"GBP","note":"A"}`,
		`{"amount":1e0,"currency":"GBP","note":"A"}`,
		`{"amount":1,"currency":"GBP","note":"A"}`,
		"{ \"amount\" : 1 ,\n  \"currency\":\"GBP\", \"note\":\"A\" }",
	} {
		h := newHistory(t).activityDoneWith(0, "ChargePayment", stored, `{"ok":true}`)
		if _, err := dispatcherFor(t, code).Execute(h.done(), "test", nil); err != nil {
			t.Fatalf("stored input %s was reported as a divergence: %v", stored, err)
		}
	}
}

// The canonicalisation rules, asserted directly, because the signature is only
// as trustworthy as these. Each case is one rule from canonical.go.
func TestCanonicalisationRules(t *testing.T) {
	same := []struct {
		rule string
		a, b string
	}{
		{"object key order is ignored", `{"a":1,"b":2}`, `{"b":2,"a":1}`},
		{"nested key order is ignored", `{"x":{"a":1,"b":2}}`, `{"x":{"b":2,"a":1}}`},
		{"1 and 1.0 are one number", `1`, `1.0`},
		{"exponent form is one number", `0.5`, `5e-1`},
		{"a large integer keeps its value", `10000000000000000001`, `1.0000000000000000001e19`},
		{"string escaping is normalised", `"A"`, `"A"`},
		{"whitespace is ignored", `{"a":[1,2]}`, "{ \"a\" : [ 1 , 2 ] }"},
		{"an absent input and an explicit null agree", ``, `null`},
	}
	for _, c := range same {
		if inputDigest(json.RawMessage(c.a)) != inputDigest(json.RawMessage(c.b)) {
			t.Errorf("%s: %s and %s digested differently (%s / %s)",
				c.rule, c.a, c.b, canonicalise(json.RawMessage(c.a)), canonicalise(json.RawMessage(c.b)))
		}
	}

	differ := []struct {
		rule string
		a, b string
	}{
		{"array order is meaning", `[1,2]`, `[2,1]`},
		{"values differ", `{"amount":100}`, `{"amount":200}`},
		{"a number is not its string", `1`, `"1"`},
		{"an absent field is NOT an explicit null", `{}`, `{"a":null}`},
		{"a missing field is a different call", `{"a":1}`, `{"a":1,"b":2}`},
		{"precision past float64 is kept", `10000000000000000001`, `10000000000000000002`},
		{"zero is not empty string", `0`, `""`},
	}
	for _, c := range differ {
		if inputDigest(json.RawMessage(c.a)) == inputDigest(json.RawMessage(c.b)) {
			t.Errorf("%s: %s and %s digested the same (%s)",
				c.rule, c.a, c.b, canonicalise(json.RawMessage(c.a)))
		}
	}
}

// Canonicalisation must never panic or diverge on input it cannot parse: it
// runs inside replay, where a panic would take the worker with it.
func TestCanonicalisationIsTotal(t *testing.T) {
	for _, raw := range []string{``, `   `, `{`, `not json`, `{"a":1} trailing`, "\x00\x01"} {
		a := inputDigest(json.RawMessage(raw))
		b := inputDigest(json.RawMessage(raw))
		if a != b {
			t.Errorf("digest of %q is not stable: %s then %s", raw, a, b)
		}
	}
	// And unparseable input can never collide with valid JSON that happens to
	// render the same way.
	if inputDigest(json.RawMessage(`not json`)) == inputDigest(json.RawMessage(`"not json"`)) {
		t.Error("unparseable input collided with a JSON string of the same text")
	}
}

// ---- F-03: a version A run resumed under version B ----------------------

// twoStepWorkflow is version A: charge, then ship, then return a receipt.
func twoStepWorkflow(shipTo string) Fn {
	return func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		charged, err := ctx.ExecuteActivity("ChargePayment", map[string]any{"amount": 100}).Get()
		if err != nil {
			return nil, err
		}
		if _, err := ctx.ExecuteActivity("Ship", map[string]any{"to": shipTo}).Get(); err != nil {
			return nil, err
		}
		return charged, nil
	}
}

// historyAfterVersionA is the state of a run that started under version A and
// persisted two decisions: the charge completed, and the shipment is scheduled
// but not yet resolved. That mid-flight shape is the one a rolling deployment
// actually meets.
func historyAfterVersionA(t *testing.T, version string) *histBuilder {
	t.Helper()
	return newHistory(t).
		startedAtVersion(version).
		activityDoneWith(0, "ChargePayment", `{"amount":100}`, `{"charged":100}`).
		activityScheduledWith(1, "Ship", `{"to":"London"}`)
}

// A COMPATIBLE change: version B's code is different — it has been
// recompiled, its comments and internals have moved — but every call it makes
// at a k that history already records is the same call with the same argument.
// The run must continue, and must NOT re-schedule the shipment that is already
// in flight.
func TestARunStartedUnderVersionAResumesUnderACompatibleVersionB(t *testing.T) {
	h := historyAfterVersionA(t, "A")

	d := dispatcherFor(t, twoStepWorkflow("London"))
	d.CodeVersion = "B"
	d.AcceptVersion = func(recorded string) bool { return recorded == "A" || recorded == "B" }

	res, err := d.Execute(h.done(), "test", nil)
	if err != nil {
		t.Fatalf("a compatible version B must resume the run: %v", err)
	}
	if res.Status != core.StatusRunning || !res.Blocking {
		t.Fatalf("status = %q blocking = %v, want RUNNING and blocked on the shipment", res.Status, res.Blocking)
	}
	// The crucial assertion: nothing was scheduled again. A second Ship command
	// would ship the order twice.
	if len(res.Commands) != 0 {
		t.Fatalf("version B emitted %d command(s) for work already in flight: %+v",
			len(res.Commands), res.Commands)
	}

	// And once the shipment resolves, the run completes with the recorded
	// charge — the result version A obtained, not a new one.
	h.activityCompleted(1, `{"shipped":true}`)
	res, err = d.Execute(h.done(), "test", nil)
	if err != nil {
		t.Fatalf("completing under version B: %v", err)
	}
	if res.Status != core.StatusCompleted {
		t.Fatalf("status = %q, want COMPLETED", res.Status)
	}
	if string(res.Output) != `{"charged":100}` {
		t.Fatalf("output = %s, want the charge recorded under version A", res.Output)
	}
}

// An INCOMPATIBLE change: version B ships somewhere else. The destination is
// an argument at a k that history already records, so the engine must refuse —
// and must refuse by returning, leaving history untouched, rather than by
// journaling a terminal failure that would strand the charge with no refund.
func TestARunStartedUnderVersionAIsRefusedByAnIncompatibleVersionB(t *testing.T) {
	h := historyAfterVersionA(t, "A")
	before := len(h.done())

	d := dispatcherFor(t, twoStepWorkflow("Manchester"))
	d.CodeVersion = "B"
	d.AcceptVersion = func(string) bool { return true } // the gate is open; the code itself diverges

	res, err := d.Execute(h.done(), "test", nil)
	var nd *core.NonDeterminismError
	if !errors.As(err, &nd) {
		t.Fatalf("version B shipping elsewhere was accepted: res = %+v err = %v", res, err)
	}
	if nd.K != 1 {
		t.Fatalf("divergence at k=%d, want 1 — the shipment is the call that changed", nd.K)
	}
	// No commands, no status, no output: the dispatcher decided nothing.
	if len(res.Commands) != 0 || res.Status != "" {
		t.Fatalf("a refused replay produced a decision: %+v", res)
	}
	if after := len(h.done()); after != before {
		t.Fatalf("history grew from %d to %d events; a divergence must leave it untouched", before, after)
	}
	// Redeploying version A is the documented recovery, so it has to work on
	// the very same history.
	if _, err := dispatcherFor(t, twoStepWorkflow("London")).Execute(h.done(), "test", nil); err != nil {
		t.Fatalf("redeploying the original code must resume the run: %v", err)
	}
}

// The version gate itself: a binary that declines the recorded version must
// refuse BEFORE running any workflow code, so it cannot decide anything at all.
func TestTheVersionGateRefusesBeforeRunningAnyWorkflowCode(t *testing.T) {
	h := historyAfterVersionA(t, "A")

	ran := false
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		ran = true
		return nil, nil
	})
	d.CodeVersion = "C"
	d.AcceptVersion = func(recorded string) bool { return recorded == "C" }

	res, err := d.Execute(h.done(), "test", nil)
	var vi *core.VersionIncompatibleError
	if !errors.As(err, &vi) {
		t.Fatalf("got %v, want *core.VersionIncompatibleError", err)
	}
	if ran {
		t.Error("workflow code ran despite the version being refused")
	}
	if vi.Recorded != "A" || vi.Running != "C" {
		t.Errorf("error names recorded=%q running=%q, want A and C", vi.Recorded, vi.Running)
	}
	if len(res.Commands) != 0 || res.Status != "" {
		t.Fatalf("a refused version produced a decision: %+v", res)
	}
}

// Version checking must be OFF unless a deployer turns it on, or simply
// deploying a build that sets CodeVersion would park every run in flight.
func TestVersionCheckingIsOffByDefaultAndAcceptsUnversionedRuns(t *testing.T) {
	// A run from before versions were recorded at all.
	h := newHistory(t).
		activityDoneWith(0, "ChargePayment", `{"amount":100}`, `{"charged":100}`).
		activityScheduledWith(1, "Ship", `{"to":"London"}`)

	d := dispatcherFor(t, twoStepWorkflow("London"))
	d.CodeVersion = "B" // declared, but no policy set
	if _, err := d.Execute(h.done(), "test", nil); err != nil {
		t.Fatalf("an unversioned run must replay when no version policy is set: %v", err)
	}

	// A policy that forgets about unversioned runs refuses them, which is the
	// deployer's mistake to make knowingly rather than the engine's to hide.
	d.AcceptVersion = func(recorded string) bool { return recorded == "B" }
	var vi *core.VersionIncompatibleError
	if _, err := d.Execute(h.done(), "test", nil); !errors.As(err, &vi) {
		t.Fatalf("got %v, want the unversioned run refused by an exact-match policy", err)
	}
	if vi.Recorded != "" {
		t.Errorf("recorded version = %q, want empty for an unversioned run", vi.Recorded)
	}
}
