package runtime

// Regression tests for finding F-05: a failed compensation was terminal and
// unrecoverable, and COMPENSATING was dead code.
//
// The dead end, precisely: forward steps 1..3 succeed, step 4 fails, the saga
// unwinds, and the compensation for step 2 exhausts its retry budget. The run
// becomes COMPENSATING_FAILED, which IsTerminal reports true — so Signal
// refused it, Cancel refused it (Cancel goes through Signal), and a fresh start
// on the business key was answered "already open". The compensations for steps
// 2 and 1 were still owed and unreachable. Recovery meant hand-written SQL
// against the one table the design declares append-only.
//
// Separately, core.StatusCompensating was declared and assigned NOWHERE in the
// repository, so a workflow actively rolling back reported RUNNING and an
// operator could not see a rollback in progress at all.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/dharaneswar339/durexec/internal/api"
	"github.com/dharaneswar339/durexec/internal/core"
)

// seedCompensatingFailed puts a run into the dead-end state: a start, a
// completed forward step, and a journaled failure, with the status pointer at
// COMPENSATING_FAILED.
func seedCompensatingFailed(t *testing.T, h *startHarness, wfID string) core.RunID {
	t.Helper()
	ctx := context.Background()
	run := core.RunID(uuid.NewString())

	started, _ := json.Marshal(map[string]any{"workflow_type": "DeliveryOrder", "input": nil})
	sched, _ := json.Marshal(core.ActivityScheduled{
		K: 0, ActivityType: "ChargeCard", Input: json.RawMessage(`{"amount":100}`),
		DedupKey: "k0", Mode: core.ModeExact,
	})
	done, _ := json.Marshal(core.ActivityCompleted{K: 0, Result: json.RawMessage(`{"charged":100}`)})
	failed, _ := json.Marshal(core.WorkflowFailed{
		Error: `compensation "RefundPayment" for workflow "x" failed after 5 attempts: gateway down`,
	})

	d := core.Decision{
		ShardID: h.shard, WorkflowID: wfID, RunID: run, Token: h.token,
		CreateExecution: &core.NewExecution{WorkflowType: "DeliveryOrder"},
		Events: []core.Event{
			{ShardID: h.shard, WorkflowID: wfID, RunID: run, Seq: 1,
				Type: core.EvWorkflowStarted, Payload: started, CreatedAt: testNow},
			{ShardID: h.shard, WorkflowID: wfID, RunID: run, Seq: 2,
				Type: core.EvActivityScheduled, Payload: sched, CreatedAt: testNow},
			{ShardID: h.shard, WorkflowID: wfID, RunID: run, Seq: 3,
				Type: core.EvActivityCompleted, Payload: done, CreatedAt: testNow},
			{ShardID: h.shard, WorkflowID: wfID, RunID: run, Seq: 4,
				Type: core.EvWorkflowFailed, Payload: failed, CreatedAt: testNow},
		},
		Status: core.StatusCompensatingFailed, LastSeq: 4,
	}
	if err := h.store.CommitDecision(ctx, d); err != nil {
		t.Fatalf("seed a half-rolled-back saga: %v", err)
	}
	return run
}

// The dead end, asserted so the fix is measured against it rather than
// described.
func TestAHalfRolledBackSagaRefusesEverythingExceptTheResumeSignal(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()
	seedCompensatingFailed(t, h, "wf-stuck-rollback")
	req := func(name string) api.SignalRequest {
		return api.SignalRequest{ShardID: h.shard, WorkflowID: "wf-stuck-rollback", Name: name}
	}

	// An ordinary signal is still refused: the run is terminal and nothing
	// automatic should touch it.
	if err := h.engine.Signal(ctx, req("anything")); !errors.Is(err, core.ErrWorkflowNotFound) {
		t.Fatalf("ordinary signal: got %v, want ErrWorkflowNotFound", err)
	}
	// So is cancel, which travels the same path.
	if err := h.engine.Cancel(ctx, api.CancelRequest{
		ShardID: h.shard, WorkflowID: "wf-stuck-rollback", Reason: "give up",
	}); !errors.Is(err, core.ErrWorkflowNotFound) {
		t.Fatalf("cancel: got %v, want ErrWorkflowNotFound", err)
	}
	// And a fresh start is answered "already open", so the business key is
	// occupied by a run nobody can reach.
	res, err := h.engine.StartWorkflow(ctx, h.req("wf-stuck-rollback"))
	if err != nil || !res.AlreadyOpen {
		t.Fatalf("start over the stuck run: res=%+v err=%v, want AlreadyOpen", res, err)
	}
	if res.Status != core.StatusCompensatingFailed {
		t.Fatalf("start reported status %q, want COMPENSATING_FAILED", res.Status)
	}
}

