package runtime

// Regression tests for finding F-08b: graceful shutdown cancelled in-flight
// activities instead of draining them.
//
// The shutdown ORDERING was always right and deliberately so — HTTP stop, then
// drain, then release leases, so no shard is handed over while this node is
// still writing. What was wrong was the middle step. StopAll cancelled each
// shard's context, and that is the context the activity itself runs under:
// Executor.Execute returned at its ctx.Err() check and the task was handed
// back. So every in-flight activity was RE-INVOKED after the restart, on every
// rolling deployment rather than only on a crash, and the comment on StopAll
// claimed the opposite ("a worker that is mid-decision finishes its commit").
//
// At-least-once invocation permits it. The objection is that it was spending
// the engine's one real guarantee routinely and for no reason, with nothing
// downstream absorbing the repeat.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
)

// blockingActivity is an activity that reports when it starts, waits to be
// released, and reports how it ended. It is the only way to tell a drain from
// a cancel: both leave the process quiet, and the difference is whether the
// activity got to finish.
type blockingActivity struct {
	started   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	finished  int
	cancelled int
}

func (b *blockingActivity) definition() activity.Definition {
	return activity.Definition{
		Name: "SlowExternalCall",
		Mode: core.ModeExact,
		Retry: activity.RetryPolicy{
			InitialInterval: time.Millisecond, BackoffCoefficient: 1,
			MaximumInterval: time.Millisecond, MaximumAttempts: 1,
		},
		Fn: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			select {
			case b.started <- struct{}{}:
			default:
			}
			select {
			case <-b.release:
				b.mu.Lock()
				b.finished++
				b.mu.Unlock()
				return json.RawMessage(`{"done":true}`), nil
			case <-ctx.Done():
				b.mu.Lock()
				b.cancelled++
				b.mu.Unlock()
				return nil, ctx.Err()
			}
		},
	}
}

func (b *blockingActivity) counts() (finished, cancelled int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.finished, b.cancelled
}

// drainHarness is a real worker pool over the harness's shard, with one
// activity that can be held mid-call.
type drainHarness struct {
	*effectHarness
	act  *blockingActivity
	pool *WorkerPool
}

