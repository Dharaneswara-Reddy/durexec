package runtime

// Regression tests for the reconciler (findings F-01's residue and F-13).
//
// The engine can no longer CREATE an abandoned start — that is fixed at source
// in start_atomicity_test.go. These tests are about the rows that already
// exist, and about the state the audit could prove was reachable but
// deliberately would not attribute to a cause: one live workflow with a
// complete, successful history whose only missing event was its own
// completion, three days stale, reported as healthy, with no recovery path
// anywhere in the engine.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/taskq"
)

// reconcilerFor wires a Reconciler to the harness's store and shard, with a
// staleness threshold short enough to test and a real queue so an enqueue is a
// real row.
func reconcilerFor(t *testing.T, h *startHarness, stale time.Duration) *Reconciler {
	t.Helper()
	h.engine.Tasks = taskq.New(h.pool, taskq.DefaultConfig())
	return &Reconciler{
		Engine:  h.engine,
		Scanner: h.store,
		Shards:  func() []core.ShardID { return []core.ShardID{h.shard} },
		Stale:   stale,
	}
}

// seedOrphan writes the exact shape of the 19 rows on the live cluster:
// RUNNING, last_seq 0, no events, no task, no timer. Raw SQL, because the
// engine can no longer produce it.
func seedOrphan(t *testing.T, h *startHarness, wfID string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO executions (shard_id, workflow_id, run_id, workflow_type, status, last_seq, created_at, updated_at)
		 VALUES ($1, $2, $3::uuid, 'DeliveryOrder', 'RUNNING', 0, now() - interval '2 days', now() - interval '2 days')`,
		int32(h.shard), wfID, uuid.NewString()); err != nil {
		t.Fatalf("seed orphan %s: %v", wfID, err)
	}
}

func TestTheReconcilerRemovesAbandonedStartsAndFreesTheBusinessKey(t *testing.T) {
	h := newStartHarness(t)
	r := reconcilerFor(t, h, time.Minute)
	ctx := context.Background()

	for _, wf := range []string{"orphan-a", "orphan-b", "orphan-c"} {
		seedOrphan(t, h, wf)
	}

	// Before: the engine refuses to tell a client these are open, but cannot
	// clear them either.
	if _, err := h.engine.StartWorkflow(ctx, h.req("orphan-a")); err == nil {
		t.Fatal("a start over an orphan must not report success")
	}

	res := r.Once(ctx)
	if res.Scanned != 3 || res.Deleted != 3 || res.Redriven != 0 {
		t.Fatalf("pass = %+v, want 3 scanned and 3 deleted", res)
	}
	h.assertNoOrphansInShard(t)

	// After: the business key is free, so the work the client asked for can
	// actually be started.
	resStart, err := h.engine.StartWorkflow(ctx, h.req("orphan-a"))
	if err != nil {
		t.Fatalf("start after reconciliation: %v", err)
	}
	if resStart.AlreadyOpen {
		t.Fatal("start after reconciliation reported AlreadyOpen")
	}
	h.assertFullyStarted(t, "orphan-a")

	// A second pass finds nothing, so the component is not churning.
	if second := r.Once(ctx); second.Scanned != 0 {
		t.Fatalf("second pass = %+v, want nothing left to do", second)
	}
}

// F-13's state: a run with a complete, successful history and no task row. The
// repair is to hand it a workflow task; replay writes the completion it was
// missing. The reconciler must never write an event itself.
func TestTheReconcilerRedrivesARunThatLostItsTaskRow(t *testing.T) {
	h := newStartHarness(t)
	r := reconcilerFor(t, h, time.Minute)
	ctx := context.Background()

	res, err := h.engine.StartWorkflow(ctx, h.req("wf-lost-task"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Whatever put the live workflow in this state — a restore, an operator, a
	// test writing into the live database — the effect was this: the task row
	// is gone and the row is stale.
	if _, err := h.pool.Exec(ctx,
		`DELETE FROM tasks WHERE shard_id = $1 AND workflow_id = $2`,
		int32(h.shard), "wf-lost-task"); err != nil {
		t.Fatalf("remove the task row: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE executions SET updated_at = now() - interval '3 days'
		  WHERE shard_id = $1 AND workflow_id = $2`,
		int32(h.shard), "wf-lost-task"); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	before := h.shape(t, "wf-lost-task")
	if before.tasks != 0 {
		t.Fatalf("precondition: %d task(s) remain", before.tasks)
	}

	pass := r.Once(ctx)
	if pass.Redriven != 1 || pass.Deleted != 0 {
		t.Fatalf("pass = %+v, want 1 re-driven and 0 deleted", pass)
	}

	after := h.shape(t, "wf-lost-task")
	if after.tasks != 1 {
		t.Fatalf("%d task(s) after reconciliation, want 1", after.tasks)
	}
	// Nothing was journaled and nothing was failed: the reconciler's whole job
	// is to make the engine look at the run again, not to decide its fate.
	if after.events != before.events {
		t.Fatalf("events went from %d to %d; the reconciler must not write history",
			before.events, after.events)
	}
	if after.status != string(core.StatusRunning) {
		t.Fatalf("status = %q, want it left RUNNING", after.status)
	}
	if after.runID != string(res.RunID) {
		t.Fatalf("run id changed from %s to %s", res.RunID, after.runID)
	}

	// And the task it enqueued is pollable by a real worker.
	q := taskq.New(h.pool, taskq.DefaultConfig())
	task, err := q.Poll(ctx, h.shard, core.TaskWorkflow)
	if err != nil {
		t.Fatalf("poll the re-driven task: %v", err)
	}
	if task.WorkflowID != "wf-lost-task" {
		t.Fatalf("polled %q, want wf-lost-task", task.WorkflowID)
	}
}