func TestResumeCompensationReopensAHalfRolledBackSaga(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()
	run := seedCompensatingFailed(t, h, "wf-resume")

	reason := json.RawMessage(`{"operator":"on-call","note":"gateway is back"}`)
	if err := h.engine.ResumeCompensation(ctx, h.shard, "wf-resume", reason); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// The status is now COMPENSATING, which is what makes a rollback in
	// progress visible. It was dead code before: never assigned anywhere.
	ex, err := h.store.Get(ctx, h.shard, "wf-resume")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ex.Status != core.StatusCompensating {
		t.Fatalf("status = %q, want COMPENSATING", ex.Status)
	}
	if ex.Status.IsTerminal() {
		t.Fatal("COMPENSATING must not be terminal, or the run cannot be driven")
	}
	if ex.RunID != run {
		t.Fatalf("run id changed to %s; a resume must not start a new run", ex.RunID)
	}

	// Nothing was rewritten. The failure event is still there, which is the
	// point of recording the resume as its own event.
	hist, err := h.store.Load(ctx, h.shard, "wf-resume", run)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var sawFailure, sawResume, sawSignal bool
	for _, ev := range hist {
		switch ev.Type {
		case core.EvWorkflowFailed:
			sawFailure = true
		case core.EvCompensationResumed:
			sawResume = true
			// Compared as values, not as bytes: the payload column is jsonb,
			// so PostgreSQL stores a normalised form and returns the keys in
			// its own order. (This is the same reason the replay signature
			// digests a canonical form of an activity input rather than
			// comparing the stored bytes — see internal/workflow/canonical.go.)
			var got, want map[string]any
			if err := json.Unmarshal(ev.Payload, &got); err != nil {
				t.Errorf("resume payload is not decodable: %v", err)
			}
			_ = json.Unmarshal(reason, &want)
			if len(got) != len(want) {
				t.Errorf("resume payload = %s, want the operator's reason %s", ev.Payload, reason)
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("resume payload %s = %v, want %v", k, got[k], v)
				}
			}
		case core.EvSignalReceived:
			sawSignal = true
		}
	}
	if !sawFailure {
		t.Error("the original failure event was removed; history must stay append-only")
	}
	if !sawResume || !sawSignal {
		t.Fatalf("resume=%v signal=%v, want both events written", sawResume, sawSignal)
	}

	// And a workflow task exists, so the engine will actually drive the
	// remaining compensations rather than merely marking the row differently.
	got := h.shape(t, "wf-resume")
	if got.tasks != 1 {
		t.Fatalf("%d workflow task(s) after the resume, want 1", got.tasks)
	}

	// The run is now reachable by ordinary signals too, because it is no
	// longer terminal.
	if err := h.engine.Signal(ctx, api.SignalRequest{
		ShardID: h.shard, WorkflowID: "wf-resume", Name: "nudge",
	}); err != nil {
		t.Fatalf("ordinary signal after the resume: %v", err)
	}
}

// The resume signal must not be a back door into any other terminal run: a
// COMPLETED or FAILED workflow has no compensations owed and reopening one
// would be rewriting history, not recovering it.
func TestResumeCompensationIsRefusedOnAnyOtherTerminalStatus(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()

	for _, status := range []core.ExecutionStatus{core.StatusCompleted, core.StatusFailed} {
		wf := "wf-term-" + string(status)
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO executions (shard_id, workflow_id, run_id, workflow_type, status, last_seq)
			 VALUES ($1, $2, $3::uuid, 'DeliveryOrder', $4, 4)`,
			int32(h.shard), wf, uuid.NewString(), string(status)); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
		err := h.engine.ResumeCompensation(ctx, h.shard, wf, nil)
		if !errors.Is(err, core.ErrWorkflowNotFound) {
			t.Fatalf("resume on %s: got %v, want ErrWorkflowNotFound", status, err)
		}
	}
}

// Resuming twice must be harmless. An operator who clicks the button again, or
// a retried HTTP request, must not corrupt the run.
func TestResumingTwiceIsHarmless(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()
	run := seedCompensatingFailed(t, h, "wf-resume-twice")

	if err := h.engine.ResumeCompensation(ctx, h.shard, "wf-resume-twice", nil); err != nil {
		t.Fatalf("first resume: %v", err)
	}
	// The second call is now an ordinary signal to a COMPENSATING run, which
	// is accepted and simply wakes it again.
	if err := h.engine.ResumeCompensation(ctx, h.shard, "wf-resume-twice", nil); err != nil {
		t.Fatalf("second resume: %v", err)
	}

	ex, err := h.store.Get(ctx, h.shard, "wf-resume-twice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ex.Status != core.StatusCompensating || ex.RunID != run {
		t.Fatalf("execution = %q run %s, want COMPENSATING on run %s", ex.Status, ex.RunID, run)
	}
	// Sequence numbers are still strictly increasing with no gaps, which is
	// what proves neither commit collided or overwrote the other.
	hist, err := h.store.Load(ctx, h.shard, "wf-resume-twice", run)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for i, ev := range hist {
		if ev.Seq != core.Seq(i+1) {
			t.Fatalf("history is not contiguous at index %d: seq %d", i, ev.Seq)
		}
	}
	if int64(ex.LastSeq) != int64(len(hist)) {
		t.Fatalf("last_seq = %d but history has %d events", ex.LastSeq, len(hist))
	}
}

// A stale owner must not be able to resume anything. The resume is a write
// like any other and goes through the same fencing check.
func TestAStaleOwnerCannotResumeACompensation(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()
	seedCompensatingFailed(t, h, "wf-resume-fenced")

	h.setLease(t, 99) // the shard moved on; this node still holds 7
	before := h.shape(t, "wf-resume-fenced")

	if err := h.engine.ResumeCompensation(ctx, h.shard, "wf-resume-fenced", nil); !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("resume by a stale owner: got %v, want ErrLostOwnership", err)
	}
	after := h.shape(t, "wf-resume-fenced")
	if after.events != before.events || after.status != before.status || after.tasks != before.tasks {
		t.Fatalf("a fenced resume changed state: %+v -> %+v", before, after)
	}
	if h.fencing.count() != 1 {
		t.Fatalf("fencing rejections = %d, want 1", h.fencing.count())
	}
}
