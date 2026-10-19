package semantic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// TestGateDecidesEscalatesInsteadOfRunningAgain pins the design change the
// ten-domain run forced.
//
// With every deciding field in agreement and only the leftover prose
// disagreeing, the comparator as first built answered DISTINCT — which runs
// the activity a second time. That is the wrong direction for doubt about an
// irreversible action, and on the ten-domain corpus it discarded almost every
// duplicate the gate had correctly passed. With GateDecides on, the same pair
// is ESCALATE: a question for a person, not a second charge.
func TestGateDecidesEscalatesInsteadOfRunningAgain(t *testing.T) {
	const (
		activity = "SendPayment"
		// Same amount, same currency, same payee; the prose around them is
		// different enough that a threshold meant for whole intents fails it.
		first  = `pay 4500 inr to acme supplies, the invoice from last quarter that finance flagged`
		second = `pay 4500 inr to acme supplies for the flagged quarterly bill`
	)

	run := func(gateDecides bool) core.DedupVerdict {
		t.Helper()
		emb := NewFakeEmbedder(DefaultDim)
		store := &memIntents{}
		textA, err := IntentText(activity, mustJSON(t, first))
		if err != nil {
			t.Fatalf("intent text: %v", err)
		}
		vecA, err := emb.Embed(context.Background(), textA)
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		if err := store.Put(context.Background(), core.Intent{
			ShardID: 0, WorkflowID: "wf", RunID: "run", ScheduledSeq: 1,
			ActivityType: activity, IntentText: textA, Embedding: vecA,
		}); err != nil {
			t.Fatalf("put: %v", err)
		}

		c := NewChecker(emb, store)
		c.Fields = GenericFields
		c.GateMode = GateNeutral
		c.MinAgreeing = 1
		c.GateDecides = gateDecides
		th := core.Thresholds{TauLow: 0.85, TauHigh: 0.90}
		c.Thresholds = th

		ev, err := c.Check(context.Background(), CheckRequest{
			ShardID: 0, WorkflowID: "wf", RunID: "run", K: 1,
			ActivityType: activity, Input: mustJSON(t, second),
			Thresholds: th, KBySeq: map[core.Seq]int{1: 0},
		})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		return ev.Verdict
	}

	// The fake embedder gives these two remainders a low pseudo-similarity,
	// which is exactly the situation under test: the gate agrees and the
	// remainder does not.
	off := run(false)
	on := run(true)

	if off != core.VerdictDistinct {
		t.Skipf("with GateDecides off this pair scored %s rather than DISTINCT, so the "+
			"behaviour under test is not exercised by this fixture", off)
	}
	if on != core.VerdictEscalate {
		t.Errorf("with GateDecides on the verdict is %s, want ESCALATE: every deciding field "+
			"agreed and only the free text did not, which is doubt and not evidence of a "+
			"different action", on)
	}
}

// mustJSON renders one prose argument as the tool-call envelope the agent
// schedules, which is the shape every extractor is handed in production.
func mustJSON(t *testing.T, args string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"tool": "SendPayment", "args": args})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
