package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
	"github.com/dharaneswar339/durexec/internal/taskq"
)

// The tests in this file cover two liveness defects. Neither could corrupt
// anything — no event was ever written twice, no side effect ever happened
// twice — and that is exactly why both survived every safety test in the
// repository. What they did was make a healthy-looking node stop working.
//
//	1. A lost race for a sequence number was reported as lost shard ownership,
//	   which retired the worker for good. Four of them retired a shard.
//	2. A task claim lasts TASK_LOCK. Nothing renewed it, so an activity that
//	   ran longer had its claim lapse and a second worker ran it concurrently.

// ---- 1: a sequence race must not retire the worker ------------------------

func seqRace() error {
	return &eventlog.SeqRaceError{Shard: 1, Run: "run-1", Seq: 2}
}

// TestLostOwnershipIsTwoDifferentThings is the core of the first fix. Both
// errors satisfy errors.Is(err, core.ErrLostOwnership) — that compatibility is
// deliberate — so the only thing separating them is the type, and the worker
// has to act on the type.
func TestLostOwnershipIsTwoDifferentThings(t *testing.T) {
	tests := []struct {
		name      string
		commitErr error

		// wantStop is whether the worker should end its loop. Only a genuine
		// fencing rejection means this node really lost the shard.
		wantStop bool
		// wantReleases is whether the task was handed back for another try.
		wantReleases int
	}{
		{
			name:      "a fencing rejection means we lost the shard, so stop",
			commitErr: &eventlog.FencedError{Shard: 1, Stored: 9, Held: 7},
			wantStop:  true, wantReleases: 0,
		},
		{
			name:      "a sequence race means we still own it, so keep going",
			commitErr: seqRace(),
			wantStop:  false, wantReleases: 1,
		},
		{
			// A bare wrapped sentinel is still read conservatively: we cannot
			// tell what happened, so stopping is the safe reading.
			name:      "an untyped lost-ownership error is still treated as fenced",
			commitErr: core.ErrLostOwnership,
			wantStop:  true, wantReleases: 0,
		},
	}

	for _, tc := range tests {
		t.Run("workflow worker: "+tc.name, func(t *testing.T) {
			e, commits, queue := engineForWorker(t, startedHistory("TwoStep"), runningExecution("TwoStep", 1))
			commits.err = tc.commitErr

			w := &WorkflowWorker{Engine: e, Shard: 1}
			err := w.process(context.Background(), workflowTask())

			if got := errors.Is(err, errShardGone); got != tc.wantStop {
				t.Errorf("worker stopped = %v, want %v (err %v)", got, tc.wantStop, err)
			}
			if got := queue.releaseCount(); got != tc.wantReleases {
				t.Errorf("releases = %d, want %d", got, tc.wantReleases)
			}
		})

		t.Run("activity worker: "+tc.name, func(t *testing.T) {
			e, commits, queue := activityEngine(t,
				func(context.Context, json.RawMessage) (json.RawMessage, error) {
					return json.RawMessage(`"ok"`), nil
				}, activity.DefaultRetryPolicy())
			commits.err = tc.commitErr

			w := &ActivityWorker{Engine: e, Shard: 1}
			err := w.process(context.Background(), activityTask(2))

			if got := errors.Is(err, errShardGone); got != tc.wantStop {
				t.Errorf("worker stopped = %v, want %v (err %v)", got, tc.wantStop, err)
			}
			if got := queue.releaseCount(); got != tc.wantReleases {
				t.Errorf("releases = %d, want %d", got, tc.wantReleases)
			}
		})
	}
}

