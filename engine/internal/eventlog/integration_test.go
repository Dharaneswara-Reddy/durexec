package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Everything in this file needs a real PostgreSQL with the pgvector extension
// available. It is skipped unless DUREXEC_TEST_DSN names one, so the unit tests
// still pass on a laptop with no database at all.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DUREXEC_TEST_DSN")
	if dsn == "" {
		t.Skip("set DUREXEC_TEST_DSN to run the PostgreSQL tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// freshShard picks a shard id unlikely to collide with a concurrent test and
// makes sure it has a lease row and no leftover data.
func freshShard(t *testing.T, pool *pgxpool.Pool) core.ShardID {
	t.Helper()
	ctx := context.Background()
	shard := core.ShardID(100000 + rand.Int31n(1000000))

	for _, tbl := range []string{"events", "executions", "tasks", "timers", "activity_intents", "shard_leases"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl+" WHERE shard_id = $1", int32(shard)); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"events", "executions", "tasks", "timers", "activity_intents", "shard_leases"} {
			_, _ = pool.Exec(context.Background(), "DELETE FROM "+tbl+" WHERE shard_id = $1", int32(shard))
		}
	})
	return shard
}

// setLease writes a lease row directly, so the commit tests do not depend on the
// lease package.
func setLease(t *testing.T, pool *pgxpool.Pool, shard core.ShardID, tok core.FencingToken) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO shard_leases (shard_id, owner_node, fencing_token, expires_at)
		 VALUES ($1, 'test', $2, now() + interval '1 hour')
		 ON CONFLICT (shard_id) DO UPDATE SET fencing_token = EXCLUDED.fencing_token`,
		int32(shard), int64(tok))
	if err != nil {
		t.Fatalf("set lease: %v", err)
	}
}

func TestCreateIsIdempotentOnDuplicateStart(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	s := New(pool)
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	if err := s.Create(ctx, shard, "order-1", run, "OrderWorkflow"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// A second start for the same business key must be reported as already open,
	// which the caller treats as success rather than as a failure.
	err := s.Create(ctx, shard, "order-1", core.RunID(uuid.NewString()), "OrderWorkflow")
	if !errors.Is(err, core.ErrWorkflowAlreadyOpen) {
		t.Fatalf("second create: got %v, want ErrWorkflowAlreadyOpen", err)
	}
}

func TestCommitDecisionAppliesEverything(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	setLease(t, pool, shard, 7)
	s := New(pool)
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	if err := s.Create(ctx, shard, "wf-1", run, "T"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A task to complete, standing in for the workflow task that produced this
	// decision.
	var taskID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO tasks (shard_id, task_type, workflow_id, run_id) VALUES ($1,'WORKFLOW',$2,$3::uuid) RETURNING task_id`,
		int32(shard), "wf-1", string(run)).Scan(&taskID)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	emb := make([]float32, 384)
	for i := range emb {
		emb[i] = float32(i) / 1000
	}

	d := core.Decision{
		ShardID: shard, WorkflowID: "wf-1", RunID: run, Token: 7,
		Events: []core.Event{
			{ShardID: shard, WorkflowID: "wf-1", RunID: run, Seq: 1, Type: core.EvWorkflowStarted, Payload: json.RawMessage(`{}`)},
			{ShardID: shard, WorkflowID: "wf-1", RunID: run, Seq: 2, Type: core.EvActivityScheduled, Payload: json.RawMessage(`{"k":0}`)},
		},
		NewTasks:  []core.Task{{ShardID: shard, Type: core.TaskActivity, WorkflowID: "wf-1", RunID: run, ScheduledEventSeq: 2}},
		NewTimers: []core.Timer{{ShardID: shard, WorkflowID: "wf-1", RunID: run, TimerSeq: 3, FireAt: time.Now().Add(time.Hour)}},
		Intents: []core.Intent{{
			ShardID: shard, WorkflowID: "wf-1", RunID: run, ScheduledSeq: 2,
			ActivityType: "ChargeCard", IntentText: "charge 10 usd", Embedding: emb,
		}},
		Status: core.StatusRunning, LastSeq: 2, CompleteTaskID: taskID,
	}
	if err := s.CommitDecision(ctx, d); err != nil {
		t.Fatalf("commit: %v", err)
	}

	hist, err := s.Load(ctx, shard, "wf-1", run)
	if err != nil || len(hist) != 2 {
		t.Fatalf("Load = %d events, %v; want 2", len(hist), err)
	}
	if hist[0].Seq != 1 || hist[1].Seq != 2 {
		t.Fatalf("history out of seq order: %v", hist)
	}
	if last, _ := s.LastSeq(ctx, shard, "wf-1", run); last != 2 {
		t.Fatalf("LastSeq = %d, want 2", last)
	}
	assertCount(t, pool, "SELECT count(*) FROM tasks WHERE shard_id=$1", shard, 1)
	assertCount(t, pool, "SELECT count(*) FROM timers WHERE shard_id=$1", shard, 1)
	assertCount(t, pool, "SELECT count(*) FROM activity_intents WHERE shard_id=$1", shard, 1)
	assertCount(t, pool, "SELECT count(*) FROM tasks WHERE shard_id=$1 AND task_id=$2", shard, 0, taskID)
}

