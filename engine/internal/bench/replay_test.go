package bench

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// TestScenariosAreWellFormed checks the fixtures before anything is timed. A
// benchmark that silently measures a broken replay is worse than no benchmark,
// because it produces a number that looks like evidence.
func TestScenariosAreWellFormed(t *testing.T) {
	d := NewDispatcher()

	tests := []struct {
		name           string
		events         int
		wantActivities int
		wantBlocking   bool
		wantStatus     core.ExecutionStatus
	}{
		{"10 events", 10, 4, true, core.StatusRunning},
		{"50 events", 50, 24, true, core.StatusRunning},
		{"100 events", 100, 49, true, core.StatusRunning},
		{"200 events", 200, 99, true, core.StatusRunning},
		{"500 events", 500, 249, true, core.StatusRunning},
		{"an odd count runs to completion", 101, 50, false, core.StatusCompleted},
		{"the smallest history", 2, 0, true, core.StatusRunning},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := BuildScenario(tc.events)
			if err != nil {
				t.Fatalf("BuildScenario(%d): %v", tc.events, err)
			}
			if len(s.Events) != tc.events {
				t.Fatalf("history has %d events, want %d", len(s.Events), tc.events)
			}
			if s.Activities != tc.wantActivities {
				t.Fatalf("history records %d completed activities, want %d", s.Activities, tc.wantActivities)
			}
			if s.Blocking != tc.wantBlocking {
				t.Fatalf("Blocking = %v, want %v", s.Blocking, tc.wantBlocking)
			}
			// Seqs must ascend from 1 with no gaps, the way the event store
			// promises, or indexHistory would be reading a log no database
			// would ever produce.
			for i, ev := range s.Events {
				if ev.Seq != core.Seq(i+1) {
					t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, i+1)
				}
			}

			res, err := d.Execute(s.Events, ChainWorkflow, s.Input)
			if err != nil {
				t.Fatalf("replay failed: %v", err)
			}
			if res.Status != tc.wantStatus {
				t.Fatalf("replay ended with status %s, want %s", res.Status, tc.wantStatus)
			}
			if res.Blocking != tc.wantBlocking {
				t.Fatalf("replay Blocking = %v, want %v", res.Blocking, tc.wantBlocking)
			}
			// A pure replay must produce no new commands: every decision it
			// reached was already in the log. A command here would mean the
			// benchmark was measuring new work, not replay.
			if tc.wantBlocking && len(res.Commands) != 0 {
				t.Fatalf("a pure replay emitted %d commands, want none: %+v", len(res.Commands), res.Commands)
			}
		})
	}
}

func TestBuildScenarioRejectsTooShortAHistory(t *testing.T) {
	for _, n := range []int{-1, 0, 1} {
		if _, err := BuildScenario(n); err == nil {
			t.Fatalf("BuildScenario(%d) should have failed", n)
		}
	}
}

// TestReplayIsDeterministic is the property the whole engine rests on: the same
// history replayed twice must give the same answer. If this ever failed, every
// latency number in this package would be measuring something unusable.
func TestReplayIsDeterministic(t *testing.T) {
	d := NewDispatcher()
	s := MustBuildScenario(200)

	first, err := d.Execute(s.Events, ChainWorkflow, s.Input)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := d.Execute(s.Events, ChainWorkflow, s.Input)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != first.Status || got.Blocking != first.Blocking || !got.Now.Equal(first.Now) {
			t.Fatalf("replay %d diverged: status %s/%s, blocking %v/%v, clock %s/%s",
				i, got.Status, first.Status, got.Blocking, first.Blocking, got.Now, first.Now)
		}
	}
}

// ---------------------------------------------------------------------------
// NFR-6: replay latency against history length.
// ---------------------------------------------------------------------------

