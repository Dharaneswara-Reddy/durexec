package taskq

import (
	"math/rand"
	"testing"
	"time"
)

func TestBackoffCeiling(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Factor: 2, Cap: 60 * time.Second}
	tests := []struct {
		name    string
		attempt int
		want    time.Duration
	}{
		{"attempt zero is treated as the first", 0, 100 * time.Millisecond},
		{"negative attempt is treated as the first", -5, 100 * time.Millisecond},
		{"first retry uses base", 1, 100 * time.Millisecond},
		{"second doubles", 2, 200 * time.Millisecond},
		{"third doubles again", 3, 400 * time.Millisecond},
		{"tenth", 10, 51200 * time.Millisecond},
		{"eleventh is capped", 11, 60 * time.Second},
		{"a huge attempt cannot overflow past the cap", 1000, 60 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.Ceiling(tc.attempt); got != tc.want {
				t.Fatalf("Ceiling(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}
}

// The ceiling must never shrink as attempts pile up, and must never exceed the
// cap. Those two together are the whole contract the retry loop relies on.
func TestBackoffCeilingGrowsAndIsBounded(t *testing.T) {
	b := DefaultBackoff()
	prev := time.Duration(0)
	for attempt := 1; attempt <= 200; attempt++ {
		got := b.Ceiling(attempt)
		if got < prev {
			t.Fatalf("Ceiling(%d) = %v shrank from %v", attempt, got, prev)
		}
		if got > b.Cap {
			t.Fatalf("Ceiling(%d) = %v exceeds cap %v", attempt, got, b.Cap)
		}
		if got <= 0 {
			t.Fatalf("Ceiling(%d) = %v, want positive", attempt, got)
		}
		prev = got
	}
	if prev != b.Cap {
		t.Fatalf("ceiling never reached the cap: %v", prev)
	}
}

func TestBackoffWithDefaults(t *testing.T) {
	d := DefaultBackoff()
	tests := []struct {
		name string
		in   Backoff
		want Backoff
	}{
		{"zero value takes every default", Backoff{}, d},
		{"factor below one is replaced", Backoff{Base: time.Second, Factor: 0.5, Cap: time.Minute},
			Backoff{Base: time.Second, Factor: 2, Cap: time.Minute}},
		{"a cap below the base is raised to the base", Backoff{Base: 5 * time.Second, Factor: 2, Cap: time.Second},
			Backoff{Base: 5 * time.Second, Factor: 2, Cap: 5 * time.Second}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.withDefaults(); got != tc.want {
				t.Fatalf("withDefaults() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBackoffJitterIsAFractionOfTheCeiling(t *testing.T) {
	b := Backoff{Base: time.Second, Factor: 2, Cap: time.Minute}
	tests := []struct {
		name    string
		attempt int
		rnd     float64
		want    time.Duration
	}{
		{"zero draws no delay at all", 1, 0, 0},
		{"half of the first ceiling", 1, 0.5, 500 * time.Millisecond},
		{"half of the second ceiling", 2, 0.5, time.Second},
		{"a quarter", 3, 0.25, time.Second},
		{"a negative draw is clamped to zero", 1, -1, 0},
		{"a draw of one is pulled just below the ceiling", 1, 1, time.Second - 1},
		{"a draw above one is clamped the same way", 1, 5, time.Second - 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := b.Jitter(tc.attempt, tc.rnd)
			// The clamped cases land one nanosecond short of the ceiling, which is
			// the point: the delay is drawn from [0, ceiling), never the ceiling.
			if got != tc.want {
				t.Fatalf("Jitter(%d, %v) = %v, want %v", tc.attempt, tc.rnd, got, tc.want)
			}
		})
	}
}

// Seeded so the assertion is about the bounds and the spread, not about luck.
func TestBackoffDelayStaysInsideTheWindow(t *testing.T) {
	b := DefaultBackoff()
	r := rand.New(rand.NewSource(1))

	distinct := map[time.Duration]struct{}{}
	for i := 0; i < 2000; i++ {
		attempt := i%12 + 1
		got := b.Delay(attempt, r)
		if got < 0 || got >= b.Ceiling(attempt) {
			t.Fatalf("Delay(%d) = %v, outside [0, %v)", attempt, got, b.Ceiling(attempt))
		}
		distinct[got] = struct{}{}
	}
	// Full jitter exists to break up lockstep retries. If every worker drew the
	// same value the herd would survive, so the spread is the property to check.
	if len(distinct) < 1000 {
		t.Fatalf("only %d distinct delays in 2000 draws; jitter is not spreading", len(distinct))
	}
}

func TestIntervalLiteral(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0 milliseconds"},
		{-time.Second, "0 milliseconds"},
		{time.Millisecond, "1 milliseconds"},
		{30 * time.Second, "30000 milliseconds"},
		{90 * time.Minute, "5400000 milliseconds"},
		// Sub-millisecond durations truncate to zero, which is honest: the column
		// has no finer resolution to offer.
		{500 * time.Microsecond, "0 milliseconds"},
	}
	for _, tc := range tests {
		if got := interval(tc.in); got != tc.want {
			t.Errorf("interval(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseToken(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty is rejected", "", true},
		{"garbage is rejected", "not-a-uuid", true},
		{"a valid uuid passes", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", false},
		{"case is normalised", "3F2504E0-4F89-11D3-9A0C-0305E82C3301", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseToken(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseToken(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err == nil && got != "3f2504e0-4f89-11d3-9a0c-0305e82c3301" {
				t.Fatalf("parseToken(%q) = %q, want the canonical lowercase form", tc.in, got)
			}
		})
	}
}

func TestConfigWithDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.LockDuration != DefaultConfig().LockDuration {
		t.Fatalf("LockDuration = %v, want the default", got.LockDuration)
	}
	if got.Backoff != DefaultBackoff() {
		t.Fatalf("Backoff = %+v, want the default", got.Backoff)
	}
	custom := Config{LockDuration: 5 * time.Second}.withDefaults()
	if custom.LockDuration != 5*time.Second {
		t.Fatalf("an explicit LockDuration was overwritten: %v", custom.LockDuration)
	}
}

// TestRenewTimeoutIsNeverTighterThanAQueryTakes pins the bound that broke an
// integration test when it was first written. RenewTimeout exists so a blocked
// renewal cannot silently eat the attempts that cover a slow one — but a bound
// that fires on healthy behaviour is not a margin, it is a new failure mode. A
// ninth of a 300ms lock is 33ms, which a localhost query under a parallel test
// run exceeds easily.
func TestRenewTimeoutIsNeverTighterThanAQueryTakes(t *testing.T) {
	cases := []struct {
		lock time.Duration
		want time.Duration
	}{
		// The production default: a third of the lock, so a timed-out renewal
		// costs exactly one of the three attempts the lock lifetime allows.
		{30 * time.Second, 10 * time.Second},
		{3 * time.Second, time.Second},
		// Short locks are floored, because below the floor the bound would
		// fire on a healthy query rather than on a stall.
		{300 * time.Millisecond, minRenewTimeout},
		{30 * time.Millisecond, minRenewTimeout},
	}
	for _, c := range cases {
		got := Config{LockDuration: c.lock}.withDefaults().RenewTimeout
		if got != c.want {
			t.Errorf("a %s lock gave a renew timeout of %s, want %s", c.lock, got, c.want)
		}
		if got < minRenewTimeout {
			t.Errorf("a %s lock gave %s, below the %s floor", c.lock, got, minRenewTimeout)
		}
	}

	// An explicit value is honoured, but still floored.
	if got := (Config{LockDuration: time.Minute, RenewTimeout: 2 * time.Second}).
		withDefaults().RenewTimeout; got != 2*time.Second {
		t.Errorf("an explicit renew timeout became %s, want 2s", got)
	}
	if got := (Config{LockDuration: time.Minute, RenewTimeout: time.Millisecond}).
		withDefaults().RenewTimeout; got != minRenewTimeout {
		t.Errorf("an explicit 1ms renew timeout became %s, want the %s floor", got, minRenewTimeout)
	}
}
