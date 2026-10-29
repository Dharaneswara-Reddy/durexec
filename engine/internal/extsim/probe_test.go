package extsim

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// TestProbeAbsentUnlessRegistered is the safety regression for the test-only
// external-effect path. engined calls RegisterProbe only when EXTSIM_URL is
// set; this pins the other half of that contract — that NOT calling it leaves
// no trace. If either name could appear in a registry by any other route, a
// deployment could make a real network call believing it only ran in-process
// activities.
func TestProbeAbsentUnlessRegistered(t *testing.T) {
	acts := activity.NewRegistry()
	wfs := workflow.NewRegistry()

	if _, ok := wfs.Get(ProbeWorkflowType); ok {
		t.Fatalf("%s is registered in a fresh registry", ProbeWorkflowType)
	}
	if _, ok := acts.Get(PayActivityName); ok {
		t.Fatalf("%s is registered in a fresh registry", PayActivityName)
	}

	if err := RegisterProbe(acts, wfs, "http://127.0.0.1:1/"); err != nil {
		t.Fatalf("RegisterProbe: %v", err)
	}
	if _, ok := wfs.Get(ProbeWorkflowType); !ok {
		t.Fatalf("%s missing after RegisterProbe", ProbeWorkflowType)
	}
	if _, ok := acts.Get(PayActivityName); !ok {
		t.Fatalf("%s missing after RegisterProbe", PayActivityName)
	}
}

// TestProbeInputDefaults pins the defaults, because a probe run with zero
// payments would silently assert nothing at all and still pass.
func TestProbeInputDefaults(t *testing.T) {
	var got ProbeInput
	if err := json.Unmarshal([]byte(`{}`), &got); err != nil {
		t.Fatal(err)
	}
	// The defaults are applied inside ProbeWorkflow, so drive it with a
	// context that records what it was asked to do.
	rec := &recordingCtx{}
	if _, err := ProbeWorkflow(rec, []byte(`{}`)); err == nil {
		// recordingCtx returns an error from Get, so ProbeWorkflow must fail;
		// what matters is that it asked for exactly one payment first.
		t.Fatal("expected the recording context to fail the activity")
	}
	if rec.calls != 1 {
		t.Fatalf("default Payments produced %d activity calls, want 1", rec.calls)
	}
	if rec.lastType != PayActivityName {
		t.Fatalf("scheduled %q, want %q", rec.lastType, PayActivityName)
	}
	in, ok := rec.lastInput.(PayInput)
	if !ok {
		t.Fatalf("input was %T, want PayInput", rec.lastInput)
	}
	if in.Amount == 0 || in.Payee == "" {
		t.Fatalf("defaults not applied: %+v", in)
	}
}

// TestProbeRejectsBadInput keeps a malformed probe request from looking like a
// successful run with no effects.
func TestProbeRejectsBadInput(t *testing.T) {
	_, err := ProbeWorkflow(&recordingCtx{}, []byte(`{"amount": "not-a-number"}`))
	if err == nil {
		t.Fatal("undecodable input was accepted")
	}
	if !strings.Contains(err.Error(), "undecodable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestExternalPaymentHasNoCompensation is a TRIPWIRE, not an endorsement.
//
// Phase 7 scenario E showed a real external payment standing after its
// workflow failed, with zero compensation events. That is correct for the code
// as written, and the honest state of the design: a refund is itself an
// external effect needing its own idempotency key, and — the hard part —
// refunding a payment whose outcome is still AMBIGUOUS can create a negative
// effect that never had a positive one. See
// docs/cloud-testing/08-DESIGN-external-effect-contract.md.
//
// So this test fails the moment someone registers a compensation here. That is
// deliberate: the failure is the prompt to answer the ambiguity question
// first, rather than shipping a refund that fires on uncertain evidence. If
// the design question HAS been answered, update this test and say so in the
// report.
func TestExternalPaymentHasNoCompensation(t *testing.T) {
	def := PayDefinition(nil, "http://127.0.0.1:1/")
	// Compensation is the NAME of the compensating activity, so empty means
	// none is registered.
	if def.Compensation != "" {
		t.Fatalf("ExternalPayment now registers the compensation %q. Before allowing this, "+
			"confirm the compensation has its own stable idempotency key DERIVED from the "+
			"effect key, and that it cannot run while the original effect's outcome is "+
			"ambiguous. See docs/cloud-testing/08-DESIGN-external-effect-contract.md.", def.Compensation)
	}
}

// TestProbeFailAfterReportsFailureNotSuccess pins the other half of the same
// guarantee: a workflow whose later step fails must FAIL. If it returned
// success, a run that left a real payment un-refunded would look clean.
func TestProbeFailAfterReportsFailureNotSuccess(t *testing.T) {
	rec := &recordingOKCtx{}
	out, err := ProbeWorkflow(rec, []byte(`{"payments":1,"fail_after":true}`))
	if err == nil {
		t.Fatalf("fail_after returned success with output %s", out)
	}
	if rec.calls != 1 {
		t.Fatalf("the payment was not attempted before the failure (calls=%d)", rec.calls)
	}
	if !strings.Contains(err.Error(), "already landed") {
		t.Fatalf("the failure does not say the effect had already landed: %v", err)
	}
}
