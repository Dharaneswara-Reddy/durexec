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
