package taskq

import (
	"strings"
	"testing"
	"time"
)

func TestNextIdle(t *testing.T) {
	tests := []struct {
		name      string
		current   time.Duration
		foundWork bool
		want      time.Duration
	}{
		{"finding work resets to the floor", 800 * time.Millisecond, true, IdleMin},
		{"finding work at the ceiling resets to the floor", IdleMax, true, IdleMin},
		{"an empty poll from the floor doubles", IdleMin, false, 20 * time.Millisecond},
		{"and doubles again", 20 * time.Millisecond, false, 40 * time.Millisecond},
		{"a value below the floor is raised to it", 0, false, IdleMin},
		{"a negative value is raised to the floor", -time.Second, false, IdleMin},
		{"doubling is clamped at the ceiling", 640 * time.Millisecond, false, IdleMax},
		{"the ceiling stays the ceiling", IdleMax, false, IdleMax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextIdle(tc.current, tc.foundWork); got != tc.want {
				t.Fatalf("NextIdle(%v, %v) = %v, want %v", tc.current, tc.foundWork, got, tc.want)
			}
		})
	}
}

// The stated policy is 10ms doubling to a 1s cap. Walking the sequence pins the
// exact ladder rather than just its endpoints.
func TestNextIdleLadder(t *testing.T) {
	want := []time.Duration{
		20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond,
		160 * time.Millisecond, 320 * time.Millisecond, 640 * time.Millisecond,
		IdleMax, IdleMax, IdleMax,
	}
	cur := IdleMin
	for i, w := range want {
		cur = NextIdle(cur, false)
		if cur != w {
			t.Fatalf("step %d: got %v, want %v", i+1, cur, w)
		}
	}
	// One task arriving after a long quiet spell must restore full speed at once,
	// not walk the ladder back down.
	if got := NextIdle(cur, true); got != IdleMin {
		t.Fatalf("reset after work = %v, want %v", got, IdleMin)
	}
}

func TestNextIdleNeverLeavesTheBounds(t *testing.T) {
	cur := IdleMin
	for i := 0; i < 500; i++ {
		cur = NextIdle(cur, i%37 == 0)
		if cur < IdleMin || cur > IdleMax {
			t.Fatalf("iteration %d: idle %v left [%v, %v]", i, cur, IdleMin, IdleMax)
		}
	}
}

func TestPanicErrorNamesTheValue(t *testing.T) {
	e := &PanicError{Value: "boom"}
	if !strings.Contains(e.Error(), "boom") {
		t.Fatalf("PanicError.Error() = %q, want it to mention the panic value", e.Error())
	}
}

// The SQL is a literal in the package, and two things about it are load-bearing
// enough to be worth an assertion that cannot drift silently.
func TestPollSQLKeepsItsGuarantees(t *testing.T) {
	if !strings.Contains(pollSQL, "SKIP LOCKED") {
		t.Fatal("pollSQL lost SKIP LOCKED: every worker would serialise behind one row lock")
	}
	if !strings.Contains(pollSQL, "FOR UPDATE") {
		t.Fatal("pollSQL lost FOR UPDATE: two workers could claim the same task")
	}
	if !strings.Contains(pollSQL, "attempt      = t.attempt + 1") &&
		!strings.Contains(pollSQL, "attempt = attempt + 1") {
		t.Fatal("pollSQL no longer counts attempts, so backoff cannot grow")
	}
	// Regression guard. The obvious spelling of this statement —
	// UPDATE ... WHERE (shard_id, task_id) IN (SELECT ... FOR UPDATE SKIP
	// LOCKED) — silently returns no rows under concurrency, because READ
	// COMMITTED re-runs the uncorrelated subplan during EvalPlanQual and the
	// re-run skips the locks it just lost to. Measured: one poller of four got
	// work, in 12 of 12 rounds. The CTE form does not have this behaviour.
	if strings.Contains(pollSQL, "IN (") && strings.Contains(pollSQL, "SKIP LOCKED") {
		t.Fatal("pollSQL went back to the IN (SELECT ... FOR UPDATE SKIP LOCKED) form, " +
			"which returns no rows under concurrent polling; use the CTE form")
	}
	if !strings.Contains(pollSQL, "WITH pick AS") {
		t.Fatal("pollSQL lost its CTE selection: concurrent pollers will starve")
	}
	// visible_at is honoured through readyAt rather than as its own condition.
	// GREATEST(visible_at, coalesce(locked_until,'-infinity')) <= now() holds
	// only when visible_at <= now(), so a released task still waits out its
	// backoff — but as one range condition on one indexable expression, which
	// is what lets tasks_poll_ready serve the filter and the order together.
	// Spelling it as two conditions is what made the poll cost 95 ms and read
	// 208 MiB of pages at 100,000 rows per shard.
	if !strings.Contains(pollSQL, readyAt+" <= now()") {
		t.Fatal("pollSQL no longer gates on readyAt, so a released task retries instantly " +
			"or a live lock can be stolen")
	}
	if !strings.Contains(pollSQL, "ORDER BY shard_id, task_type, "+readyAt) {
		t.Fatal("pollSQL's ORDER BY no longer matches tasks_poll_ready, so the plan " +
			"gains a Sort over every claimable row")
	}
	// Regression guard for the index, not the lock. The disjunction cannot be
	// served by any index: it forces the lock state onto the heap, so every
	// row under a live lock is fetched and discarded. Measured 95 ms and
	// 26,615 pages for one poll at 100,000 rows per shard, against 0.11 ms and
	// 3 pages for the readyAt form.
	if strings.Contains(pollSQL, "locked_until IS NULL OR") {
		t.Fatal("pollSQL went back to the (locked_until IS NULL OR locked_until < now()) " +
			"form, which no index can serve; use readyAt")
	}
}