// BenchmarkReplay measures how long it takes to re-execute a workflow against a
// recorded history, at each of the history lengths the design document names.
//
// The history is built once, outside the timer. That is the correct boundary
// for NFR-6's replay half: in production the events arrive from a single
// indexed range scan, and the question this benchmark answers is what the
// engine does with them afterwards.
func BenchmarkReplay(b *testing.B) {
	d := NewDispatcher()

	for _, n := range HistoryLengths {
		s := MustBuildScenario(n)
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := d.Execute(s.Events, ChainWorkflow, s.Input)
				if err != nil {
					b.Fatal(err)
				}
				if !res.Blocking {
					b.Fatal("expected the replay to suspend")
				}
			}
			// Cost per recorded activity, which is the figure that shows
			// whether growth is linear.
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(s.Activities), "ns/activity")
		})
	}
}

// BenchmarkReplayLongHistories pushes past the specified range, to show where
// the cost would start to matter for a workflow that runs for months.
func BenchmarkReplayLongHistories(b *testing.B) {
	d := NewDispatcher()
	for _, n := range []int{1000, 2000, 5000} {
		s := MustBuildScenario(n)
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := d.Execute(s.Events, ChainWorkflow, s.Input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestNFR6ReplayBudget is the requirement written as a test, so a regression
// fails the build instead of waiting to be noticed in a benchmark run.
//
// NFR-6 says replay latency must stay under 50 ms at 200 events. What is
// measured here is the replay alone, not the database read that precedes it, so
// passing this test is necessary for NFR-6 and not sufficient for it. The
// margin is logged so the report can quote how much headroom the load half has.
func TestNFR6ReplayBudget(t *testing.T) {
	const (
		events = 200
		budget = 50 * time.Millisecond
	)
	d := NewDispatcher()
	s := MustBuildScenario(events)

	// Warm up, so the first run's one-off costs are not the measurement.
	for i := 0; i < 5; i++ {
		if _, err := d.Execute(s.Events, ChainWorkflow, s.Input); err != nil {
			t.Fatal(err)
		}
	}

	const samples = 50
	times := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		if _, err := d.Execute(s.Events, ChainWorkflow, s.Input); err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(start))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })

	p50 := times[len(times)/2]
	p99 := times[int(float64(len(times))*0.99+0.9999)-1]
	worst := times[len(times)-1]

	t.Logf("NFR-6: replay of %d events (%d recorded activities), %d samples",
		events, s.Activities, samples)
	t.Logf("  p50=%v  p99=%v  worst=%v  budget=%v", p50, p99, worst, budget)
	t.Logf("  headroom at p99: %.1fx under budget; the remaining time is the "+
		"database read, which this harness does not measure",
		float64(budget)/float64(p99))

	if p99 > budget {
		t.Fatalf("replay p99 is %v, over the NFR-6 budget of %v before the database "+
			"read has even been counted", p99, budget)
	}
}

// TestReplayCostGrowsLinearly checks the shape of the curve rather than one
// point on it. Replay walks each recorded decision once, so doubling the
// history should roughly double the work. Anything super-linear would mean a
// hidden quadratic — a scan inside a loop — which would only show up as a
// problem on a long-running workflow, exactly the case this engine is for.
func TestReplayCostGrowsLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive; skipped under -short")
	}
	d := NewDispatcher()

	measure := func(events int) time.Duration {
		s := MustBuildScenario(events)
		for i := 0; i < 3; i++ {
			if _, err := d.Execute(s.Events, ChainWorkflow, s.Input); err != nil {
				t.Fatal(err)
			}
		}
		const samples = 30
		times := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			start := time.Now()
			if _, err := d.Execute(s.Events, ChainWorkflow, s.Input); err != nil {
				t.Fatal(err)
			}
			times = append(times, time.Since(start))
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		return times[len(times)/2] // median, to shrug off scheduler noise
	}

	small, large := measure(200), measure(2000)
	ratio := float64(large) / float64(small)
	t.Logf("median replay: 200 events=%v, 2000 events=%v, ratio=%.2fx for 10x the history",
		small, large, ratio)

	// Linear would be about 10x. The bound is generous because this is wall
	// clock on a shared machine; it is set to catch a quadratic (which would be
	// near 100x), not to police a constant factor.
	if ratio > 30 {
		t.Fatalf("replaying 10x the history took %.1fx as long, which suggests the cost "+
			"is worse than linear in history length", ratio)
	}
}
