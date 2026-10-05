// Package faultinj is the deliberate-failure switch used by the demonstration
// applications and by the CI smoke stage.
//
// Three properties matter more than features here:
//
//  1. It must be impossible to fire in production. An Injector that was not
//     explicitly enabled by configuration ignores every arming call and every
//     Check returns nil, so a stray FailNext left in code is inert.
//  2. It must cost nothing when nothing is armed. The hot path is a single
//     atomic load; the mutex is only taken once a fault is actually waiting.
//  3. It must be exact. FailNext(t, 3) fails three times and then stops, so a
//     test can assert a count rather than a vague "it failed sometimes".
package faultinj

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/dharaneswar339/durexec/internal/core"
)

// CrashPoint names a place in the engine where a process may be told to die.
// These are the seams the evaluation needs, not every seam that exists: each
// one sits immediately after a durable write and immediately before the next
// one, which is where a crash is most likely to expose a correctness bug.
type CrashPoint string

const (
	// CrashNone is the zero value and never fires.
	CrashNone CrashPoint = ""

	// AfterActivityJournaled fires once an activity result is durably written
	// but before the workflow has made its next decision. Recovery must replay
	// the journaled result and must not run the activity a second time.
	AfterActivityJournaled CrashPoint = "AfterActivityJournaled"

	// BeforeNextPlanCompletes fires while the agent's next planning call is in
	// flight, so that plan is lost. This is the crash that produces the
	// double-booking scenario: on recovery the planner re-decides from the
	// transcript and may phrase the same intent differently.
	BeforeNextPlanCompletes CrashPoint = "BeforeNextPlanCompletes"

	// BeforeDecisionCommit fires after a workflow task has computed its
	// decision but before the transaction that would make it durable.
	BeforeDecisionCommit CrashPoint = "BeforeDecisionCommit"

	// AfterLeaseRenew fires just after a shard lease renewal, used to force an
	// ownership handover while work is in flight.
	AfterLeaseRenew CrashPoint = "AfterLeaseRenew"
)

// ErrInjected is the root cause of every injected activity failure. Callers
// should test with errors.Is rather than reading the message.
var ErrInjected = errors.New("fault injected")

// Fault is the error handed back by Check. It carries the activity type and
// how many injected failures remain so a demo UI can display the state.
type Fault struct {
	ActivityType string
	Remaining    int
}

func (f *Fault) Error() string {
	return fmt.Sprintf("injected failure for activity %q (%d remaining)", f.ActivityType, f.Remaining)
}

func (f *Fault) Unwrap() error { return ErrInjected }

// IsInjected reports whether err originated in the fault injector. Production
// code paths use this to keep injected failures out of real error metrics.
func IsInjected(err error) bool { return errors.Is(err, ErrInjected) }

type armedFault struct {
	remaining int
	fatal     bool
}

// Injector is safe for concurrent use by any number of workers.
//
// A nil *Injector is a valid disabled injector, so callers that have no
// injector at all can pass nil instead of threading an option through.
type Injector struct {
	// enabled is fixed at construction. Nothing can turn an Injector on later;
	// that is the guarantee that a real deployment cannot be made to inject.
	enabled bool

	// armed is the fast path. When it is false — the only state a production
	// process is ever in — Check does one atomic load and returns.
	armed atomic.Bool

	mu      sync.Mutex
	fails   map[string]*armedFault
	crashes map[CrashPoint]int

	fired atomic.Int64
}

// New returns an Injector. When enabled is false the returned Injector is
// permanently inert: arming calls are dropped and Check always returns nil.
// The flag comes from config.Config.FaultInjection, which itself refuses to be
// true unless the environment set it explicitly.
func New(enabled bool) *Injector {
	return &Injector{
		enabled: enabled,
		fails:   make(map[string]*armedFault),
		crashes: make(map[CrashPoint]int),
	}
}

// Disabled returns an injector that can never fire. Useful as a default.
func Disabled() *Injector { return New(false) }

// Enabled reports whether this injector was switched on by configuration.
func (i *Injector) Enabled() bool { return i != nil && i.enabled }

// Armed reports whether any fault is currently waiting to fire.
func (i *Injector) Armed() bool { return i != nil && i.armed.Load() }

// Fired returns the total number of faults this injector has produced, which
// is what the demo surfaces and what the smoke test asserts on.
func (i *Injector) Fired() int64 {
	if i == nil {
		return 0
	}
	return i.fired.Load()
}

// FailNext arms the next `times` invocations of activityType to fail with a
// retryable error, then stop. Times of zero or less disarms that activity.
func (i *Injector) FailNext(activityType string, times int) {
	i.arm(activityType, times, false)
}

// FailNextFatal is FailNext with a non-retryable error, which is what the
// delivery demo's "fail this step" button uses: a retryable failure would just
// be retried into success and the saga compensation would never be shown.
func (i *Injector) FailNextFatal(activityType string, times int) {
	i.arm(activityType, times, true)
}

func (i *Injector) arm(activityType string, times int, fatal bool) {
	if !i.Enabled() || activityType == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if times <= 0 {
		delete(i.fails, activityType)
	} else {
		i.fails[activityType] = &armedFault{remaining: times, fatal: fatal}
	}
	i.refreshArmedLocked()
}

// CrashAt arms a single crash at point. The engine calls ShouldCrash at that
// seam; consuming the crash disarms it so recovery is not an infinite loop of
// dying at the same place.
func (i *Injector) CrashAt(point CrashPoint) {
	if !i.Enabled() || point == CrashNone {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.crashes[point]++
	i.refreshArmedLocked()
}

// Check is called by an activity implementation immediately before it would do
// real work. It returns nil unless a fault is armed for that activity type.
func (i *Injector) Check(activityType string) error {
	// Fast path: one atomic load, no lock, no map lookup, no allocation.
	if i == nil || !i.armed.Load() {
		return nil
	}

	i.mu.Lock()
	f, ok := i.fails[activityType]
	if !ok {
		i.mu.Unlock()
		return nil
	}
	f.remaining--
	fatal := f.fatal
	remaining := f.remaining
	if f.remaining <= 0 {
		delete(i.fails, activityType)
	}
	i.refreshArmedLocked()
	i.mu.Unlock()

	i.fired.Add(1)
	fault := &Fault{ActivityType: activityType, Remaining: remaining}

	// Wrapping in the core taxonomy lets the existing retry policy decide what
	// to do, so injected failures travel the same path as real ones.
	if fatal {
		return &core.NonRetryableError{Cause: fault}
	}
	return &core.RetryableError{Cause: fault}
}

// ShouldCrash reports whether a crash is armed at point and consumes it.
func (i *Injector) ShouldCrash(point CrashPoint) bool {
	if i == nil || !i.armed.Load() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	n, ok := i.crashes[point]
	if !ok || n <= 0 {
		return false
	}
	n--
	if n == 0 {
		delete(i.crashes, point)
	} else {
		i.crashes[point] = n
	}
	i.refreshArmedLocked()
	i.fired.Add(1)
	return true
}

// Reset disarms everything without resetting the fired counter, so a demo can
// be re-run from a clean state.
func (i *Injector) Reset() {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	clear(i.fails)
	clear(i.crashes)
	i.refreshArmedLocked()
}

// refreshArmedLocked keeps the lock-free fast-path flag in step with the maps.
// Callers must hold i.mu.
func (i *Injector) refreshArmedLocked() {
	i.armed.Store(len(i.fails) > 0 || len(i.crashes) > 0)
}