// This is the safety property the engine is built around: a node holding a stale
// fencing token can write nothing at all.
func TestCommitDecisionRejectsStaleToken(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	setLease(t, pool, shard, 9) // the shard has moved on to token 9
	s := New(pool)
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	if err := s.Create(ctx, shard, "wf-2", run, "T"); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := core.Decision{
		ShardID: shard, WorkflowID: "wf-2", RunID: run, Token: 8, // we still hold 8
		Events: []core.Event{{ShardID: shard, WorkflowID: "wf-2", RunID: run, Seq: 1, Type: core.EvWorkflowStarted}},
		Status: core.StatusRunning, LastSeq: 1,
	}
	if err := s.CommitDecision(ctx, d); !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("commit with stale token: got %v, want ErrLostOwnership", err)
	}
	// Nothing may have landed — not even the events that come after the check.
	assertCount(t, pool, "SELECT count(*) FROM events WHERE shard_id=$1", shard, 0)
	assertCount(t, pool, "SELECT count(*) FROM executions WHERE shard_id=$1 AND last_seq > 0", shard, 0)
}

// A duplicate seq means another writer got there first, which we report the same
// way as a lost lease: stop, do not retry blindly.
func TestCommitDecisionRejectsDuplicateSeq(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	setLease(t, pool, shard, 1)
	s := New(pool)
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	if err := s.Create(ctx, shard, "wf-3", run, "T"); err != nil {
		t.Fatalf("create: %v", err)
	}
	d := core.Decision{
		ShardID: shard, WorkflowID: "wf-3", RunID: run, Token: 1,
		Events: []core.Event{{ShardID: shard, WorkflowID: "wf-3", RunID: run, Seq: 1, Type: core.EvWorkflowStarted}},
		Status: core.StatusRunning, LastSeq: 1,
	}
	if err := s.CommitDecision(ctx, d); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if err := s.CommitDecision(ctx, d); !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("replayed commit: got %v, want ErrLostOwnership", err)
	}
}

func assertCount(t *testing.T, pool *pgxpool.Pool, sql string, shard core.ShardID, want int, extra ...any) {
	t.Helper()
	args := append([]any{int32(shard)}, extra...)
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != want {
		t.Fatalf("%q gave %d, want %d", sql, n, want)
	}
}

// The counter is what makes the fencing mechanism externally visible, so it is
// checked against a real PostgreSQL rather than only against a fake: the two
// commits below are indistinguishable to the caller — both report lost
// ownership — and only one of them is the token check firing.
func TestFencingCounterCountsOnlyTheRealRejectionAgainstPostgres(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	setLease(t, pool, shard, 5)
	seen := &countingObserver{}
	c := NewFencingCounter(New(pool), seen)
	s := New(pool)
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	if err := s.Create(ctx, shard, "wf-fenced", run, "T"); err != nil {
		t.Fatalf("create: %v", err)
	}
	started := core.Event{ShardID: shard, WorkflowID: "wf-fenced", RunID: run, Seq: 1, Type: core.EvWorkflowStarted}

	// A stale writer: the shard is at token 5, this one still holds 4.
	stale := core.Decision{
		ShardID: shard, WorkflowID: "wf-fenced", RunID: run, Token: 4,
		Events: []core.Event{started}, Status: core.StatusRunning, LastSeq: 1,
	}
	if err := c.CommitDecision(ctx, stale); !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("stale commit: got %v, want ErrLostOwnership", err)
	}
	if len(seen.shards) != 1 || seen.shards[0] != shard {
		t.Fatalf("rejections counted: %v, want exactly [%d]", seen.shards, shard)
	}

	// The duplicate-seq race reports the same error to the caller, and must not
	// be counted: it is a lost race, not evidence that fencing fired.
	current := core.Decision{
		ShardID: shard, WorkflowID: "wf-fenced", RunID: run, Token: 5,
		Events: []core.Event{started}, Status: core.StatusRunning, LastSeq: 1,
	}
	if err := c.CommitDecision(ctx, current); err != nil {
		t.Fatalf("first commit with the live token: %v", err)
	}
	if err := c.CommitDecision(ctx, current); !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("replayed commit: got %v, want ErrLostOwnership", err)
	}
	if len(seen.shards) != 1 {
		t.Fatalf("rejections counted: %v, want still exactly one", seen.shards)
	}
}
