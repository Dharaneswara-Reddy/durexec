// Package acceptit holds the acceptance assertions that used to be MANUAL rows
// in the acceptance matrix because they need a crash, a takeover or a restart.
//
// Each test here drives the REAL packages (eventlog, lease, taskq, timers)
// against a REAL PostgreSQL, simulates the crash by abandoning a goroutine's
// token, lock or in-memory state (the same thing a SIGKILLed node leaves
// behind in the database), and then ASSERTS. A test only counts if it can fail:
// every one has a violation it would catch, and says which.
//
// Outcomes, in the matrix's own words:
//
//	PASS     the test ran against PostgreSQL and every assertion held
//	FAIL     it ran and an assertion did not hold
//	BLOCKED  it could not run because something it needs does not exist yet.
//	         It skips with a reason starting "BLOCKED-ON-<owner>:" and
//	         cmd/testinventory lists it under BLOCKED, never as a pass
//	SKIPPED  no database was configured. Optional locally, a FAILURE when
//	         DUREXEC_REQUIRE_DB is set (CI, make test-integration)
//
// What this does NOT show: a real node dying mid-transaction on the live
// cluster. These tests simulate the crash at the database boundary. The
// kill-a-container evidence is still the loadgen/chaos runs under docs/results.
package acceptit

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
	"github.com/dharaneswar339/durexec/internal/lease"
	"github.com/dharaneswar339/durexec/internal/taskq"
	"github.com/dharaneswar339/durexec/internal/testdb"
	"github.com/dharaneswar339/durexec/internal/timers"
)

var allTables = []string{"events", "executions", "tasks", "timers", "activity_intents", "shard_leases"}

