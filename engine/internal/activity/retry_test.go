package activity

import (
	"errors"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// maxRand always returns the top of the range it is given, which turns full
// jitter into "wait the whole base delay". That makes the geometric series
// observable exactly, instead of as a distribution.
type maxRand struct{}

func (maxRand) Int64N(n int64) int64 { return n - 1 }

// zeroRand always returns the bottom of the range: the shortest wait full
// jitter can produce.
type zeroRand struct{}

func (zeroRand) Int64N(int64) int64 { return 0 }

func TestBaseDelayGrowsGeometricallyAndCaps(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2,
		MaximumInterval:    10 * time.Second,
		MaximumAttempts:    0, // unlimited, so the cap is what we are testing
	}

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 10 * time.Second}, // 16s would exceed MaximumInterval
		{6, 10 * time.Second},
		{20, 10 * time.Second}, // and stays there rather than drifting
	}
	for _, c := range cases {
		if got := p.BaseDelay(c.attempt); got != c.want {
			t.Errorf("BaseDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestNextDelayFullJitterBounds(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2,
		MaximumInterval:    10 * time.Second,
		MaximumAttempts:    5,
	}

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
	}
	for _, c := range cases {
		// Full jitter draws uniformly from [0, base]. With a source pinned to
		// the top of the range the delay is exactly the base, and with one
		// pinned to the bottom it is exactly zero. Asserting both ends pins the
		// interval without ever asserting on a random draw.
		got, ok := p.NextDelayWith(maxRand{}, c.attempt)
		if !ok {
			t.Fatalf("attempt %d: policy refused a retry it should allow", c.attempt)
		}
		if got != c.want {
			t.Errorf("NextDelayWith(max, %d) = %v, want %v", c.attempt, got, c.want)
		}
		if got, _ := p.NextDelayWith(zeroRand{}, c.attempt); got != 0 {
			t.Errorf("NextDelayWith(zero, %d) = %v, want 0", c.attempt, got)
		}
	}

	// The budget is five attempts, so after the fifth failure there is nothing
	// left to schedule.
	if _, ok := p.NextDelayWith(maxRand{}, 5); ok {
		t.Error("a fifth failure under a five-attempt budget must not schedule a sixth")
	}
}

func TestNextDelayCapAppliesToJitteredValueToo(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 10,
		MaximumInterval:    5 * time.Second,
	}
	// Attempt 3 would nominally be 100s; the cap must bite before the jitter,
	// so the largest possible draw is the cap itself.
	if got, _ := p.NextDelayWith(maxRand{}, 3); got != 5*time.Second {
		t.Fatalf("NextDelayWith = %v, want the capped 5s", got)
	}
}

func TestUnlimitedAndDegeneratePolicies(t *testing.T) {
	unlimited := RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2}
	if !unlimited.Unlimited() {
		t.Fatal("a zero MaximumAttempts means retry forever")
	}
	if _, ok := unlimited.NextDelayWith(maxRand{}, 1000); !ok {
		t.Fatal("an unlimited policy must always allow another attempt")
	}

	// A coefficient below 1 would shrink the delay after each failure, which is
	// the opposite of backing off. It is clamped to a constant delay.
	shrinking := RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 0.5}
	for attempt := 1; attempt <= 4; attempt++ {
		if got := shrinking.BaseDelay(attempt); got != time.Second {
			t.Fatalf("BaseDelay(%d) = %v, want a constant 1s", attempt, got)
		}
	}

	// An unset policy must not produce a zero delay, which would be a tight
	// retry loop against a struggling service.
	var zero RetryPolicy
	if !zero.IsZero() {
		t.Fatal("a zero-valued policy must report IsZero")
	}
	if got := zero.BaseDelay(1); got != DefaultRetryPolicy().InitialInterval {
		t.Fatalf("BaseDelay on an unset policy = %v, want the default interval", got)
	}
}

func TestShouldRetry(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2,
		MaximumAttempts:    3,
	}
	boom := errors.New("connection reset")

	cases := []struct {
		name    string
		err     error
		attempt int
		want    bool
	}{
		{"success is not retried", nil, 1, false},
		{"transient failure, budget left", boom, 1, true},
		{"transient failure, budget left", boom, 2, true},
		{"budget exhausted", boom, 3, false},
		{"budget overrun", boom, 4, false},
		{
			// The important one. A non-retryable failure means the request is
			// wrong, not the world, so the identical call can only fail
			// identically. Refusing on the very first attempt is what lets the
			// workflow start compensating immediately instead of waiting out
			// the whole backoff for an answer that will not change.
			"non-retryable on attempt 1",
			&core.NonRetryableError{Cause: errors.New("card number invalid")},
			1,
			false,
		},
		{
			"non-retryable stays non-retryable when wrapped",
			&RetryableError{Cause: &core.NonRetryableError{Cause: boom}},
			1,
			false,
		},
		{
			"explicitly retryable is retried",
			&core.RetryableError{Cause: boom},
			1,
			true,
		},
	}

	for _, c := range cases {
		if got := ShouldRetry(c.err, c.attempt, p); got != c.want {
			t.Errorf("%s: ShouldRetry(attempt=%d) = %v, want %v", c.name, c.attempt, got, c.want)
		}
	}
}

// RetryableError is a local wrapper used only to prove that errors.As still
// finds a core.NonRetryableError buried inside another error.
type RetryableError struct{ Cause error }

func (e *RetryableError) Error() string { return e.Cause.Error() }
func (e *RetryableError) Unwrap() error { return e.Cause }
