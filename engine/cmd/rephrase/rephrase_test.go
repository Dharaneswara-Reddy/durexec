package main

import (
	"path/filepath"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// The drift reader decides which trials count as evidence and which are thrown
// out, so it is the piece of this harness most able to quietly change a
// published figure. These tests pin its behaviour on the three cases that
// matter: a faithful rewording, a sentence that dropped a field, and a sentence
// that changed one.

func TestDriftReaderSeparatesRewordingFromDrift(t *testing.T) {
	byID := map[string]goalSpec{}
	for _, g := range goals {
		byID[g.ID] = g
	}

	cases := []struct {
		name     string
		goalID   string
		sentence string
		drifted  bool
		missing  []string
	}{
		{
			name:     "flight reworded with every field intact",
			goalID:   "f01",
			sentence: "Book the 7:40 Bengaluru to New Delhi flight on Friday morning, one seat",
		},
		{
			name:     "flight on the wrong day is drift, not a rewording",
			goalID:   "f01",
			sentence: "Book BLR to DEL Saturday morning for one passenger",
			drifted:  true,
		},
		{
			name:     "a reversed route is drift even with every word correct",
			goalID:   "f01",
			sentence: "Book Delhi to Bangalore Friday morning, one seat",
			drifted:  true,
		},
		{
			name:     "a dropped daypart is silence, not drift",
			goalID:   "f01",
			sentence: "Book Bangalore to Delhi on Friday for one passenger",
			missing:  []string{"daypart"},
		},
		{
			name:     "a reformatted amount is still the same amount",
			goalID:   "p01",
			sentence: "Transfer rs 4,500 to Anand for his work last month",
		},
		{
			name:     "a different amount is drift",
			goalID:   "p01",
			sentence: "Transfer 5,400 rupees to Anand for his work last month",
			drifted:  true,
		},
		{
			name:     "a different payee is drift",
			goalID:   "p01",
			sentence: "Send 4500 rupees to Priya for last month's work",
			drifted:  true,
		},
		{
			name:     "an order number reformatted is the same order",
			goalID:   "r01",
			sentence: "Refund order A10492, the item was broken on arrival",
		},
		{
			name:     "a different order number is drift",
			goalID:   "r01",
			sentence: "Refund order A-10493 because it arrived damaged",
			drifted:  true,
		},
		{
			name:     "a 30-minute meeting written with the hyphen still states its length",
			goalID:   "m01",
			sentence: "Book a 30-minute slot with Anita on Tuesday at 11 am",
		},
		{
			name:     "a message to the wrong recipient is drift",
			goalID:   "s01",
			sentence: "Email Carla to say we are renewing the lease for another year",
			drifted:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, ok := byID[tc.goalID]
			if !ok {
				t.Fatalf("the corpus has no goal %q", tc.goalID)
			}
			r := read(g, tc.sentence)
			if r.Drifted() != tc.drifted {
				t.Fatalf("drifted = %v, want %v (reading: %v)", r.Drifted(), tc.drifted, r.Stated())
			}
			missing := r.Missing()
			if len(missing) != len(tc.missing) {
				t.Fatalf("missing = %v, want %v", missing, tc.missing)
			}
			for i := range missing {
				if missing[i] != tc.missing[i] {
					t.Fatalf("missing = %v, want %v", missing, tc.missing)
				}
			}
		})
	}
}

// Every goal must declare values its own readers can actually find in the goal
// text. A goal whose declared day is not in the sentence the user typed would
// make the ground truth disagree with the request itself, and every trial on
// that goal would be classified against something nobody asked for.
func TestEveryGoalAgreesWithItsOwnText(t *testing.T) {
	for _, g := range goals {
		r := read(g, g.Text)
		if r.Drifted() {
			t.Errorf("goal %s contradicts its own declared fields: %v\n  text: %s",
				g.ID, r.Contradictions(), g.Text)
		}
		for _, f := range r.Fields {
			if f.Kind == fieldMissing {
				t.Errorf("goal %s declares %s=%s but its own text does not state it\n  text: %s",
					g.ID, f.Name, f.Want, g.Text)
			}
		}
	}
}

// The corpus has to stay balanced across domains, because every per-domain
// figure in the report is a count over equal-sized groups and the interleaving
// is what makes a truncated run balanced too.
func TestCorpusIsBalancedAndInterleaved(t *testing.T) {
	perDomain := map[string]int{}
	for _, g := range goals {
		perDomain[g.Domain]++
	}
	if len(perDomain) != len(domainOrder) {
		t.Fatalf("the corpus covers %d domains, domainOrder names %d", len(perDomain), len(domainOrder))
	}
	want := len(goals) / len(domainOrder)
	for _, name := range domainOrder {
		if perDomain[name] != want {
			t.Errorf("domain %s has %d goals, every domain should have %d", name, perDomain[name], want)
		}
	}
	// Any prefix one round long must touch every domain exactly once.
	seen := map[string]bool{}
	for i := 0; i < len(domainOrder); i++ {
		if seen[goals[i].Domain] {
			t.Fatalf("the first %d goals repeat domain %s, so a truncated run would be unbalanced",
				len(domainOrder), goals[i].Domain)
		}
		seen[goals[i].Domain] = true
	}
}