// A healthy workflow must never be touched, and the two independent reasons it
// is safe — it has a task, and it is not stale — are both asserted so a change
// that breaks one is still caught by the other.
func TestTheReconcilerLeavesHealthyWorkflowsAlone(t *testing.T) {
	h := newStartHarness(t)
	r := reconcilerFor(t, h, time.Minute)
	ctx := context.Background()

	// Freshly started: has a task AND is not stale.
	if _, err := h.engine.StartWorkflow(ctx, h.req("wf-healthy")); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Stale but still has its task, which is a busy worker, not a stuck run.
	if _, err := h.engine.StartWorkflow(ctx, h.req("wf-busy")); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE executions SET updated_at = now() - interval '3 days'
		  WHERE shard_id = $1 AND workflow_id = $2`,
		int32(h.shard), "wf-busy"); err != nil {
		t.Fatalf("age the row: %v", err)
	}
	// Completed, so not RUNNING at all.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO executions (shard_id, workflow_id, run_id, workflow_type, status, last_seq, updated_at)
		 VALUES ($1, 'wf-done', $2::uuid, 'DeliveryOrder', 'COMPLETED', 4, now() - interval '3 days')`,
		int32(h.shard), uuid.NewString()); err != nil {
		t.Fatalf("seed a completed run: %v", err)
	}

	if res := r.Once(ctx); res.Scanned != 0 {
		t.Fatalf("pass = %+v, want nothing scanned", res)
	}
	h.assertFullyStarted(t, "wf-healthy")
	h.assertFullyStarted(t, "wf-busy")
}

