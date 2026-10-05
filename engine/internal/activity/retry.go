// Package activity holds everything the engine needs in order to *decide* how
// an activity should run: the registry of known activities, the retry policy
// that governs a failing one, the deduplication key that is handed to external
// services, and an executor that ties the three together.
//
// The package is deliberately pure. Nothing in here opens a database
// connection, writes an event, or knows that PostgreSQL exists. It takes the
// facts it needs as arguments and returns an outcome; journaling that outcome
// is the caller's job. That separation is what lets the retry maths and the
// dedup key be unit-tested with no infrastructure at all.
package activity

import (
	"errors"
	"math"
	rand "math/rand/v2"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Rand is the only randomness the retry maths needs.
//
// It is an interface, and not a direct call to the global generator, so a test
// can inject a source that always returns the same number and then assert on
// exact delay values. Retry timing is otherwise untestable: you would be
// asserting on a random draw.
type Rand interface {
	// Int64N returns a pseudo-random number in the half-open interval [0, n).
	Int64N(n int64) int64
}

// globalRand adapts the standard library's goroutine-safe top-level generator
// to Rand. It is the default when no source is injected.
type globalRand struct{}

func (globalRand) Int64N(n int64) int64 { return rand.Int64N(n) }

// DefaultRand is the randomness used when a caller does not inject its own.
func DefaultRand() Rand { return globalRand{} }

// RetryPolicy describes how a failing activity is retried.
//
// The delay before attempt N is InitialInterval * BackoffCoefficient^(N-2),
// clamped to MaximumInterval, and then jittered (see NextDelay). Backing off
// geometrically stops a struggling downstream service from being hammered;
// clamping stops the wait from growing without bound on a long-running
// workflow that may legitimately retry for hours.
type RetryPolicy struct {
	// InitialInterval is the delay before the second attempt.
	InitialInterval time.Duration
	// BackoffCoefficient multiplies the delay after every failure. 2.0 doubles.
	BackoffCoefficient float64
	// MaximumInterval caps the delay. Zero or negative means uncapped.
	MaximumInterval time.Duration
	// MaximumAttempts is the total number of attempts, including the first.
	// Zero or negative means retry forever; the surrounding context or the
	// workflow's own timeout is then the only bound.
	MaximumAttempts int
}

// DefaultRetryPolicy is what an activity gets when it declares nothing: start
// at one second, double each time, never wait more than a minute, and give up
// after five attempts. Five attempts spread over roughly a minute and a half
// rides out a restart or a brief network partition without turning a genuine
// bug into an unbounded retry loop.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2.0,
		MaximumInterval:    60 * time.Second,
		MaximumAttempts:    5,
	}
}

// IsZero reports whether the policy was left unset, so a caller can substitute
// the default rather than silently retrying with a zero interval.
func (p RetryPolicy) IsZero() bool { return p == RetryPolicy{} }

// normalized fills in values that make no arithmetic sense. Doing this in one
// place means every other method can assume a sane policy.
func (p RetryPolicy) normalized() RetryPolicy {
	if p.InitialInterval <= 0 {
		p.InitialInterval = DefaultRetryPolicy().InitialInterval
	}
	if p.BackoffCoefficient < 1 {
		// A coefficient below 1 would shrink the delay after each failure,
		// which is the opposite of backing off. Treat it as "constant delay".
		p.BackoffCoefficient = 1
	}
	if p.MaximumInterval <= 0 {
		p.MaximumInterval = time.Duration(math.MaxInt64)
	}
	if p.MaximumInterval < p.InitialInterval {
		p.MaximumInterval = p.InitialInterval
	}
	return p
}

// Unlimited reports whether the policy allows an unbounded number of attempts.
func (p RetryPolicy) Unlimited() bool { return p.MaximumAttempts <= 0 }

// AttemptsRemaining reports whether another attempt is permitted after
// `attempt` attempts have already been made. Attempt numbering is 1-based:
// after the first attempt fails, attempt == 1.
func (p RetryPolicy) AttemptsRemaining(attempt int) bool {
	if p.Unlimited() {
		return true
	}
	return attempt < p.MaximumAttempts
}