func newDrainHarness(t *testing.T) *drainHarness {
	t.Helper()
	// Reuse the external-effect harness for its real queue, real store and a
	// seeded ACTIVITY task, then swap the registry for a holdable activity.
	h := newEffectHarness(t, extsimQuiet())

	act := &blockingActivity{
		started: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	reg := activity.NewRegistry()
	if err := reg.Register(act.definition()); err != nil {
		t.Fatalf("register: %v", err)
	}
	h.engine.Activities = reg
	h.engine.Executor = activity.NewExecutor(reg)

	// Re-point the seeded task at the holdable activity.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE events SET payload = jsonb_set(payload, '{activity_type}', '"SlowExternalCall"')
		  WHERE shard_id = $1 AND workflow_id = $2 AND event_type = 'ActivityTaskScheduled'`,
		int32(h.shard), h.workflowID); err != nil {
		t.Fatalf("re-point the scheduled activity: %v", err)
	}

	p := NewWorkerPool(h.engine, 1, 1)
	t.Cleanup(func() { p.StopAllWithin(0) })
	return &drainHarness{effectHarness: h, act: act, pool: p}
}

// awaitStart waits for the activity to be running, so the test is never racing
// the worker's first poll.
func (d *drainHarness) awaitStart(t *testing.T) {
	t.Helper()
	select {
	case <-d.act.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the activity never started; the worker pool is not polling")
	}
}

// THE REGRESSION TEST. A drain lets the in-flight activity finish and commit.
func TestShutdownDrainsAnInFlightActivityInsteadOfCancellingIt(t *testing.T) {
	d := newDrainHarness(t)
	ctx := context.Background()

	d.pool.Start(ctx, d.shard)
	d.awaitStart(t)

	// Shutdown begins while the activity is mid-call. Nothing may cancel it.
	done := make(chan struct{})
	go func() {
		d.pool.StopAllWithin(10 * time.Second)
		close(done)
	}()

	// Give the drain a moment to stop polling, then prove the activity is
	// still alive by letting it finish.
	time.Sleep(100 * time.Millisecond)
	if _, cancelled := d.act.counts(); cancelled != 0 {
		t.Fatalf("the activity was cancelled %d time(s) by the drain; it must be left to finish", cancelled)
	}
	close(d.act.release)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("StopAllWithin did not return after the activity finished")
	}

	finished, cancelled := d.act.counts()
	if finished != 1 || cancelled != 0 {
		t.Fatalf("activity finished=%d cancelled=%d, want 1 and 0", finished, cancelled)
	}

	// The result was committed, which is what "drained" has to mean: the task
	// is gone and the workflow has its answer. Under the old cancel-based
	// shutdown the task was released instead and the activity re-invoked.
	completed, failed, _ := d.journaled(t)
	if completed != 1 || failed != 0 {
		t.Fatalf("engine journaled completed=%d failed=%d, want 1 and 0 — the drain did not commit the result",
			completed, failed)
	}
	if got := d.shape(t, d.workflowID); got.tasks != 1 {
		// One task: the WORKFLOW wake-up written in the same transaction as
		// the result. The ACTIVITY task was deleted by that commit.
		t.Fatalf("%d task(s) after the drain, want 1 (the workflow wake-up)", got.tasks)
	}
}

// A drain must stop polling IMMEDIATELY, or shutdown would keep picking up new
// work and never converge.
func TestADrainStopsPollingAtOnce(t *testing.T) {
	d := newDrainHarness(t)
	ctx := context.Background()
	close(d.act.release) // activities complete instantly in this test

	d.pool.Start(ctx, d.shard)
	d.awaitStart(t)

	// Wait for the first task to be fully committed, then seed several more
	// and drain. None of the new ones may be taken.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if completed, _, _ := d.journaled(t); completed == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	before, _ := d.act.counts()
	d.pool.StopAllWithin(5 * time.Second)

	// Now add work that nothing should touch: the pool is stopped.
	d.seedExtraActivityTasks(t, 3)
	time.Sleep(200 * time.Millisecond)

	after, _ := d.act.counts()
	if after != before {
		t.Fatalf("the activity ran %d more time(s) after the drain; polling did not stop", after-before)
	}
	if got := d.shape(t, d.workflowID); got.tasks < 3 {
		t.Fatalf("%d task(s) left; the 3 seeded tasks must be untouched for the next node", got.tasks)
	}
}

// An expired budget falls back to cancelling, because a node that will not shut
// down is worse than an abandoned task — and the fallback must be LOUD, since
// anything cut off here is re-invoked after the restart.
func TestAnExpiredDrainBudgetFallsBackToCancelling(t *testing.T) {
	d := newDrainHarness(t)
	ctx := context.Background()

	d.pool.Start(ctx, d.shard)
	d.awaitStart(t)

	start := time.Now()
	d.pool.StopAllWithin(250 * time.Millisecond) // the activity never releases
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("StopAllWithin took %s; an expired budget must not wait indefinitely", elapsed)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("StopAllWithin returned in %s, before the budget expired: it did not drain at all", elapsed)
	}
	_, cancelled := d.act.counts()
	if cancelled != 1 {
		t.Fatalf("activity cancelled %d time(s), want 1 once the budget expired", cancelled)
	}

	// And the task is still there for another node, which is why cancelling is
	// survivable at all: at-least-once invocation under the same dedup key.
	if got := d.shape(t, d.workflowID); got.tasks != 1 {
		t.Fatalf("%d task(s) after a cancelled shutdown, want the activity task left behind", got.tasks)
	}
	if completed, _, _ := d.journaled(t); completed != 0 {
		t.Fatal("a cancelled activity must not have journaled a result")
	}
}

// Losing a lease must still cancel at once. Draining there would be wrong: the
// shard belongs to another node now, every write would be refused by the
// fencing check anyway, and holding on would only delay the new owner.
func TestLosingAShardStillCancelsImmediately(t *testing.T) {
	d := newDrainHarness(t)
	ctx := context.Background()

	d.pool.Start(ctx, d.shard)
	d.awaitStart(t)

	start := time.Now()
	d.pool.Stop(d.shard)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, cancelled := d.act.counts(); cancelled == 1 {
			t.Logf("ownership loss cancelled the in-flight activity in %s", time.Since(start))
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("losing the shard did not cancel the in-flight activity; a lost lease must stop work at once")
}
