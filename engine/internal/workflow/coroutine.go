package workflow

import (
	"errors"
	"fmt"
	"runtime/debug"
)

// ---------------------------------------------------------------------------
// The cooperative handshake
// ---------------------------------------------------------------------------
//
// Workflow code looks like ordinary blocking Go: you call an activity, you call
// Get on the future it returns, and execution appears to stop until the answer
// comes back. In reality the answer may not exist for another three weeks, and
// the process running the workflow will very likely have been restarted several
// times before it does. So "stop" cannot mean "block an OS thread" — a thousand
// sleeping workflows would cost a thousand parked threads and would not survive
// a restart anyway.
//
// Instead the workflow body runs on its own goroutine and is *suspended*: it is
// parked on a channel receive, the dispatcher collects the commands it produced
// so far, and the goroutine is then thrown away entirely. When the answer
// finally arrives, the workflow is re-run from its very first line against a
// longer history, and it races back to the same suspension point using recorded
// results instead of real calls. Nothing about the goroutine is persisted; the
// history in PostgreSQL is the only durable state.
//
// The handshake below is what makes that suspension exact rather than
// approximate. There are two unbuffered channels:
//
//	resume  dispatcher -> workflow   "you have control, run"
//	yield   workflow   -> dispatcher "I have given control back"
//
// An unbuffered channel send cannot complete until a receiver is already
// waiting. That single property is what buys determinism:
//
//   - The dispatcher's step() sends on resume. That send cannot finish until the
//     workflow goroutine is sitting at its matching receive. The instant it
//     finishes, the dispatcher moves straight to <-yield and blocks there.
//   - The workflow goroutine runs until it either finishes or parks. Parking
//     sends on yield, which cannot finish until the dispatcher is sitting at its
//     matching receive — which it is.
//
// So control is handed over, never shared. At any instant exactly one of the two
// goroutines is doing work and the other is blocked on a channel. There is no
// mutex, because there is nothing to contend for; there is no dependence on how
// the Go scheduler feels about running two runnable goroutines, because the two
// are never runnable at the same time. Re-running the same workflow against the
// same history therefore produces the same commands in the same order, every
// time, on any machine.
//
// (Between the send on yield and the following receive on resume the workflow
// goroutine is technically still executing a couple of instructions. It touches
// no shared state in that window — the two statements are adjacent inside park()
// — so the guarantee that matters, "no two goroutines observe or mutate workflow
// state at once", still holds.)
//
// Channel operations also give us memory ordering for free: everything the
// workflow goroutine wrote before its send on yield is guaranteed visible to the
// dispatcher after its receive. That is why the result fields below need no
// locking and why the package is clean under `go test -race`.
//
// One more channel, kill, exists purely to dispose of a suspended workflow. When
// a workflow blocks on an activity that will not complete for three weeks, its
// goroutine is parked forever and would leak. destroy() closes kill; every park
// point selects on it and panics with a private sentinel, which unwinds the
// workflow's call stack through all its defers and lets the goroutine exit. The
// sentinel is swallowed by the recover in run(), so it never reaches the caller.

// errAbandoned is panicked into a parked workflow goroutine to unwind it. It is
// never returned to a caller: run() recognises it and exits quietly.
var errAbandoned = errors.New("workflow coroutine abandoned")

// abortPanic carries a real error out of deep inside workflow code without
// waiting for the workflow to check a return value. It is used for divergence:
// once history and code disagree there is nothing sensible left to execute, and
// letting the workflow run on would produce commands derived from a history we
// have already proved does not describe this code.
type abortPanic struct{ err error }

// PanicError wraps a panic raised by workflow code. Workflow code runs on a
// goroutine of our making, so an unrecovered panic there would take the whole
// worker process down and, worse, would take down every unrelated workflow that
// happened to share it. We convert it into an ordinary workflow failure and keep
// the stack, because that stack is the only clue anyone will get.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("workflow panicked: %v", e.Value)
}

// coroutine owns the workflow goroutine and the two-channel handshake.
type coroutine struct {
	resume chan struct{} // dispatcher -> workflow
	yield  chan struct{} // workflow -> dispatcher
	kill   chan struct{} // closed by destroy to unwind a suspended workflow
	exited chan struct{} // closed when the goroutine has actually returned

	// The three fields below are written by the workflow goroutine and read by
	// the dispatcher. They are safe without a mutex only because every write
	// happens-before the send on yield that hands control back, and every read
	// happens-after the matching receive.
	finished bool
	result   []byte
	err      error
}

func newCoroutine() *coroutine {
	return &coroutine{
		resume: make(chan struct{}),
		yield:  make(chan struct{}),
		kill:   make(chan struct{}),
		exited: make(chan struct{}),
	}
}

// start launches body on its own goroutine and leaves it parked before the
// first statement. Nothing of the workflow runs until step() is called, so the
// dispatcher can finish preparing state without racing the workflow.
func (c *coroutine) start(body func() ([]byte, error)) {
	go c.run(body)
}

func (c *coroutine) run(body func() ([]byte, error)) {
	// Registered first so it runs last: the dispatcher's destroy() waits on
	// exited, and by the time it is closed every other defer has completed and
	// every write to the shared fields has happened.
	defer close(c.exited)

	// Park before the first line of workflow code. Symmetric with the second
	// half of park().
	select {
	case <-c.resume:
	case <-c.kill:
		return
	}

	defer func() {
		switch r := recover().(type) {
		case nil:
			// normal return
		case error:
			if errors.Is(r, errAbandoned) {
				// Deliberate teardown by destroy(). The dispatcher is not
				// waiting for a handover and must not be sent one.
				return
			}
			c.err = &PanicError{Value: r, Stack: debug.Stack()}
		case *abortPanic:
			c.err = r.err
		default:
			c.err = &PanicError{Value: r, Stack: debug.Stack()}
		}

		c.finished = true
		// Final handover. Symmetric with the first half of park(). The select
		// on kill is defensive: the dispatcher is always at <-yield here, but a
		// bare send would deadlock forever if it ever were not.
		select {
		case c.yield <- struct{}{}:
		case <-c.kill:
		}
	}()

	c.result, c.err = body()
}

// step hands control to the workflow and blocks until the workflow hands it
// back, either by parking on an unresolved future or by returning. It is called
// exactly once per workflow task, because a task ends the first time the
// workflow cannot make progress.
func (c *coroutine) step() {
	c.resume <- struct{}{}
	<-c.yield
}

// park suspends the workflow goroutine and gives control to the dispatcher.
// Called only from Future.Get, and only when the future has no recorded answer.
func (c *coroutine) park() {
	select {
	case c.yield <- struct{}{}:
	case <-c.kill:
		panic(errAbandoned)
	}
	select {
	case <-c.resume:
	case <-c.kill:
		panic(errAbandoned)
	}
}

// destroy disposes of the goroutine and waits for it to be gone. It is
// idempotent in effect but must be called exactly once, which the dispatcher
// guarantees with a defer. Waiting on exited rather than firing and forgetting
// means a test can assert there are no stray goroutines, and means the workflow
// state object is provably not being touched any more once Execute returns.
func (c *coroutine) destroy() {
	close(c.kill)
	<-c.exited
}