// shard returns a shard id used by nothing else and clears it before and after.
func shard(t *testing.T, pool *pgxpool.Pool) core.ShardID {
	t.Helper()
	s := core.ShardID(700000 + rand.Int31n(1000000))
	clean := func() {
		for _, tbl := range allTables {
			_, _ = pool.Exec(context.Background(), "DELETE FROM "+tbl+" WHERE shard_id = $1", int32(s))
		}
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO shard_leases (shard_id, owner_node, fencing_token, expires_at) VALUES ($1, NULL, 0, now())", int32(s)); err != nil {
		t.Fatalf("seed lease row: %v", err)
	}
	return s
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

type countingObserver struct{ n atomic.Int64 }

func (c *countingObserver) FencingRejection(core.ShardID) { c.n.Add(1) }

func startDecision(s core.ShardID, wf string, run core.RunID, tok core.FencingToken) core.Decision {
	return core.Decision{
		ShardID: s, WorkflowID: wf, RunID: run, Token: tok,
		CreateExecution: &core.NewExecution{WorkflowType: "AcceptWF"},
		Events:          []core.Event{{ShardID: s, WorkflowID: wf, RunID: run, Seq: 1, Type: core.EvWorkflowStarted, Payload: json.RawMessage(`{"input":1}`)}},
		NewTasks:        []core.Task{{ShardID: s, Type: core.TaskWorkflow, WorkflowID: wf, RunID: run}},
		Status:          core.StatusRunning, LastSeq: 1,
	}
}

// FR-7 / FR-8. A node that lost the shard to a takeover can write nothing, the
// refusal is counted (fencing_rejections_total's source), and the new owner's
// work is the only work in the log.
//
// Would catch: a fenced-out commit that lands any row; a rejection that is not
// counted; a token that does not increase on takeover.
func TestAcceptFR8StaleOwnerIsFencedOutAfterTakeover(t *testing.T) {
	pool := testdb.Pool(t)
	s := shard(t, pool)
	ctx := context.Background()
	leases := lease.New(pool)
	store := eventlog.New(pool)
	obs := &countingObserver{}
	committer := eventlog.NewFencingCounter(store, obs)

	tokA, err := leases.Acquire(ctx, s, "node-a", 60)
	if err != nil {
		t.Fatalf("node-a acquire: %v", err)
	}
	// node-a "freezes" (GC pause, partition, SIGSTOP): its lease runs out while
	// it still believes it is the owner.
	if _, err := pool.Exec(ctx, "UPDATE shard_leases SET expires_at = now() - interval '1 second' WHERE shard_id=$1", int32(s)); err != nil {
		t.Fatal(err)
	}
	tokB, err := leases.Acquire(ctx, s, "node-b", 60)
	if err != nil {
		t.Fatalf("node-b takeover: %v", err)
	}
	if tokB <= tokA {
		t.Fatalf("FENCING VIOLATION: takeover token %d is not greater than the old token %d", tokB, tokA)
	}

	runA, runB := core.RunID(uuid.NewString()), core.RunID(uuid.NewString())
	// node-a wakes up and tries to commit with its old token.
	err = committer.CommitDecision(ctx, startDecision(s, "wf-stale", runA, tokA))
	if !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("stale owner commit: got %v, want ErrLostOwnership", err)
	}
	if n := obs.n.Load(); n != 1 {
		t.Fatalf("fencing rejection counted %d times, want exactly 1", n)
	}
	for _, tbl := range []string{"events", "executions", "tasks"} {
		if n := count(t, pool, "SELECT count(*) FROM "+tbl+" WHERE shard_id=$1 AND workflow_id='wf-stale'", int32(s)); n != 0 {
			t.Fatalf("FENCING VIOLATION: the fenced-out node left %d row(s) in %s", n, tbl)
		}
	}
	// the real owner proceeds, and is not counted as a rejection.
	if err := committer.CommitDecision(ctx, startDecision(s, "wf-new", runB, tokB)); err != nil {
		t.Fatalf("new owner commit: %v", err)
	}
	if n := obs.n.Load(); n != 1 {
		t.Fatalf("a successful commit changed the rejection count to %d", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM events WHERE shard_id=$1", int32(s)); n != 1 {
		t.Fatalf("expected exactly the new owner's 1 event, found %d", n)
	}
}

// Start atomicity (the start-path orphan, audit finding F-01). Depends on
// core.Decision.CreateExecution, added by the correctness fix.
//
// Would catch: an executions row that survives a rolled-back start.
func TestAcceptStartIsAtomicWithItsFirstEvent(t *testing.T) {
	pool := testdb.Pool(t)
	s := shard(t, pool)
	ctx := context.Background()
	leases := lease.New(pool)
	store := eventlog.New(pool)

	tok, err := leases.Acquire(ctx, s, "node-a", 60)
	if err != nil {
		t.Fatal(err)
	}
	// lose the shard between the ownership check and the commit
	if _, err := pool.Exec(ctx, "UPDATE shard_leases SET fencing_token = fencing_token + 5 WHERE shard_id=$1", int32(s)); err != nil {
		t.Fatal(err)
	}
	run := core.RunID(uuid.NewString())
	if err := store.CommitDecision(ctx, startDecision(s, "wf-start", run, tok)); !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("start with a lost lease: got %v, want ErrLostOwnership", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM executions WHERE shard_id=$1", int32(s)); n != 0 {
		t.Fatalf("START-PATH ORPHAN: %d executions row(s) survive a start that did not commit", n)
	}

	// and a start that does commit leaves row, event and task together.
	cur, _ := leases.Get(ctx, s)
	if err := store.CommitDecision(ctx, startDecision(s, "wf-start", run, cur.Token)); err != nil {
		t.Fatalf("start with the current token: %v", err)
	}
	if got := count(t, pool, "SELECT count(*) FROM executions e WHERE e.shard_id=$1 AND e.last_seq=1", int32(s)); got != 1 {
		t.Fatalf("committed start: want 1 execution at last_seq=1, got %d", got)
	}
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE shard_id=$1", int32(s)); n != 1 {
		t.Fatalf("committed start: want 1 task, got %d", n)
	}
}

// Orphan detection. The query the acceptance tool and any sweeper would use
// must find an execution with no first event, and must not flag a healthy one.
//
// Would catch: a detector that misses the orphan, or flags a started workflow.
func TestAcceptOrphanDetectionFindsAbandonedStarts(t *testing.T) {
	pool := testdb.Pool(t)
	s := shard(t, pool)
	ctx := context.Background()
	store := eventlog.New(pool)

	// An execution row with nothing behind it, as the old two-step start left.
	// Written by hand: the engine no longer has a way to produce one.
	if _, err := pool.Exec(ctx,
		`INSERT INTO executions (shard_id, workflow_id, run_id, workflow_type, status) VALUES ($1,'wf-orphan',$2::uuid,'AcceptWF','RUNNING')`,
		int32(s), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	// A healthy one.
	tok, _ := lease.New(pool).Acquire(ctx, s, "node-a", 60)
	if err := store.CommitDecision(ctx, startDecision(s, "wf-ok", core.RunID(uuid.NewString()), tok)); err != nil {
		t.Fatal(err)
	}
	// age the orphan past the detector's threshold
	if _, err := pool.Exec(ctx, "UPDATE executions SET created_at = now() - interval '5 minutes' WHERE shard_id=$1", int32(s)); err != nil {
		t.Fatal(err)
	}

	const detector = `
SELECT workflow_id FROM executions
 WHERE shard_id=$1 AND status='RUNNING' AND last_seq=0 AND created_at < now() - interval '30 seconds'`
	rows, err := pool.Query(ctx, detector, int32(s))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		found = append(found, id)
	}
	if len(found) != 1 || found[0] != "wf-orphan" {
		t.Fatalf("detector returned %v, want exactly [wf-orphan]", found)
	}
}

