package activity

import (
	"strings"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

const (
	wfA  = "order-42"
	runA = core.RunID("11111111-1111-1111-1111-111111111111")
	runB = core.RunID("22222222-2222-2222-2222-222222222222")
)

// TestDedupKeyIsStable is the property the whole exactly-once-effect argument
// rests on. The key is what a payment provider sees as its Idempotency-Key, so
// if it ever changed between a call and its retry, the customer would be charged
// twice.
func TestDedupKeyIsStable(t *testing.T) {
	first := DedupKey(wfA, runA, 3)

	// Repeated calls in this process.
	for i := 0; i < 100; i++ {
		if got := DedupKey(wfA, runA, 3); got != first {
			t.Fatalf("call %d returned %q, want %q", i, got, first)
		}
	}

	// The function reads nothing but its arguments — no clock, no attempt
	// counter, no randomness — so a call made after a crash and a full replay,
	// possibly on a different machine, produces these same bytes. That is what
	// this hard-coded expectation records: if someone changes the recipe, this
	// test fails and every in-flight workflow's keys would have changed with it.
	const want = "dx1:7b9c15d083ee700b2df640839fbdea106ebe9c0846a0ffea79169e7cdd0b9ab0"
	if first != want {
		t.Fatalf("DedupKey(%q, %q, 3) = %q, want %q", wfA, runA, first, want)
	}
}

func TestDedupKeyDiffersPerField(t *testing.T) {
	base := DedupKey(wfA, runA, 0)

	cases := []struct {
		name string
		key  string
	}{
		// k is the program-order index of the call. Two charges in the same
		// run are different charges and must not share a key, or the second
		// would be silently swallowed by the provider as a duplicate.
		{"different k", DedupKey(wfA, runA, 1)},
		{"far-apart k", DedupKey(wfA, runA, 99)},
		// A retry of the business key is a new run and is genuinely allowed to
		// charge again.
		{"different run", DedupKey(wfA, runB, 0)},
		{"different workflow", DedupKey("order-43", runA, 0)},
	}

	seen := map[string]string{base: "base"}
	for _, c := range cases {
		if prev, dup := seen[c.key]; dup {
			t.Errorf("%s collided with %s", c.name, prev)
		}
		seen[c.key] = c.name
	}
}

// TestDedupKeyIsUnambiguous checks the length prefixing. Without it, the fields
// could be re-sliced across the boundary between them and two different runs
// could hash to the same key — which would mean two different payments sharing
// an idempotency key, and one of them silently never happening.
func TestDedupKeyIsUnambiguous(t *testing.T) {
	a := DedupKey("order", core.RunID("42run"), 0)
	b := DedupKey("order42", core.RunID("run"), 0)
	if a == b {
		t.Fatal("field boundaries are ambiguous: two different runs produced the same key")
	}
}

// TestDedupKeyIsOpaqueAndBounded: a workflow ID is caller-supplied and is about
// to be placed in an HTTP header on somebody else's server, so it must not be
// echoed there verbatim.
func TestDedupKeyIsOpaqueAndBounded(t *testing.T) {
	key := DedupKey("customer-email@example.com/very/long/business/key", runA, 7)
	if strings.Contains(key, "example.com") {
		t.Fatal("the workflow ID leaked into the dedup key")
	}
	if want := len("dx1:") + 64; len(key) != want {
		t.Fatalf("key length = %d, want a fixed %d", len(key), want)
	}
}
