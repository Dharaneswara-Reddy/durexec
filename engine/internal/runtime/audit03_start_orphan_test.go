package runtime

// AUDIT 03 EVIDENCE (added by the production-readiness audit, not part of the
// original suite).
//
// CHARACTERISATION TEST: it asserts the CURRENT behaviour of
// Engine.StartWorkflow, which is a defect. Engine.StartWorkflow's comment says
// "all in one Decision, so a start either fully happens or does not happen at
// all", but Executions.Create is a separate, already-committed statement that
// runs BEFORE the fencing-checked CommitDecision. If this node does not hold
// the shard's lease at that moment (TokenFor returns false), or the commit is
// fenced out, the executions row stays behind: status RUNNING, last_seq 0, no
// event, no task. A retry of the same workflow_id then gets AlreadyOpen
// (HTTP 200) and the workflow never runs.
//
// When the defect is fixed this test will FAIL on the first assertion; flip it
// into a regression test at that point.
//
// Needs a real PostgreSQL: DUREXEC_TEST_DSN must be set. Use a THROWAWAY
// database, never the demonstration cluster's.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/api"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
)

func TestAudit03StartWorkflowLeavesAnOrphanWhenTheShardIsNotOwned(t *testing.T) {
	dsn := os.Getenv("DUREXEC_TEST_DSN")
	if dsn == "" {
		t.Skip("set DUREXEC_TEST_DSN (a throwaway database) to run")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := eventlog.Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	const shardID = core.ShardID(777001)
	const wf = "audit03-orphan-wf"
	cleanup := func() {
		for _, tbl := range []string{"events", "executions", "tasks", "shard_leases"} {
			_, _ = pool.Exec(ctx, "DELETE FROM "+tbl+" WHERE shard_id = $1", int32(shardID))
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	store := eventlog.New(pool)
	owned := false
	e := &Engine{
		Executions: store, Events: store, Committer: store,
		Leases:     &fakeLeases{lease: core.Lease{ShardID: shardID, Owner: "someone-else"}},
		ShardCount: 4, NodeID: "node-test", Clock: fixedClock{testNow},
		TokenFor: func(core.ShardID) (core.FencingToken, bool) { return 1, owned },
	}
	req := api.StartRequest{ShardID: shardID, WorkflowID: wf, WorkflowType: "DeliveryOrder"}

	// 1. Start while the shard is not owned: the caller is told NotOwner.
	if _, err := e.StartWorkflow(ctx, req); err == nil {
		t.Fatal("expected a NotOwner error")
	}

	// 2. ...but an executions row was already committed.
	ex, err := store.Get(ctx, shardID, wf)
	if err != nil {
		t.Fatalf("expected the orphan execution row to exist (current, defective behaviour): %v", err)
	}
	var events, tasks int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE shard_id=$1 AND workflow_id=$2`, int32(shardID), wf).Scan(&events)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE shard_id=$1 AND workflow_id=$2`, int32(shardID), wf).Scan(&tasks)
	if ex.Status != core.StatusRunning || ex.LastSeq != 0 || events != 0 || tasks != 0 {
		t.Fatalf("orphan shape changed: status=%s last_seq=%d events=%d tasks=%d", ex.Status, ex.LastSeq, events, tasks)
	}

	// 3. The client retries once the shard is owned. It is told the workflow is
	// already open (a success), and still nothing will ever run it.
	owned = true
	res, err := e.StartWorkflow(ctx, req)
	if err != nil || !res.AlreadyOpen {
		t.Fatalf("retry: res=%+v err=%v", res, err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE shard_id=$1 AND workflow_id=$2`, int32(shardID), wf).Scan(&events)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE shard_id=$1 AND workflow_id=$2`, int32(shardID), wf).Scan(&tasks)
	if events != 0 || tasks != 0 {
		t.Fatalf("expected the workflow to stay inert after the 'already open' retry; events=%d tasks=%d", events, tasks)
	}
	t.Logf("CONFIRMED: retry returned AlreadyOpen=true (HTTP 200) for a workflow with 0 events and 0 tasks; it will never execute")
}
