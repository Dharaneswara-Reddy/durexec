package workflow

import (
	"encoding/json"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Context is the entire world a workflow function can see.
//
// The interface is deliberately tiny, and every method on it is deliberately
// incapable of touching anything outside the process. There is no HTTP client,
// no database handle, no time.Now, no channel. If a workflow could reach the
// outside world directly, the second execution of that workflow would see a
// different world and would take different decisions from the first, which is
// precisely the thing the whole engine exists to prevent. So the rule is: the
// workflow decides what should happen, and an activity is the only thing
// allowed to make it happen.
type Context interface {
	// ExecuteActivity asks for a unit of real work. The Future is returned
	// immediately and does not block, so a workflow can start several
	// activities and only then wait for them.
	ExecuteActivity(activityType string, input any) Future

	// Sleep is a durable sleep. It is a row in the timers table, not a
	// goroutine, so a three-week sleep costs nothing and survives every
	// restart in between.
	Sleep(d time.Duration) Future

	// WaitForSignal blocks until a signal of this name is delivered to the run.
	// The Future resolves to the signal's payload.
	WaitForSignal(name string) Future

	// SideEffect runs a small non-deterministic snippet exactly once and
	// records its result. Use it for a UUID or a random choice — things that
	// are not worth an activity but must not change on replay. On every
	// re-execution the recorded value is returned and fn is not called again.
	SideEffect(fn func() any) Future

	// Now is the workflow's clock. It is derived from history, never from
	// time.Now, so two executions of the same run see the same time.
	Now() time.Time
}

// Future is the promise of an answer that may not exist yet.
type Future interface {
	// Get returns the recorded answer, or suspends the workflow if there is
	// none yet. Suspension does not block an OS thread: the workflow goroutine
	// hands control back to the dispatcher and is then discarded, and the
	// workflow is re-executed from the top once the answer has been journaled.
	Get() (json.RawMessage, error)
}

// resolvedFuture is a future whose answer is already in history. Get returns
// immediately and the workflow never yields, which is what makes replaying a
// long history fast: it is a straight-line run through recorded values.
type resolvedFuture struct {
	value json.RawMessage
	err   error
}

func (f resolvedFuture) Get() (json.RawMessage, error) { return f.value, f.err }

// pendingFuture is a future with no answer in history. Get suspends the
// workflow, and in practice never returns: the dispatcher ends the workflow
// task at that point and destroys the goroutine. The loop is written anyway so
// the code stays correct if a future ever becomes resolvable mid-task.
type pendingFuture struct {
	co       *coroutine
	resolved bool
	value    json.RawMessage
	err      error
}

func newPendingFuture(co *coroutine) *pendingFuture { return &pendingFuture{co: co} }

func (f *pendingFuture) Get() (json.RawMessage, error) {
	for !f.resolved {
		f.co.park()
	}
	return f.value, f.err
}

// ---------------------------------------------------------------------------

// wfContext is the live state of one execution of one workflow. It is created
// fresh for every workflow task and thrown away with the goroutine, because the
// durable state of a workflow is its history and nothing else.
//
// It is not safe for concurrent use, and it does not need to be: the handshake
// in coroutine.go guarantees that only one goroutine is ever running.
type wfContext struct {
	co   *coroutine
	hist *history

	// k is the program-order counter described in history.go. It is bumped
	// exactly once per blocking call, before anything else can fail, so that a
	// call which errors still consumes its slot and every later call keeps the
	// number it had on the previous execution.
	k int

	// sigCursor tracks how many signals of each name have already been
	// consumed, so two WaitForSignal calls for the same name see the first and
	// the second delivery rather than both seeing the first.
	sigCursor map[string]int

	commands []core.Command
	now      time.Time

	// modeFor lets the worker stamp each scheduled activity with its declared
	// idempotency mode. It is a function rather than a registry because this
	// package must not depend on the activity package, and because it must be
	// a pure lookup — calling out to anything with I/O here would break replay.
	modeFor func(activityType string) core.IdempotencyMode
}

func newWFContext(co *coroutine, h *history, modeFor func(string) core.IdempotencyMode) *wfContext {
	return &wfContext{
		co:        co,
		hist:      h,
		sigCursor: make(map[string]int),
		now:       h.start,
		modeFor:   modeFor,
	}
}

// next allocates the k for the blocking call that is about to be made.
func (c *wfContext) next() int {
	k := c.k
	c.k++
	return k
}

// advance moves the workflow clock forward to the timestamp of an event being
// replayed. Time in a workflow only moves when history moves: between two
// recorded events the workflow's Now is frozen, so a computation like
// "deadline := ctx.Now().Add(time.Hour)" yields the same instant on the first
// execution and on the tenth. It never moves backwards, so a workflow can rely
// on Now being monotonic even if two events share a timestamp.
func (c *wfContext) advance(t time.Time) {
	if t.After(c.now) {
		c.now = t
	}
}

func (c *wfContext) Now() time.Time { return c.now }

// diverge aborts the whole execution. There is no attempt to repair anything:
// the engine has two accounts of what this workflow did and no way to tell
// which is true, so guessing could double-charge a customer or skip a refund.
// Refusing loudly is the only honest option.
func (c *wfContext) diverge(k int, e *histEntry, requested string) {
	panic(&abortPanic{err: &core.NonDeterminismError{
		Seq:       e.seq,
		K:         k,
		Recorded:  e.signature,
		Requested: requested,
	}})
}

// replay is the shared shape of every blocking call.
//
// If history has an entry at this k, we are in REPLAY MODE: the answer (or the
// fact that the work is still in flight) comes from the log and no command is
// emitted, because emitting one would schedule work that has already been
// scheduled. If it does not, we are in NEW MODE: the caller emits a command and
// gets a future that will not resolve until a later workflow task.
//
// The bool reports whether replay handled the call.
func (c *wfContext) replay(k int, requested string) (Future, bool) {
	e, ok := c.hist.entry(k)
	if !ok {
		return nil, false
	}
	if e.signature != "" && e.signature != requested {
		c.diverge(k, e, requested)
	}
	c.advance(e.at)
	if e.resolved {
		return resolvedFuture{value: e.value, err: e.err}, true
	}
	// Scheduled, but no result yet. The activity task or timer row already
	// exists, so we must block WITHOUT emitting a second command.
	return newPendingFuture(c.co), true
}

func (c *wfContext) ExecuteActivity(activityType string, input any) Future {
	k := c.next()
	if f, done := c.replay(k, sigActivity(activityType)); done {
		return f
	}

	raw, err := marshalInput(input)
	if err != nil {
		// A value the workflow cannot even serialise is a bug in the workflow,
		// not a transient fault. Hand the error back through the future so the
		// workflow's own error handling (usually a saga compensation) runs, and
		// emit no command. k has still been consumed, so this stays
		// deterministic on every later replay.
		return resolvedFuture{err: err}
	}

	c.commands = append(c.commands, core.Command{
		Type:         core.CmdScheduleActivity,
		K:            k,
		ActivityType: activityType,
		Input:        raw,
		Mode:         c.mode(activityType),
		// DedupKey is left empty on purpose. It is derived from the workflow
		// and run identifiers, which this package deliberately does not know,
		// and BuildDecision fills it in.
	})
	return newPendingFuture(c.co)
}

func (c *wfContext) mode(activityType string) core.IdempotencyMode {
	if c.modeFor == nil {
		return core.ModeExact
	}
	m := c.modeFor(activityType)
	if m == "" {
		return core.ModeExact
	}
	return m
}

func (c *wfContext) Sleep(d time.Duration) Future {
	k := c.next()
	if f, done := c.replay(k, sigTimer); done {
		return f
	}
	if d <= 0 {
		// A zero or negative sleep is not worth a timer row and a whole extra
		// workflow task. It still consumes its k, so replay stays aligned.
		return resolvedFuture{value: jsonNull}
	}
	c.commands = append(c.commands, core.Command{
		Type:     core.CmdStartTimer,
		K:        k,
		Duration: d,
	})
	return newPendingFuture(c.co)
}

func (c *wfContext) WaitForSignal(name string) Future {
	k := c.next()

	// A signal wait leaves no record of its own at this k, so the only entry we
	// could find here belongs to a different kind of call — which is a
	// divergence, and replay reports it.
	if e, ok := c.hist.entry(k); ok && e.signature != "" && e.signature != sigSignal(name) {
		c.diverge(k, e, sigSignal(name))
	}

	// Signals are matched by name in delivery order rather than by k. See the
	// long note in indexHistory.
	queue := c.hist.signals[name]
	if idx := c.sigCursor[name]; idx < len(queue) {
		c.sigCursor[name] = idx + 1
		rec := queue[idx]
		c.advance(rec.at)
		value := rec.payload
		if len(value) == 0 {
			value = jsonNull
		}
		return resolvedFuture{value: value}
	}

	// The command is a declaration of interest, not something to journal, and
	// it is re-emitted on every workflow task until the signal actually
	// arrives, because history holds no "started waiting" event to suppress it.
	// The committer must therefore treat CmdWaitSignal as idempotent.
	c.commands = append(c.commands, core.Command{
		Type:   core.CmdWaitSignal,
		K:      k,
		Signal: name,
	})
	return newPendingFuture(c.co)
}

func (c *wfContext) SideEffect(fn func() any) Future {
	k := c.next()
	if f, done := c.replay(k, sigSideEffect); done {
		// The recorded value is returned and fn is NOT called. That is the
		// whole point of a side effect: the random number, UUID or clock read
		// is taken once, written to the log, and reused forever after.
		return f
	}

	var value any
	if fn != nil {
		value = fn()
	}
	raw, err := marshalInput(value)
	if err != nil {
		return resolvedFuture{err: err}
	}

	c.commands = append(c.commands, core.Command{
		Type:  core.CmdRecordSideEffect,
		K:     k,
		Value: raw,
	})
	// Resolved immediately, so a side effect never costs an extra workflow
	// task. That is safe because the command is committed in the same
	// transaction as everything else this task produced: by the time any of
	// this task's decisions are durable, the recorded value is durable too.
	return resolvedFuture{value: raw}
}

// marshalInput turns a workflow's argument into the bytes that go into history.
// A json.RawMessage passes through untouched, which is what lets a workflow
// forward an activity's result to the next activity byte-for-byte.
func marshalInput(v any) (json.RawMessage, error) {
	if v == nil {
		return jsonNull, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return jsonNull, nil
		}
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// SagaRunner adapts a Context to the tiny function shape the saga package
// expects. It lives here rather than in the saga package so that saga stays
// independent of this one: saga only ever sees a function.
//
// Running a compensation through ExecuteActivity is what makes rollback as
// durable as the forward path — it is journaled, retried and crash-safe in
// exactly the same way, rather than being a best-effort call in a defer.
func SagaRunner(ctx Context) func(activityType string, input any) (json.RawMessage, error) {
	return func(activityType string, input any) (json.RawMessage, error) {
		return ctx.ExecuteActivity(activityType, input).Get()
	}
}
