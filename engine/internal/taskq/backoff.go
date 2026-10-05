package taskq

import (
	"math"
	"math/rand"
	"time"
)

// Backoff is exponential backoff with full jitter. The ceiling doubles per
// attempt and the actual delay is drawn uniformly from [0, ceiling).
//
// Full jitter rather than plain exponential because every worker in the fleet
// fails the same task at the same time when a downstream dependency is down.
// Without jitter they would all retry in lockstep and rebuild the thundering
// herd on every cycle. Drawing from the whole interval spreads them out.
type Backoff struct {
	Base   time.Duration
	Factor float64
	Cap    time.Duration
}

// DefaultBackoff is base 100ms, factor 2, cap 60s.
func DefaultBackoff() Backoff {
	return Backoff{Base: 100 * time.Millisecond, Factor: 2, Cap: 60 * time.Second}
}

// withDefaults fills zero fields so a zero-value Backoff still behaves.
func (b Backoff) withDefaults() Backoff {
	d := DefaultBackoff()
	if b.Base <= 0 {
		b.Base = d.Base
	}
	if b.Factor < 1 {
		b.Factor = d.Factor
	}
	if b.Cap <= 0 {
		b.Cap = d.Cap
	}
	if b.Cap < b.Base {
		b.Cap = b.Base
	}
	return b
}

// Ceiling is the upper bound of the jitter window for an attempt number.
// Attempt is 1-based: the first retry uses Base. It is pure and monotonically
// non-decreasing in attempt, which is the property the tests assert.
func (b Backoff) Ceiling(attempt int) time.Duration {
	b = b.withDefaults()
	if attempt < 1 {
		attempt = 1
	}
	// math.Pow on a float is used rather than repeated shifting so a non-integer
	// factor stays available; the +inf guard covers a large attempt count.
	scaled := float64(b.Base) * math.Pow(b.Factor, float64(attempt-1))
	if math.IsInf(scaled, 0) || scaled >= float64(b.Cap) {
		return b.Cap
	}
	return time.Duration(scaled)
}

// Jitter is the pure form of the delay: rnd must be in [0,1) and the result is
// rnd fraction of the ceiling. Kept separate from any RNG so a table-driven
// test can pin the exact output without seeding anything.
func (b Backoff) Jitter(attempt int, rnd float64) time.Duration {
	if rnd < 0 {
		rnd = 0
	}
	if rnd >= 1 {
		rnd = math.Nextafter(1, 0)
	}
	return time.Duration(rnd * float64(b.Ceiling(attempt)))
}

// Delay draws one jittered delay from r. r must not be nil.
func (b Backoff) Delay(attempt int, r *rand.Rand) time.Duration {
	return b.Jitter(attempt, r.Float64())
}
