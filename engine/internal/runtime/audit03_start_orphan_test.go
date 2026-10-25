package runtime

// AUDIT 03 EVIDENCE, now a regression test.
//
// This file arrived from the production-readiness audit as a CHARACTERISATION
// test: it asserted the defective behaviour of Engine.StartWorkflow on purpose,
// and its own header said "when the defect is fixed this test will FAIL on the
// first assertion; flip it into a regression test at that point." That has
// happened, so the assertions below are inverted — same scenario, opposite
// expectation — and the original wording is kept so the two can be compared.
//
// The scenario: a node accepts POST /v1/workflows for a shard it does not own.
// The old engine had already committed the executions row by then, on the pool,
// outside any transaction and with no fencing check, so the row survived as
// RUNNING with last_seq 0, no event and no task. A retry of the same
// workflow_id was answered AlreadyOpen (HTTP 200) and the workflow never ran.
//
// What must happen now: ownership is checked BEFORE anything is written, the
// row is opened inside the fenced decision transaction, and the retry produces
// a real run. The fuller set of start-path failure modes is in
// start_atomicity_test.go; this one is kept because it is the independent
// reproduction a second auditor reached without shared context.
//
// Needs a real PostgreSQL. internal/testdb supplies the DSN and refuses to let
// it be the demonstration cluster's.

import (
	"context"
	"errors"
	"testing"

	"github.com/dharaneswar339/durexec/internal/api"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
	"github.com/dharaneswar339/durexec/internal/testdb"
)

func TestAudit03StartWorkflowLeavesNoOrphanWhenTheShardIsNotOwned(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()

	const shardID = core.ShardID(777001)
	const wf = "audit03-orphan-wf"
	cleanup := func() {
		for _, tbl := range []string{"events", "executions", "tasks", "shard_leases"} {
			_, _ = pool.Exec(ctx, "DELETE FROM "+tbl+" WHERE shard_id = $1", int32(shardID))
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	// The lease row has to exist for the commit's fencing check to pass once
	// this node does own the shard. Under the old code the start path never
	// consulted it at all.
	if _, err := pool.Exec(ctx,
		`INSERT INTO shard_leases (shard_id, owner_node, fencing_token, expires_at)
		 VALUES ($1, 'node-test', 1, now() + interval '1 hour')
		 ON CONFLICT (shard_id) DO UPDATE SET fencing_token = 1`, int32(shardID)); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	store := eventlog.New(pool)
	owned := false
	e := &Engine{
		Executions: store, Events: store, Committer: store,
		Leases:     &fakeLeases{lease: core.Lease{ShardID: shardID, Owner: "someone-else"}},
		ShardCount: 4, NodeID: "node-test", Clock: fixedClock{testNow},
		TokenFor: func(core.ShardID) (core.FencingToken, bool) { return 1, owned },
	}
	req := api.StartRequest{ShardID: shardID, WorkflowID: wf, WorkflowType: "DeliveryOrder"}

	count := func(table string) int {
		var n int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE shard_id=$1 AND workflow_id=$2",
			int32(shardID), wf).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// 1. Start while the shard is not owned: the caller is told NotOwner.
	if _, err := e.StartWorkflow(ctx, req); err == nil {
		t.Fatal("expected a NotOwner error")
	}

	// 2. ...and NOTHING was committed. This is the assertion that was inverted:
	// the audit asserted the orphan row existed, because it did.
	if _, err := store.Get(ctx, shardID, wf); !errors.Is(err, core.ErrWorkflowNotFound) {
		t.Fatalf("a refused start left an executions row behind: Get returned %v, want ErrWorkflowNotFound", err)
	}
	if ev, tk := count("events"), count("tasks"); ev != 0 || tk != 0 {
		t.Fatalf("a refused start wrote events=%d tasks=%d, want 0 and 0", ev, tk)
	}

	// 3. The client retries once the shard is owned. It must get a real run,
	// not "already open" over a corpse.
	owned = true
	res, err := e.StartWorkflow(ctx, req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.AlreadyOpen {
		t.Fatalf("retry reported AlreadyOpen for a workflow that had never started: %+v", res)
	}
	if ev, tk := count("events"), count("tasks"); ev != 1 || tk != 1 {
		t.Fatalf("after the retry: events=%d tasks=%d, want 1 and 1 so a worker will run it", ev, tk)
	}
	ex, err := store.Get(ctx, shardID, wf)
	if err != nil {
		t.Fatalf("get after retry: %v", err)
	}
	if ex.RunID != res.RunID || ex.LastSeq != 1 {
		t.Fatalf("execution = run %s last_seq %d, want run %s at last_seq 1", ex.RunID, ex.LastSeq, res.RunID)
	}
	t.Logf("FIXED: the refused start wrote nothing, and the retry produced a real run (%s) with 1 event and 1 task",
		res.RunID)
}
