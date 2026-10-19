package taskq

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Skipped unless DUREXEC_TEST_DSN names a PostgreSQL that already has the schema
// applied, so the unit tests above still run on a machine with no database.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DUREXEC_TEST_DSN")
	if dsn == "" {
		t.Skip("set DUREXEC_TEST_DSN to run the PostgreSQL tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func freshShard(t *testing.T, pool *pgxpool.Pool) core.ShardID {
	t.Helper()
	shard := core.ShardID(200000 + rand.Int31n(1000000))
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tasks WHERE shard_id = $1", int32(shard))
	}
	clean()
	t.Cleanup(clean)
	return shard
}

func TestPollLocksOneTaskAndThenFindsNothing(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	q := New(pool, Config{LockDuration: 30 * time.Second})
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	if err := q.Enqueue(ctx, []core.Task{{ShardID: shard, Type: core.TaskWorkflow, WorkflowID: "wf", RunID: run}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := q.Poll(ctx, shard, core.TaskWorkflow)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.WorkflowID != "wf" || got.Type != core.TaskWorkflow {
		t.Fatalf("polled the wrong task: %+v", got)
	}
	if got.LockToken == "" {
		t.Fatal("poll returned no lock token")
	}
	if got.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 after the first poll", got.Attempt)
	}

	// The row is locked now, so a second poll must find nothing rather than hand
	// the same work to a second worker.
	if _, err := q.Poll(ctx, shard, core.TaskWorkflow); !errors.Is(err, core.ErrNoTask) {
		t.Fatalf("second poll: got %v, want ErrNoTask", err)
	}
	// And a poll of the other queue must not see it either.
	if _, err := q.Poll(ctx, shard, core.TaskActivity); !errors.Is(err, core.ErrNoTask) {
		t.Fatalf("cross-type poll: got %v, want ErrNoTask", err)
	}
}

func TestReleaseAppliesAVisibilityDelay(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	q := New(pool, Config{LockDuration: 30 * time.Second})
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	if err := q.Enqueue(ctx, []core.Task{{ShardID: shard, Type: core.TaskWorkflow, WorkflowID: "wf", RunID: run}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, err := q.Poll(ctx, shard, core.TaskWorkflow)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	// A generous delay: the point is that a failed task does not become instantly
	// pollable and spin the fleet.
	if err := q.Release(ctx, shard, task.TaskID, task.LockToken, 60_000); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := q.Poll(ctx, shard, core.TaskWorkflow); !errors.Is(err, core.ErrNoTask) {
		t.Fatalf("poll after a delayed release: got %v, want ErrNoTask", err)
	}

	// The old token must not work any more.
	if err := q.Extend(ctx, shard, task.TaskID, task.LockToken); !errors.Is(err, ErrLockLost) {
		t.Fatalf("extend after release: got %v, want ErrLockLost", err)
	}
}

func TestExtendAndComplete(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	q := New(pool, Config{LockDuration: 2 * time.Second})
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	if err := q.Enqueue(ctx, []core.Task{{ShardID: shard, Type: core.TaskActivity, WorkflowID: "wf", RunID: run, ScheduledEventSeq: 4}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, err := q.Poll(ctx, shard, core.TaskActivity)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if task.ScheduledEventSeq != 4 {
		t.Fatalf("scheduled_event_seq = %d, want 4", task.ScheduledEventSeq)
	}
	if err := q.Extend(ctx, shard, task.TaskID, task.LockToken); err != nil {
		t.Fatalf("extend: %v", err)
	}
	if err := q.Complete(ctx, shard, task.TaskID, task.LockToken); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Completing twice must fail loudly: the row is gone.
	if err := q.Complete(ctx, shard, task.TaskID, task.LockToken); !errors.Is(err, ErrLockLost) {
		t.Fatalf("second complete: got %v, want ErrLockLost", err)
	}
}

// TestARenewedClaimIsNeverHandedToASecondWorker is the real-database proof of
// the second liveness defect, and of the fix.
//
// The lock is deliberately short — 300ms — and the "activity" runs for four
// times that. The left half of the test is what the engine used to do: nothing
// renews the claim, so the lock lapses while the work is still running and a
// second poll takes the row. Two workers then run the same activity at once,
// and when the second one commits it collides with the first on a sequence
// number, which is the other defect in this pair. The right half renews on a
// third of the lock, exactly as ActivityWorker.keepClaimed does, and the second
// poll finds nothing for as long as the work goes on.
func TestARenewedClaimIsNeverHandedToASecondWorker(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const (
		lock = 300 * time.Millisecond
		work = 4 * lock
	)

	// Left half: no renewal. This is the bug, reproduced.
	t.Run("without renewal the claim lapses and a second worker takes the task", func(t *testing.T) {
		shard := freshShard(t, pool)
		q := New(pool, Config{LockDuration: lock})
		seedOne(t, q, shard)

		first, err := q.Poll(ctx, shard, core.TaskActivity)
		if err != nil {
			t.Fatalf("first poll: %v", err)
		}
		time.Sleep(work) // the activity is still running

		second, err := q.Poll(ctx, shard, core.TaskActivity)
		if err != nil {
			t.Fatalf("second poll: %v, want the same task handed out again", err)
		}
		if second.TaskID != first.TaskID {
			t.Fatalf("second poll got task %d, want the same row %d", second.TaskID, first.TaskID)
		}
		// Two workers, one task, both believing they hold it. This is the
		// concurrent execution the heartbeat exists to prevent.
		if second.LockToken == first.LockToken {
			t.Fatal("the second poll reused the first lock token")
		}
	})

	// Right half: renewed on a third of the lock, as the activity worker does.
	t.Run("a renewed claim is still ours when the work finishes", func(t *testing.T) {
		shard := freshShard(t, pool)
		q := New(pool, Config{LockDuration: lock})
		seedOne(t, q, shard)

		task, err := q.Poll(ctx, shard, core.TaskActivity)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}

		stop := make(chan struct{})
		renewed := make(chan error, 1)
		go func() {
			t := time.NewTicker(lock / 3)
			defer t.Stop()
			for {
				select {
				case <-stop:
					renewed <- nil
					return
				case <-t.C:
				}
				if err := q.Extend(ctx, shard, task.TaskID, task.LockToken); err != nil {
					renewed <- err
					return
				}
			}
		}()

		// Poll repeatedly for the whole time the work is running: at no point
		// may the row be handed to anyone else.
		deadline := time.Now().Add(work)
		for time.Now().Before(deadline) {
			if _, err := q.Poll(ctx, shard, core.TaskActivity); !errors.Is(err, core.ErrNoTask) {
				close(stop)
				t.Fatalf("a renewed task was handed to a second worker: %v", err)
			}
			time.Sleep(lock / 4)
		}

		close(stop)
		if err := <-renewed; err != nil {
			t.Fatalf("renewal failed while the work was still running: %v", err)
		}
		// And the claim is still ours at the end, so the result can be
		// committed and the row deleted under our own token.
		if err := q.Complete(ctx, shard, task.TaskID, task.LockToken); err != nil {
			t.Fatalf("complete after renewals: %v", err)
		}
	})
}

// seedOne enqueues a single activity task on a shard of its own.
func seedOne(t *testing.T, q *Queue, shard core.ShardID) {
	t.Helper()
	err := q.Enqueue(context.Background(), []core.Task{{
		ShardID: shard, Type: core.TaskActivity,
		WorkflowID: "wf", RunID: core.RunID(uuid.NewString()), ScheduledEventSeq: 2,
	}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

// SKIP LOCKED is what lets concurrent workers make progress. With four tasks and
// four simultaneous pollers, each must get a different one.
func TestConcurrentPollsDoNotCollide(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	q := New(pool, Config{LockDuration: 30 * time.Second})
	ctx := context.Background()

	const n = 4
	var seed []core.Task
	for i := 0; i < n; i++ {
		seed = append(seed, core.Task{ShardID: shard, Type: core.TaskWorkflow, WorkflowID: "wf", RunID: core.RunID(uuid.NewString())})
	}
	if err := q.Enqueue(ctx, seed); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ids := make(chan int64, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			task, err := q.Poll(ctx, shard, core.TaskWorkflow)
			if err != nil {
				errs <- err
				return
			}
			ids <- task.TaskID
		}()
	}
	seen := map[int64]bool{}
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent poll: %v", err)
		case id := <-ids:
			if seen[id] {
				t.Fatalf("task %d was handed to two workers", id)
			}
			seen[id] = true
		case <-time.After(10 * time.Second):
			t.Fatal("a concurrent poll blocked; SKIP LOCKED is not in effect")
		}
	}
}

// The depth query is raw SQL that no unit test executes, so this is the only
// place its predicate is actually checked against PostgreSQL — including that
// the shard list really does arrive as an int4[].
func TestDepthSamplerCountsOnlyWhatAWorkerCouldPollNow(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	q := New(pool, Config{LockDuration: 30 * time.Second})
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	later := time.Now().Add(time.Hour)
	if err := q.Enqueue(ctx, []core.Task{
		{ShardID: shard, Type: core.TaskWorkflow, WorkflowID: "wf-1", RunID: run},
		{ShardID: shard, Type: core.TaskWorkflow, WorkflowID: "wf-2", RunID: run},
		{ShardID: shard, Type: core.TaskActivity, WorkflowID: "wf-3", RunID: run},
		// Backing off after a failure: queued, but not waiting for a worker.
		{ShardID: shard, Type: core.TaskActivity, WorkflowID: "wf-4", RunID: run, VisibleAt: later},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	obs := newRecordingDepths()
	s := NewDepthSampler(q, DepthConfig{
		Shards:   func() []core.ShardID { return []core.ShardID{shard} },
		Observer: obs,
	})
	if err := s.Sample(ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got, _ := obs.depth(shard, core.TaskWorkflow); got != 2 {
		t.Fatalf("workflow depth = %v, want 2", got)
	}
	if got, _ := obs.depth(shard, core.TaskActivity); got != 1 {
		t.Fatalf("activity depth = %v, want 1: a task backing off is not waiting", got)
	}

	// A task under a live lock is in flight, not queued. Counting it would make
	// an engine that is keeping up look like one falling behind.
	if _, err := q.Poll(ctx, shard, core.TaskWorkflow); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if err := s.Sample(ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got, _ := obs.depth(shard, core.TaskWorkflow); got != 1 {
		t.Fatalf("workflow depth = %v after one task was locked, want 1", got)
	}
}
