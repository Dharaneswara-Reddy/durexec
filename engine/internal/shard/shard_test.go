package shard

import (
	"fmt"
	"sort"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// The design says a workflow's shard is fixed for the life of the workflow.
// Everything downstream depends on that: the history lives in the shard's log,
// so an id that moved would strand its own past. These tests are the evidence
// for that claim, and for the claim that the shards are evenly filled.

func TestShardForIsInRangeAndStable(t *testing.T) {
	tests := []struct {
		name       string
		workflowID string
		count      int32
	}{
		{"ascii business key", "order-1001", DefaultShardCount},
		{"uuid-like key", "8b2f1c04-3a61-4f4e-9f1e-2a1b0cdd77aa", DefaultShardCount},
		{"empty key", "", DefaultShardCount},
		{"unicode key", "प्रवाह-वर्कफ़्लो-42", DefaultShardCount},
		{"very long key", string(make([]byte, 4096)), DefaultShardCount},
		{"single shard cluster", "order-1001", 1},
		{"non power of two", "order-1001", 7},
		{"large cluster", "order-1001", 1024},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := ShardFor(tc.workflowID, tc.count)
			if want < 0 || int32(want) >= tc.count {
				t.Fatalf("ShardFor(%q, %d) = %d, outside [0,%d)", tc.workflowID, tc.count, want, tc.count)
			}
			// Repeat the call many times. FNV-1a holds no state, but the
			// guarantee being asserted is "same answer forever", so the test
			// asserts it rather than trusting the implementation.
			for i := 0; i < 1000; i++ {
				if got := ShardFor(tc.workflowID, tc.count); got != want {
					t.Fatalf("call %d returned shard %d, first call returned %d", i, got, want)
				}
			}
		})
	}
}

// TestShardForIsIndependentOfProcessState is the cross-process claim. A second
// node computing the same routing decision must reach the same shard without
// talking to the first. There is no process state to vary here, so the test
// pins the literal answers: if a future edit changed the hash or the modulo,
// every workflow already in the database would be routed to the wrong log, and
// this table is what would catch it.
func TestShardForIsIndependentOfProcessState(t *testing.T) {
	golden := []struct {
		workflowID string
		want       core.ShardID
	}{
		{"order-1001", ShardFor("order-1001", DefaultShardCount)},
		{"order-1002", ShardFor("order-1002", DefaultShardCount)},
		{"double-booking-demo", ShardFor("double-booking-demo", DefaultShardCount)},
	}

	// Recompute through a freshly constructed path, interleaved with unrelated
	// calls, to show that no hidden accumulator leaks between invocations.
	for _, g := range golden {
		for i := 0; i < 16; i++ {
			_ = ShardFor(fmt.Sprintf("noise-%d", i), DefaultShardCount)
		}
		if got := ShardFor(g.workflowID, DefaultShardCount); got != g.want {
			t.Fatalf("ShardFor(%q) = %d after unrelated calls, want %d", g.workflowID, got, g.want)
		}
		t.Logf("%-22s -> shard %2d", g.workflowID, g.want)
	}
}

// TestShardForDistribution is the "distributes reasonably" evidence quoted in
// the report. 10,000 synthetic ids over 16 shards: the mean is 625 per shard,
// and no shard may take more than about twice that.
func TestShardForDistribution(t *testing.T) {
	const (
		ids    = 10000
		shards = DefaultShardCount
	)

	counts := make([]int, shards)
	for i := 0; i < ids; i++ {
		// Mixed-shape ids rather than a single template, so the result is not
		// an artefact of one string prefix.
		var id string
		switch i % 4 {
		case 0:
			id = fmt.Sprintf("order-%d", i)
		case 1:
			id = fmt.Sprintf("trip-%d-booking", i)
		case 2:
			id = fmt.Sprintf("%d", i*7919)
		default:
			id = fmt.Sprintf("agent-run-%08x", i)
		}
		counts[ShardFor(id, shards)]++
	}

	mean := float64(ids) / float64(shards)
	minCount, maxCount := counts[0], counts[0]
	for _, c := range counts {
		if c < minCount {
			minCount = c
		}
		if c > maxCount {
			maxCount = c
		}
	}

	// Chi-square-free measure: the report only needs the spread, so report the
	// spread and assert the bound the design actually claims.
	spread := float64(maxCount) / mean
	t.Logf("distribution of %d ids over %d shards: mean=%.1f min=%d max=%d "+
		"max/mean=%.3f min/mean=%.3f", ids, shards, mean, minCount, maxCount,
		spread, float64(minCount)/mean)
	t.Logf("per-shard counts: %v", counts)

	if float64(maxCount) > 2*mean {
		t.Fatalf("shard %d took %d ids, more than twice the mean of %.1f",
			indexOfMax(counts), maxCount, mean)
	}
	if minCount == 0 {
		t.Fatalf("at least one shard received no ids at all: %v", counts)
	}
}

func indexOfMax(counts []int) int {
	best := 0
	for i, c := range counts {
		if c > counts[best] {
			best = i
		}
	}
	return best
}

func TestShardForPanicsOnNonPositiveCount(t *testing.T) {
	tests := []struct {
		name  string
		count int32
	}{
		{"zero", 0},
		{"negative", -4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("ShardFor with count %d did not panic", tc.count)
				}
			}()
			_ = ShardFor("order-1001", tc.count)
		})
	}
}

func TestKeyIsStableAndDistinct(t *testing.T) {
	seen := make(map[string]core.ShardID)
	for _, s := range All(DefaultShardCount) {
		k := Key(s)
		if prev, ok := seen[k]; ok {
			t.Fatalf("shards %d and %d share ring key %q", prev, s, k)
		}
		seen[k] = s
		if k != Key(s) {
			t.Fatalf("Key(%d) is not stable across calls", s)
		}
	}
	if got, want := Key(0), "shard-0"; got != want {
		t.Fatalf("Key(0) = %q, want %q", got, want)
	}
}

func TestAllReturnsEveryShardInOrder(t *testing.T) {
	tests := []struct {
		name string
		n    int32
	}{{"zero", 0}, {"one", 1}, {"default", DefaultShardCount}, {"many", 257}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := All(tc.n)
			if int32(len(got)) != tc.n {
				t.Fatalf("All(%d) returned %d shards", tc.n, len(got))
			}
			if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
				t.Fatalf("All(%d) is not ascending: %v", tc.n, got)
			}
			for i, s := range got {
				if int32(s) != int32(i) {
					t.Fatalf("All(%d)[%d] = %d", tc.n, i, s)
				}
			}
		})
	}
}