// FR-2 / NFR-1, task recovery. A worker that took a task and died leaves a
// lock that expires; another worker then gets the SAME task, once, and the
// task is not lost and not handed out twice while the first lock is live.
//
// Would catch: a task lost with its worker; a task handed to two workers at
// once; a task that cannot be completed after takeover.
func TestAcceptTaskIsRecoveredAfterItsWorkerDies(t *testing.T) {
	pool := testdb.Pool(t)
	s := shard(t, pool)
	ctx := context.Background()
	q := taskq.New(pool, taskq.Config{LockDuration: 700 * time.Millisecond})

	run := core.RunID(uuid.NewString())
	if err := q.Enqueue(ctx, []core.Task{{ShardID: s, Type: core.TaskWorkflow, WorkflowID: "wf-crash", RunID: run}}); err != nil {
		t.Fatal(err)
	}
	first, err := q.Poll(ctx, s, core.TaskWorkflow)
	if err != nil {
		t.Fatalf("worker A poll: %v", err)
	}
	// worker A dies here: it never completes, extends or releases.

	if _, err := q.Poll(ctx, s, core.TaskWorkflow); !errors.Is(err, core.ErrNoTask) {
		t.Fatalf("task handed to a second worker while worker A's lock is live: got %v", err)
	}
	time.Sleep(900 * time.Millisecond) // lock expires

	second, err := q.Poll(ctx, s, core.TaskWorkflow)
	if err != nil {
		t.Fatalf("TASK LOST: worker B could not recover it after A died: %v", err)
	}
	if second.TaskID != first.TaskID {
		t.Fatalf("recovered a different task: %d vs %d", second.TaskID, first.TaskID)
	}
	if second.Attempt != first.Attempt+1 {
		t.Fatalf("attempt = %d, want %d", second.Attempt, first.Attempt+1)
	}
	// worker A's stale lock token can no longer complete or extend the task.
	if err := q.Complete(ctx, s, first.TaskID, first.LockToken); err == nil {
		t.Fatalf("a dead worker's stale lock completed the task")
	}
	if err := q.Complete(ctx, s, second.TaskID, second.LockToken); err != nil {
		t.Fatalf("worker B complete: %v", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE shard_id=$1", int32(s)); n != 0 {
		t.Fatalf("%d task row(s) remain after completion", n)
	}
}

// FR-4, timer recovery. A timer committed before a crash fires after restart:
// it produces a wake-up task, is removed, and a second scan (a restarted node)
// does not wake the workflow again.
//
// Would catch: a timer that never fires after restart; a timer that fires on
// every scan; a timer that fires before it is due.
func TestAcceptTimerSurvivesRestartAndFiresOnce(t *testing.T) {
	pool := testdb.Pool(t)
	s := shard(t, pool)
	ctx := context.Background()
	leases := lease.New(pool)
	store := eventlog.New(pool)
	q := taskq.New(pool, taskq.DefaultConfig())

	tok, _ := leases.Acquire(ctx, s, "node-a", 60)
	run := core.RunID(uuid.NewString())
	d := startDecision(s, "wf-sleep", run, tok)
	d.NewTasks = nil
	d.NewTimers = []core.Timer{
		{ShardID: s, WorkflowID: "wf-sleep", RunID: run, TimerSeq: 2, FireAt: time.Now().Add(-time.Second)},
		{ShardID: s, WorkflowID: "wf-sleep", RunID: run, TimerSeq: 3, FireAt: time.Now().Add(time.Hour)},
	}
	if err := store.CommitDecision(ctx, d); err != nil {
		t.Fatal(err)
	}

	// "restart": a brand-new scanner over brand-new store objects.
	scanner := timers.NewScanner(timers.New(pool), taskq.New(pool, taskq.DefaultConfig()), timers.ScannerConfig{})
	fired, err := scanner.ScanShard(ctx, s)
	if err != nil || fired != 1 {
		t.Fatalf("first scan fired %d (err %v), want exactly the 1 overdue timer", fired, err)
	}
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE shard_id=$1 AND task_type='WORKFLOW'", int32(s)); n != 1 {
		t.Fatalf("TIMER LOST: %d wake-up task(s), want 1", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM timers WHERE shard_id=$1", int32(s)); n != 1 {
		t.Fatalf("timers remaining = %d, want only the future one", n)
	}
	// another restart, scanning again: nothing is due, so nothing fires.
	again, err := timers.NewScanner(timers.New(pool), q, timers.ScannerConfig{}).ScanShard(ctx, s)
	if err != nil || again != 0 {
		t.Fatalf("second scan fired %d (err %v), want 0", again, err)
	}
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE shard_id=$1", int32(s)); n != 1 {
		t.Fatalf("DOUBLE WAKE-UP: %d tasks after the second scan, want still 1", n)
	}
}