// An escalation hold has the SAME database signature as a stuck run — RUNNING,
// no task, no timer — because applyOutcome deliberately writes the scheduling
// event without a task while it waits for a human. The scan must exclude it, or
// the one query an operator relies on to find abandoned work is noisy exactly
// where it needs to be precise.
func TestTheReconcilerDoesNotTouchAnUnresolvedEscalationHold(t *testing.T) {
	h := newStartHarness(t)
	r := reconcilerFor(t, h, time.Minute)
	ctx := context.Background()

	run := core.RunID(uuid.NewString())
	started, _ := json.Marshal(map[string]any{"workflow_type": "DeliveryOrder", "input": nil})
	verdict, _ := json.Marshal(core.DedupEvaluated{
		K: 0, Verdict: core.VerdictEscalate, Similarity: 0.87,
		TauLow: 0.85, TauHigh: 0.90, Model: "test",
	})
	sched, _ := json.Marshal(core.ActivityScheduled{
		K: 0, ActivityType: "ChargeCard", Input: json.RawMessage(`null`),
		DedupKey: "k", Mode: core.ModeExact,
	})

	// Exactly what a held activity looks like: the verdict and the scheduling
	// event are in history, and NO task row was created.
	d := core.Decision{
		ShardID: h.shard, WorkflowID: "wf-held", RunID: run, Token: h.token,
		CreateExecution: &core.NewExecution{WorkflowType: "DeliveryOrder"},
		Events: []core.Event{
			{ShardID: h.shard, WorkflowID: "wf-held", RunID: run, Seq: 1,
				Type: core.EvWorkflowStarted, Payload: started, CreatedAt: testNow},
			{ShardID: h.shard, WorkflowID: "wf-held", RunID: run, Seq: 2,
				Type: core.EvDedupEvaluated, Payload: verdict, CreatedAt: testNow},
			{ShardID: h.shard, WorkflowID: "wf-held", RunID: run, Seq: 3,
				Type: core.EvActivityScheduled, Payload: sched, CreatedAt: testNow},
		},
		Status: core.StatusRunning, LastSeq: 3,
	}
	if err := h.store.CommitDecision(ctx, d); err != nil {
		t.Fatalf("seed the held activity: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE executions SET updated_at = now() - interval '3 days'
		  WHERE shard_id = $1 AND workflow_id = 'wf-held'`, int32(h.shard)); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	if res := r.Once(ctx); res.Scanned != 0 {
		t.Fatalf("pass = %+v, want the held activity excluded", res)
	}

	// Once a human adjudicates, the hold is resolved — and a run that is STILL
	// without a task after that really is stuck, so the scan must then see it.
	adj, _ := json.Marshal(core.SignalReceived{Name: "__adjudicate", Payload: json.RawMessage(`{"allow":true}`)})
	if err := h.store.CommitDecision(ctx, core.Decision{
		ShardID: h.shard, WorkflowID: "wf-held", RunID: run, Token: h.token,
		Events: []core.Event{{
			ShardID: h.shard, WorkflowID: "wf-held", RunID: run, Seq: 4,
			Type: core.EvSignalReceived, Payload: adj, CreatedAt: testNow,
		}},
		Status: core.StatusRunning, LastSeq: 4,
	}); err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE executions SET updated_at = now() - interval '3 days'
		  WHERE shard_id = $1 AND workflow_id = 'wf-held'`, int32(h.shard)); err != nil {
		t.Fatalf("age the row again: %v", err)
	}

	if res := r.Once(ctx); res.Redriven != 1 {
		t.Fatalf("pass after adjudication = %+v, want the run re-driven", res)
	}
}

// The reconciler must only touch shards this node owns, or two nodes would
// repair the same rows.
func TestTheReconcilerSkipsShardsThisNodeDoesNotOwn(t *testing.T) {
	h := newStartHarness(t)
	r := reconcilerFor(t, h, time.Minute)
	seedOrphan(t, h, "orphan-unowned")

	h.owned = false
	if res := r.Once(context.Background()); res.Scanned != 0 || res.Deleted != 0 {
		t.Fatalf("pass = %+v, want nothing done on an unowned shard", res)
	}
	// Still there, untouched, for whoever does own it.
	if got := h.shape(t, "orphan-unowned"); got.rows != 1 {
		t.Fatalf("the row was touched anyway: %+v", got)
	}
}

// The DELETE re-tests every condition, so a row that comes alive between the
// scan and the repair is left alone. This is what makes the component safe to
// run against a live shard.
func TestTheReconcilerWillNotDeleteARowThatCameAliveSinceTheScan(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()
	// A threshold of zero means "anything not touched this instant", which is
	// the most aggressive setting there is — and still safe.
	r := reconcilerFor(t, h, 0)
	r.Stale = time.Nanosecond

	seedOrphan(t, h, "orphan-revived")

	// Something appends an event to it, which is what "came alive" means.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO events (shard_id, workflow_id, run_id, seq, event_type, payload)
		 SELECT shard_id, workflow_id, run_id, 1, 'WorkflowExecutionStarted', '{}'::jsonb
		   FROM executions WHERE shard_id = $1 AND workflow_id = 'orphan-revived'`,
		int32(h.shard)); err != nil {
		t.Fatalf("append an event: %v", err)
	}

	res := r.Once(ctx)
	if res.Deleted != 0 {
		t.Fatalf("pass = %+v, want 0 deleted: the row has history now", res)
	}
	if got := h.shape(t, "orphan-revived"); got.rows != 1 || got.events != 1 {
		t.Fatalf("the row was deleted despite having history: %+v", got)
	}
	// It is re-driven instead, which is the correct treatment for a run with
	// history and no task.
	if res.Redriven != 1 {
		t.Fatalf("pass = %+v, want it re-driven rather than deleted", res)
	}
}