// Resumability is the whole reason the CSV exists, and it is only real if a
// row survives the round trip unchanged. A column that writes one way and
// reads back another would make a resumed run silently rerun paid-for trials.
func TestCSVRoundTripPreservesATrial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trials.csv")
	store, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}

	want := trial{
		Key: keyFor(armLostJournal, 7), Index: 7, Arm: armLostJournal,
		Domain: "payment", CommitTool: "SendPayment",
		Goal:       goalSpec{ID: "p01", Domain: "payment", Text: "Pay Anand 4,500 rupees for last month's work"},
		Outcome:    outcomeUsable,
		BeforeTool: "SendPayment", Before: "Send 4,500 rupees to Anand",
		AfterTool: "SendPayment", After: "Transfer rs 4500 to Anand",
		AfterCallCount: 1, SideEffects: 2,
		ExactKeyEqual: false, ByteIdentical: false, CanonicalEqual: false,
		Similarity: 0.912345, SemVerdict: core.VerdictDuplicate,
		HybridFellBack: true, HybridVerdict: core.VerdictDuplicate, HybridSimilarity: 0.912345,
		Drifted: false, BeforeFields: "amount=4500(agrees)", AfterFields: "amount=4500(agrees)",
		BeforeMissing: "currency",
	}
	if err := store.Append(want); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := readStore(path)
	if err != nil {
		t.Fatalf("readStore: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d trials, want 1", len(got))
	}
	g := got[0]
	if g.Key != want.Key || g.Domain != want.Domain || g.Outcome != want.Outcome {
		t.Errorf("identity fields differ:\n got %+v\nwant %+v", g, want)
	}
	if g.Before != want.Before || g.After != want.After {
		t.Errorf("sentences differ: got %q / %q", g.Before, g.After)
	}
	if g.SemVerdict != want.SemVerdict || g.HybridVerdict != want.HybridVerdict {
		t.Errorf("verdicts differ: got %q / %q", g.SemVerdict, g.HybridVerdict)
	}
	if !g.HybridFellBack {
		t.Error("hybrid_fell_back did not survive the round trip")
	}
	// Six decimal places are written, so the comparison is at that precision
	// rather than exact: asserting more than was stored would be asserting
	// against the CSV format rather than against the data.
	if diff := g.Similarity - want.Similarity; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("similarity = %f, want %f", g.Similarity, want.Similarity)
	}
	if g.SideEffects != want.SideEffects {
		t.Errorf("side effects = %d, want %d", g.SideEffects, want.SideEffects)
	}
}

// Appending must not rewrite the header, or a resumed run would leave a header
// row in the middle of the data and readStore would fail on it.
func TestAppendingToAnExistingStoreDoesNotRepeatTheHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trials.csv")
	for i := 1; i <= 2; i++ {
		store, err := openStore(path)
		if err != nil {
			t.Fatalf("openStore %d: %v", i, err)
		}
		if err := store.Append(trial{Key: keyFor(armLostJournal, i), Index: i,
			Arm: armLostJournal, Domain: "flight", Outcome: outcomeUsable}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		store.Close()
	}
	got, err := readStore(path)
	if err != nil {
		t.Fatalf("readStore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d trials, want 2", len(got))
	}
	if got[0].Key == got[1].Key {
		t.Fatalf("both rows have key %q", got[0].Key)
	}
}

// Every domain must describe two distinct tools and build a prompt that names
// them, or the model would be told about a tool it cannot call.
func TestEveryDomainDescribesItsTools(t *testing.T) {
	for _, name := range domainOrder {
		d, ok := domains[name]
		if !ok {
			t.Fatalf("domainOrder names %q, which is not a domain", name)
		}
		if d.ReadTool.Name == "" || d.CommitTool.Name == "" {
			t.Errorf("domain %s is missing a tool name", name)
			continue
		}
		if d.ReadTool.Name == d.CommitTool.Name {
			t.Errorf("domain %s uses one name for both tools", name)
		}
		prompt := d.Prompt()
		for _, want := range []string{d.ReadTool.Name, d.CommitTool.Name,
			"Vary your phrasing naturally"} {
			if !contains(prompt, want) {
				t.Errorf("domain %s prompt does not mention %q", name, want)
			}
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