// FR-1 / NFR-1, durability. What was committed is, after the pool is closed and
// reopened (a process restart), byte-identical and in order.
//
// Would catch: a lost, reordered or altered event across a reconnect.
func TestAcceptCommittedHistorySurvivesReconnect(t *testing.T) {
	pool := testdb.Pool(t)
	s := shard(t, pool)
	ctx := context.Background()
	tok, _ := lease.New(pool).Acquire(ctx, s, "node-a", 60)
	store := eventlog.New(pool)

	run := core.RunID(uuid.NewString())
	d := startDecision(s, "wf-durable", run, tok)
	if err := store.CommitDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	for seq := core.Seq(2); seq <= 20; seq++ {
		next := core.Decision{
			ShardID: s, WorkflowID: "wf-durable", RunID: run, Token: tok,
			Events: []core.Event{{ShardID: s, WorkflowID: "wf-durable", RunID: run, Seq: seq, Type: core.EvActivityCompleted,
				Payload: json.RawMessage(`{"k":` + itoa(int(seq)) + `,"result":"r` + itoa(int(seq)) + `"}`)}},
			Status: core.StatusRunning, LastSeq: seq,
		}
		if err := store.CommitDecision(ctx, next); err != nil {
			t.Fatalf("commit seq %d: %v", seq, err)
		}
	}
	before, err := store.Load(ctx, s, "wf-durable", run)
	if err != nil {
		t.Fatal(err)
	}

	// restart: a different pool, a different store object.
	fresh := testdb.Pool(t)
	after, err := eventlog.New(fresh).Load(ctx, s, "wf-durable", run)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 20 || len(after) != 20 {
		t.Fatalf("history length before=%d after=%d, want 20 and 20", len(before), len(after))
	}
	for i := range before {
		if after[i].Seq != core.Seq(i+1) {
			t.Fatalf("event %d has seq %d: history reordered or has a gap", i, after[i].Seq)
		}
		if after[i].Type != before[i].Type || string(after[i].Payload) != string(before[i].Payload) {
			t.Fatalf("event seq %d changed across the reconnect", after[i].Seq)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM (SELECT 1 FROM events WHERE shard_id=$1
		GROUP BY workflow_id, run_id, seq HAVING count(*) > 1) d`, int32(s)); n != 0 {
		t.Fatalf("%d duplicated (workflow, run, seq)", n)
	}
}

// FR-10 / FR-11, saga recovery. A compensation stack that is half done when
// its runner dies continues from where it stopped on a fresh runner: nothing
// already undone is undone again, and what is left is undone in LIFO order.
// This part is pure (internal/saga has no database) and is asserted in
// internal/saga TestCompensateResumesWhereItStopped; the database half,
// compensation events surviving a restart, is the history test above.
//
// What is NOT automated: a compensation that fails after the node holding the
// stack is killed, on the live cluster, ending in COMPENSATING_FAILED. That
// needs the fault-injection hooks at real transaction boundaries and the
// external-effect simulator.
func TestAcceptSagaRecoveryOnLiveClusterBlocked(t *testing.T) {
	t.Skip("BLOCKED-ON-AGENT-A: needs fault-injection hooks at real transaction boundaries (faultinj crash points wired into production code) and the independent external-effect simulator to assert that a compensation killed mid-way is neither skipped nor repeated against a real effect")
}

// The duplicate-effect rows (NFR-3, FR-2 "no completed activity runs twice")
// are about EXTERNAL effects, which every existing test only simulates
// in-process.
func TestAcceptNoDuplicateExternalEffectBlocked(t *testing.T) {
	t.Skip("BLOCKED-ON-AGENT-A: needs the external-effect simulator (a stub service counting calls by dedup key) and a crash between the effect and its journal write")
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