// BaseDelay is the un-jittered delay after `attempt` failed attempts. It is
// exported because it is the value a reader can reason about — the geometric
// series before randomness is layered on top — and tests assert on it directly.
func (p RetryPolicy) BaseDelay(attempt int) time.Duration {
	q := p.normalized()
	if attempt < 1 {
		attempt = 1
	}
	d := float64(q.InitialInterval)
	// Multiply in a loop rather than using math.Pow: attempt counts are tiny,
	// and this cannot drift or overflow to +Inf on a large exponent.
	for i := 1; i < attempt; i++ {
		d *= q.BackoffCoefficient
		if d >= float64(q.MaximumInterval) {
			return q.MaximumInterval
		}
	}
	if d >= float64(q.MaximumInterval) {
		return q.MaximumInterval
	}
	return time.Duration(d)
}

// NextDelay returns how long to wait before the attempt following `attempt`,
// and whether another attempt is allowed at all. It uses the shared global
// randomness source; NextDelayWith takes an injected one.
func (p RetryPolicy) NextDelay(attempt int) (time.Duration, bool) {
	return p.NextDelayWith(DefaultRand(), attempt)
}

// NextDelayWith is the pure form of NextDelay: given the same source and the
// same attempt number it always returns the same duration.
//
// The jitter applied is *full* jitter — the delay is drawn uniformly from
// [0, base] rather than from a narrow band around base. When many workers fail
// against the same downstream service at the same moment, a narrow band leaves
// them still roughly synchronised and they retry as a thundering herd. Full
// jitter spreads them across the whole window instead. The trade-off is that
// an individual retry can happen sooner than the nominal backoff suggests,
// which is acceptable because the cap still bounds the worst case.
func (p RetryPolicy) NextDelayWith(r Rand, attempt int) (time.Duration, bool) {
	if !p.AttemptsRemaining(attempt) {
		return 0, false
	}
	base := p.BaseDelay(attempt)
	if base <= 0 {
		return 0, true
	}
	if r == nil {
		return base, true
	}
	// Int64N excludes its bound, so ask for base+1 to make the full interval
	// [0, base] reachable.
	return time.Duration(r.Int64N(int64(base) + 1)), true
}

// ShouldRetry decides whether a failed attempt gets another try.
//
// A core.NonRetryableError short-circuits everything: it means the failure is
// in the request, not in the world, so trying the identical call again can
// only fail identically. Refusing to retry it is what lets a validation error
// reach the workflow quickly, where the saga can compensate the work that
// already succeeded. Looping on it would instead stall the workflow until the
// attempt budget ran out and would delay the rollback by the whole backoff.
func ShouldRetry(err error, attempt int, p RetryPolicy) bool {
	if err == nil {
		return false
	}
	if core.IsNonRetryable(err) {
		return false
	}
	return p.AttemptsRemaining(attempt)
}

// ActivityFailedError is the terminal error an Executor returns when an
// activity has run out of attempts, or failed in a way that must not be
// retried. The original failure is preserved via Unwrap, so errors.As still
// finds a *core.NonRetryableError inside it.
type ActivityFailedError struct {
	ActivityType string
	Attempts     int
	Cause        error
}

func (e *ActivityFailedError) Error() string {
	plural := "attempts"
	if e.Attempts == 1 {
		plural = "attempt"
	}
	return "activity " + e.ActivityType + " failed after " +
		itoa(e.Attempts) + " " + plural + ": " + errText(e.Cause)
}

func (e *ActivityFailedError) Unwrap() error { return e.Cause }

// AttemptCount lets a caller learn how many times the activity actually ran
// without depending on this package's concrete type — the saga package uses a
// one-method interface to read it.
func (e *ActivityFailedError) AttemptCount() int { return e.Attempts }

// NotRegisteredError means the workflow asked for an activity this worker has
// never heard of. It is non-retryable by nature: no amount of waiting will
// make an unregistered name appear in this process.
type NotRegisteredError struct{ ActivityType string }

func (e *NotRegisteredError) Error() string {
	return "activity " + e.ActivityType + " is not registered"
}

// errNilFn guards a Definition that reached the executor with no function in
// it. Registry.Register rejects that, so seeing this means a Definition was
// built by hand and passed around outside the registry.
var errNilFn = errors.New("activity definition has no function")

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// itoa avoids pulling strconv into the error path for one small integer.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