// TestTheShardKeepsMakingProgressAfterSequenceRaces is the defect itself,
// reproduced at the level it actually hurt: the worker LOOP, not one call.
//
// Four lost races used to be enough to retire all four of a shard's workflow
// workers. Here one worker loses four in a row and must still be polling
// afterwards, and the work must still get done.
func TestTheShardKeepsMakingProgressAfterSequenceRaces(t *testing.T) {
	e, commits, queue := engineForWorker(t, startedHistory("TwoStep"), runningExecution("TwoStep", 1))
	// The fake queue makes a released task immediately pollable again, so the
	// loop re-polls at once and the four lost races take milliseconds.
	queue.requeueOnRelease = true
	queue.tasks = []core.Task{workflowTask()}
	commits.failTimes(4, seqRace())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		(&WorkflowWorker{Engine: e, Shard: 1}).Run(ctx)
		close(done)
	}()

	// The fifth attempt is the one that lands.
	deadline := time.After(5 * time.Second)
	for commits.commitCount() == 0 {
		select {
		case <-done:
			t.Fatalf("the worker returned after %d lost races, having committed nothing: "+
				"a sequence race is being treated as a lost shard", queue.releaseCount())
		case <-deadline:
			t.Fatalf("no commit after 5s; %d releases, %d commits",
				queue.releaseCount(), commits.commitCount())
		case <-time.After(time.Millisecond):
		}
	}

	if got := queue.releaseCount(); got < 4 {
		t.Errorf("the task was handed back %d times, want at least 4 (one per lost race)", got)
	}

	// Still running: nothing about a lost race should end the loop.
	select {
	case <-done:
		t.Fatal("the worker returned instead of continuing to poll")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not return after its context was cancelled")
	}
}

// ---- 1b: the pool must be able to put a shard back to strength -------------

// poolEngine builds an engine whose workers exit on their first task, because
// the node is told it does not hold the shard. That is the cheapest way to get
// workers that have genuinely returned from Run.
func poolEngine(t *testing.T, owns *atomic.Bool) (*Engine, *fakeQueue) {
	t.Helper()
	e, _, queue := engineForWorker(t, startedHistory("TwoStep"), runningExecution("TwoStep", 1))
	e.TokenFor = func(core.ShardID) (core.FencingToken, bool) { return 7, owns.Load() }
	return e, queue
}

