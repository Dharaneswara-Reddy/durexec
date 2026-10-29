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
	"os"
	"sync"
	"sync/atomic"
	"syscall"

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

	// BeforeResultJournal fires once an activity has RETURNED — so any
	// external effect it performed has already happened and been confirmed by
	// the recipient — and before the transaction that would record that result
	// is begun.
	//
	// It is the one crash point in this package that KILLS THE PROCESS rather
	// than returning an error, and that difference is the whole reason it
	// exists. Every other seam models a crash by abandoning a transaction,
	// which leaves the process and its memory intact; that is enough to test
	// what the database holds, and not enough to test what happens when the
	// worker's knowledge of a completed external call is lost outright. The
	// result at this instant exists ONLY in the dying process's memory: the
	// effect is real and external, and nothing durable records it. Recovery
	// must therefore redeliver the task and re-invoke the activity, and the
	// only thing standing between that and a duplicated effect is the
	// journaled dedup key.
	BeforeResultJournal CrashPoint = "BeforeResultJournal"

	// BeforeStartCommit fires in the transaction that CREATES a workflow,
	// after the execution row, the WorkflowStarted event and the first task
	// have all been staged, and before the commit that would make any of them
	// durable.
	//
	// It is a separate point from BeforeDecisionCommit even though both sit at
	// the same line of eventlog, because a workflow start and a workflow
	// decision fail differently. Abandoning a decision leaves a run that
	// exists and will be retried; abandoning a start must leave NO trace at
	// all — no execution row, no event, no task — and "no trace" is a
	// different assertion from "unchanged". Sharing one point would make a
	// test unable to say which boundary it had actually reached.
	BeforeStartCommit CrashPoint = "BeforeStartCommit"

	// AfterLeaseRenew fires just after a shard lease renewal, used to force an
	// ownership handover while work is in flight.
	//
	// NOT WIRED. No code calls ShouldCrash with this point, so arming it has
	// no effect. It is kept rather than deleted because the constant is the
	// honest record that this seam was intended and is missing; see
	// BeforeNextPlanCompletes for the same situation.
	AfterLeaseRenew CrashPoint = "AfterLeaseRenew"
)

// Wired reports whether some code path actually calls ShouldCrash with this
// point. Arming an unwired point is silently useless, which is exactly the
// kind of "the test passed because it never ran" failure this project has
// been bitten by, so configuration validates against this rather than against
// the full list of declared constants.
func (p CrashPoint) Wired() bool {
	switch p {
	case AfterActivityJournaled, BeforeDecisionCommit, BeforeStartCommit, BeforeResultJournal:
		return true
	default:
		// BeforeNextPlanCompletes is armed only by the in-process agent
		// fixture, never from configuration; AfterLeaseRenew has no caller at
		// all.
		return false
	}
}

// ParseCrashPoint turns configuration text into a crash point, accepting only
// points that are actually wired. The boolean is false for an unknown name and
// for a declared-but-unwired one, so a typo and a dead seam both fail loudly
// at start-up instead of producing a run that quietly never crashes.
func ParseCrashPoint(s string) (CrashPoint, bool) {
	p := CrashPoint(s)
	if !p.Wired() {
		return CrashNone, false
	}
	return p, true
}

// WiredCrashPoints lists the points configuration may arm, for error messages.
func WiredCrashPoints() []string {
	return []string{
		string(AfterActivityJournaled),
		string(BeforeDecisionCommit),
		string(BeforeStartCommit),
		string(BeforeResultJournal),
	}
}

// KillsProcess reports whether arming this point terminates the process rather
// than returning an error.
//
// Callers need to know, because the two are not interchangeable as evidence
// and are not interchangeable operationally either: a point that kills the
// process will be re-armed from configuration when Kubernetes restarts the
// pod, and will then kill it again on the redelivered task. A test that arms
// one MUST disarm it as soon as it has confirmed the first death, or the node
// crash-loops.
func (p CrashPoint) KillsProcess() bool { return p == BeforeResultJournal }

// KillProcessIfArmed terminates this process if point is armed, after calling
// before so the boundary can be named in the log while there is still a
// process to log from.
//
// This is the ONLY place in the repository that deliberately kills the engine,
// deliberately so: one audited function is reviewable in a way that a kill
// inlined at a call site is not. Three gates stand in front of it, all of them
// the same ones that guard every other seam — the injector is inert unless
// FAULT_INJECTION was explicitly set to true, the point must be named in
// FAULT_CRASH_AT, and config.Validate refuses the pair otherwise. There is no
// endpoint, authenticated or otherwise, that can reach it.
//
// SIGKILL to self rather than os.Exit: os.Exit still unwinds through the Go
// runtime's exit path, and this must model a machine stopping. SIGKILL cannot
// be caught, blocked or deferred, so no shutdown hook, no connection drain and
// no buffered write survives it. That is the point.
//
// Returns false when nothing was armed, in which case before is NOT called.
func (i *Injector) KillProcessIfArmed(point CrashPoint, before func()) bool {
	if !i.ShouldCrash(point) {
		return false
	}
	if before != nil {
		before()
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	// Unreachable in practice. If a platform ever failed to deliver the
	// signal, returning true still tells the caller not to proceed as though
	// the crash had not been requested.
	return true
}

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
