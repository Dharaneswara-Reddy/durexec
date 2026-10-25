package taskq

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/testdb"
)

// testPool goes through internal/testdb rather than reading DUREXEC_TEST_DSN
// itself. testdb applies the migrations, so these tests no longer depend on
// some other package having created the schema first, and it refuses a DSN
// pointing at the demonstration cluster's ports. Reading the variable here
// bypassed that guard, which is how this package once wrote 48 rows at
// shard_id 900-911 into the live demo database.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testdb.Pool(t)
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

// ageSQL is raw SQL that no unit test executes, so this is the only place its
// predicate is checked against PostgreSQL — including that the shard list
// really does arrive as an int4[] and the task types as a text[].
//
// The property is the same one the old count had, and it is the reason the
// gauge is worth anything: only work a worker could take RIGHT NOW registers.
// A task backing off after a failure is queued but not waiting, and a task
// under a live lock is in flight, not waiting. Reporting either would make an
// engine that is keeping up look like one falling behind.
func TestQueueAgeReportsOnlyWhatAWorkerCouldPollNow(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	q := New(pool, Config{LockDuration: 30 * time.Second})
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	later := time.Now().Add(time.Hour)
	// Enqueued with an explicit visible_at in the past, so the age is a known
	// quantity rather than however long this test took to get here.
	oldest := time.Now().Add(-90 * time.Second)
	newer := time.Now().Add(-20 * time.Second)
	if err := q.Enqueue(ctx, []core.Task{
		{ShardID: shard, Type: core.TaskWorkflow, WorkflowID: "wf-1", RunID: run, VisibleAt: oldest},
		{ShardID: shard, Type: core.TaskWorkflow, WorkflowID: "wf-2", RunID: run, VisibleAt: newer},
		{ShardID: shard, Type: core.TaskActivity, WorkflowID: "wf-3", RunID: run, VisibleAt: newer},
		// Backing off after a failure: queued, but not waiting for a worker.
		{ShardID: shard, Type: core.TaskActivity, WorkflowID: "wf-4", RunID: run, VisibleAt: later},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	obs := newRecordingAges()
	s := NewDepthSampler(q, DepthConfig{
		Shards:   func() []core.ShardID { return []core.ShardID{shard} },
		Observer: obs,
	})
	if err := s.Sample(ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}

	// The OLDEST claimable task sets the gauge, not the newest and not the
	// count: 90s, within a generous tolerance for how long the test took.
	got, ok := obs.age(shard, core.TaskWorkflow)
	if !ok {
		t.Fatal("no workflow age was published")
	}
	if got < 85 || got > 120 {
		t.Fatalf("workflow age = %vs, want about 90 (the oldest claimable task)", got)
	}
	// The activity queue's only claimable task is 20s old; the one backing off
	// for an hour must not be what is reported.
	got, ok = obs.age(shard, core.TaskActivity)
	if !ok {
		t.Fatal("no activity age was published")
	}
	if got < 15 || got > 50 {
		t.Fatalf("activity age = %vs, want about 20: a task backing off is not waiting", got)
	}

	// Polling locks the oldest workflow task, so the gauge must fall back to
	// the next claimable one — 20s, not 90s. A task under a live lock is in
	// flight, not waiting.
	if _, err := q.Poll(ctx, shard, core.TaskWorkflow); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if err := s.Sample(ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	got, _ = obs.age(shard, core.TaskWorkflow)
	if got < 15 || got > 50 {
		t.Fatalf("workflow age = %vs after the oldest task was locked, want about 20", got)
	}

	// With both workflow tasks in flight, nothing is waiting, and the gauge
	// must say exactly that rather than keeping its last value.
	if _, err := q.Poll(ctx, shard, core.TaskWorkflow); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if err := s.Sample(ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got, _ := obs.age(shard, core.TaskWorkflow); got != 0 {
		t.Fatalf("workflow age = %v with every task in flight, want an explicit 0", got)
	}
}

// TestPollUsesTheReadyIndex is the guard against the one failure mode that no
// behavioural test can see: the statement and the index drifting apart. If
// readyAt in queue.go and the expression in
// migrations/0002_poll_ready_index.up.sql stop matching exactly, the poll
// still returns the right task — it just goes back to reading and discarding
// every unclaimable row at the head of the queue, which measured 95ms and
// 208 MiB of pages for one poll at 100,000 rows per shard.
func TestPollUsesTheReadyIndex(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	q := New(pool, DefaultConfig())
	// Enough rows that an index scan is plainly cheaper than a sequential
	// scan, so the plan is chosen on cost rather than on the table being tiny.
	tasks := make([]core.Task, 0, 2000)
	for i := 0; i < 2000; i++ {
		tasks = append(tasks, core.Task{
			ShardID: shard, Type: core.TaskActivity,
			WorkflowID: "wf-" + uuid.NewString(), RunID: run,
		})
	}
	if err := q.Enqueue(ctx, tasks); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE tasks"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// EXPLAIN only. The pick CTE is what chooses a row, so it is what the
	// plan has to be checked on; the UPDATE half is a primary-key lookup of
	// one row and costs the same whatever index exists.
	//
	// EXPLAIN returns one row per plan line, so every row has to be read —
	// scanning only the first gets "Limit" and nothing else, which silently
	// passes or fails on the wrong evidence.
	rows, err := pool.Query(ctx, `
EXPLAIN (FORMAT TEXT)
SELECT shard_id, task_id FROM tasks
 WHERE shard_id=$1 AND task_type=$2
   AND `+readyAt+` <= now()
 ORDER BY shard_id, task_type, `+readyAt+`
 FOR UPDATE SKIP LOCKED LIMIT 1`, int32(shard), string(core.TaskActivity))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan plan line: %v", err)
		}
		lines = append(lines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}
	plan := strings.Join(lines, "\n")
	t.Logf("plan:\n%s", plan)

	if !strings.Contains(plan, "tasks_poll_ready") {
		t.Fatalf("the poll is not using tasks_poll_ready, so readyAt and the "+
			"migration have drifted apart:\n%s", plan)
	}
	if strings.Contains(plan, "Sort") {
		t.Fatalf("the poll plan has a Sort, so the index no longer serves the "+
			"ordering:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan") {
		t.Fatalf("the poll plan has a Seq Scan:\n%s", plan)
	}
}