func waitForWorkers(t *testing.T, p *WorkerPool, shard core.ShardID, wantWf, wantAct int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		wf, act := p.LiveWorkers(shard)
		if wf == wantWf && act == wantAct {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("shard %d has %d workflow and %d activity workers, want %d and %d",
				shard, wf, act, wantWf, wantAct)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestPoolReplacesWorkersThatHaveExited covers the second half of the first
// defect. Even with the sequence race fixed, a worker that returns for any
// other reason used to be gone for good: the shard stayed in the pool's running
// map, so every later Start saw "already running" and started nothing. A shard
// could therefore sit in the running set with no workers in it, reporting
// healthy and holding its lease while doing no work at all.
func TestPoolReplacesWorkersThatHaveExited(t *testing.T) {
	var owns atomic.Bool
	e, queue := poolEngine(t, &owns)
	queue.tasks = []core.Task{workflowTask(), workflowTask(), activityTask(2), activityTask(2)}

	p := NewWorkerPool(e, 2, 2)
	defer p.StopAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.Start(ctx, 1)
	// Every worker finds a task, is told the shard is not ours, and returns.
	waitForWorkers(t, p, 1, 0, 0)

	// The pool still believes it is running this shard, which is the trap.
	if owned := p.Owned(); len(owned) != 1 || owned[0] != 1 {
		t.Fatalf("Owned() = %v, want shard 1 still in the running set", owned)
	}

	// Now the node does hold the shard, and one Start must put it back to
	// strength rather than returning early.
	owns.Store(true)
	p.Start(ctx, 1)
	waitForWorkers(t, p, 1, 2, 2)
}

// TestPoolDoesNotReviveAShardTheLeaseManagerTookAway is the other half of that
// change, and the one that matters for safety: supervision must never be able
// to restart a shard this node legitimately lost. It cannot, because Supervise
// only ever calls Start for shards the lease manager still reports as owned.
func TestPoolDoesNotReviveAShardTheLeaseManagerTookAway(t *testing.T) {
	var owns atomic.Bool
	owns.Store(true)
	e, _ := poolEngine(t, &owns)

	p := NewWorkerPool(e, 2, 1)
	defer p.StopAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// owned is the lease manager's answer, and it is the only input Supervise
	// has. It is read from the supervisor's goroutine and changed from this
	// one, so it is guarded.
	var mu sync.Mutex
	ownedSet := map[core.ShardID]core.FencingToken{1: 7}
	owned := func() map[core.ShardID]core.FencingToken {
		mu.Lock()
		defer mu.Unlock()
		return ownedSet
	}
	go p.Supervise(ctx, owned, 5*time.Millisecond)

	waitForWorkers(t, p, 1, 2, 1)

	// The lease is lost: the manager drops it from the owned set and calls Stop.
	mu.Lock()
	ownedSet = map[core.ShardID]core.FencingToken{}
	mu.Unlock()
	p.Stop(1)

	waitForWorkers(t, p, 1, 0, 0)
	// Several supervision ticks later it must still be empty.
	time.Sleep(50 * time.Millisecond)
	if wf, act := p.LiveWorkers(1); wf != 0 || act != 0 {
		t.Fatalf("a shard this node lost was restarted with %d workflow and %d activity workers", wf, act)
	}
	if owned := p.Owned(); len(owned) != 0 {
		t.Fatalf("Owned() = %v, want nothing: the shard was taken away", owned)
	}
}

// ---- 2: an activity must keep its claim while it runs ---------------------

// TestClaimRenewalEveryFollowsTheLockDuration pins the one thing that must not
// become a magic number: the heartbeat interval is a fraction of the configured
// lock, so shortening TASK_LOCK shortens the heartbeat with it.
func TestClaimRenewalEveryFollowsTheLockDuration(t *testing.T) {
	tests := []struct {
		lock time.Duration
		want time.Duration
	}{
		{lock: 30 * time.Second, want: 10 * time.Second},
		{lock: 90 * time.Second, want: 30 * time.Second},
		{lock: 3 * time.Second, want: time.Second},
		// Floored, so a misconfigured lock cannot turn the heartbeat into a
		// busy loop against PostgreSQL.
		{lock: time.Millisecond, want: minClaimRenewal},
	}
	for _, tc := range tests {
		if got := claimRenewalEvery(tc.lock); got != tc.want {
			t.Errorf("claimRenewalEvery(%s) = %s, want %s", tc.lock, got, tc.want)
		}
	}
	// And the default is whatever the queue's default lock is, never zero,
	// because a zero interval would be a ticker panic.
	e := &Engine{}
	if got, want := e.lockDuration(), taskq.DefaultConfig().LockDuration; got != want {
		t.Errorf("unset TaskLock = %s, want the queue default %s", got, want)
	}
}

// slowActivityEngine wires an activity that runs for d, under a lock of lock.
func slowActivityEngine(t *testing.T, lock, d time.Duration) (*Engine, *fakeCommitter, *fakeQueue) {
	t.Helper()
	e, commits, queue := activityEngine(t,
		func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			select {
			case <-time.After(d):
				return json.RawMessage(`"slow but done"`), nil
			case <-ctx.Done():
				// Cancellation is how the worker tells an activity to stop
				// because the claim is gone.
				return nil, ctx.Err()
			}
		}, activity.DefaultRetryPolicy())
	e.TaskLock = lock
	return e, commits, queue
}

// TestLongActivityRenewsItsClaim is the second defect. Queue.Extend existed for
// exactly this and nothing outside the tests ever called it, so an activity
// that outlived TASK_LOCK became claimable again and a second worker ran it
// concurrently — on a completely healthy node, which is what makes it worse
// than it sounds for a project about long-running workflows.
func TestLongActivityRenewsItsClaim(t *testing.T) {
	// 60ms lock means a renewal every 20ms; a 200ms activity should see
	// several. The numbers are small so the test is fast; the ratio is the
	// thing being tested.
	e, commits, queue := slowActivityEngine(t, 60*time.Millisecond, 200*time.Millisecond)

	w := &ActivityWorker{Engine: e, Shard: 1}
	if err := w.process(context.Background(), activityTask(2)); err != nil {
		t.Fatalf("process: %v", err)
	}

	// Without renewal the claim would have lapsed at 60ms, two thirds of the
	// way through the activity, and a second worker could have claimed the row.
	if got := queue.extendCount(); got < 3 {
		t.Errorf("the claim was renewed %d times during a 200ms activity under a 60ms lock, want at least 3: "+
			"an activity that outlives its claim is run again by a second worker", got)
	}
	if commits.commitCount() != 1 {
		t.Errorf("commits = %d, want 1: the activity finished and its result must be journaled",
			commits.commitCount())
	}
}

// TestActivityStopsAndCommitsNothingWhenItsClaimIsLost covers the other half:
// if renewal is refused the row is no longer ours, another worker may already
// be running the same activity, and the one thing this worker must not do is
// commit a result it is not entitled to journal.
func TestActivityStopsAndCommitsNothingWhenItsClaimIsLost(t *testing.T) {
	e, commits, queue := slowActivityEngine(t, 30*time.Millisecond, 10*time.Second)
	queue.extendErr = taskq.ErrLockLost

	w := &ActivityWorker{Engine: e, Shard: 1}

	done := make(chan error, 1)
	go func() { done <- w.process(context.Background(), activityTask(2)) }()

	select {
	case err := <-done:
		// Losing a task claim is not losing the shard: the worker must stay in
		// its loop and poll for other work.
		if err != nil {
			t.Fatalf("process returned %v, want nil: a lost task claim is not a lost shard", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the activity was not stopped when its claim was refused; " +
			"the context handed to it is not being cancelled")
	}

	if commits.commitCount() != 0 {
		t.Error("a result was committed for a task this worker no longer held")
	}
	if queue.releaseCount() != 0 {
		t.Error("the task must be left alone, not released: its lock has already lapsed " +
			"and Release would be refused anyway")
	}
}

// A transient renewal failure is not a lost claim. Giving up an activity
// because one UPDATE failed would turn a database hiccup into abandoned work,
// which is why only taskq.ErrLockLost is treated as definitive.
func TestTransientRenewalFailureDoesNotAbandonTheActivity(t *testing.T) {
	e, commits, queue := slowActivityEngine(t, 30*time.Millisecond, 120*time.Millisecond)
	queue.extendErr = errBoom

	w := &ActivityWorker{Engine: e, Shard: 1}
	if err := w.process(context.Background(), activityTask(2)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if queue.extendCount() < 2 {
		t.Errorf("renewal was attempted %d times, want at least 2: a failed renewal must be retried", queue.extendCount())
	}
	if commits.commitCount() != 1 {
		t.Errorf("commits = %d, want 1: the activity finished and its result must still be journaled",
			commits.commitCount())
	}
}

// The heartbeat must not outlive the activity it was renewing. There is no way
// to observe a leaked goroutine directly from here, so this asserts the
// property that makes the leak impossible: settleClaim waits for the goroutine,
// so no renewal can arrive after process has returned.
func TestClaimHeartbeatStopsWithTheActivity(t *testing.T) {
	e, _, queue := slowActivityEngine(t, 30*time.Millisecond, 50*time.Millisecond)

	w := &ActivityWorker{Engine: e, Shard: 1}
	if err := w.process(context.Background(), activityTask(2)); err != nil {
		t.Fatalf("process: %v", err)
	}
	after := queue.extendCount()
	time.Sleep(100 * time.Millisecond) // several renewal intervals
	if got := queue.extendCount(); got != after {
		t.Errorf("the claim was renewed %d more times after the activity finished: the heartbeat goroutine leaked",
			got-after)
	}
}
