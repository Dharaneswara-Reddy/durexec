package runtime

// Finding F-02, the external-effect guarantee, exercised end to end for the
// first time.
//
// WHAT WAS WRONG. activity.DedupKey is derived correctly and delivered to
// activity implementations through activity.Info, and NOTHING READ IT. A grep
// for InfoFromContext across app/ returned nothing; every shipped activity was
// an in-process sleep that incremented a counter. There was no HTTP call and no
// Idempotency-Key header anywhere in the repository, so the mechanism the
// project's "effectively-once" argument depends on had never executed once.
//
// Worse, the existing evidence could not have noticed. "Zero duplicate
// completed steps across 3,512 completed activities" is a statement about the
// event log's primary key, and the log cannot record a duplicate INVOCATION at
// all: every retry path in the engine — release-for-retry, claim lost, fenced,
// lapsed lock — returns without journaling anything. Zero duplicate completions
// is consistent with any number of duplicate invocations.
//
// WHAT THESE TESTS DO INSTEAD. They run the real ActivityWorker against a real
// PostgreSQL and a real HTTP service, and then ask the SERVICE how many effects
// it performed. The service's ledger is kept entirely separately from the
// engine's journal (internal/extsim), so the answer comes from a source the
// engine cannot write to. Each test prints invocations against effects, which
// is the comparison no measurement in this project had ever made.
//
// NOTHING HERE CLAIMS EXACTLY-ONCE EXTERNAL EXECUTION. The engine's ceiling is
// at-least-once invocation with a stable key. What is demonstrated is that the
// engine upholds its half — one key per logical effect, forwarded on every
// attempt — and what each failure actually costs. The cases where the guarantee
// does NOT hold are tested too, and named as such.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
	"github.com/dharaneswar339/durexec/internal/extsim"
	"github.com/dharaneswar339/durexec/internal/faultinj"
	"github.com/dharaneswar339/durexec/internal/taskq"
	"github.com/dharaneswar339/durexec/internal/testdb"
)

// ---- harness --------------------------------------------------------------

// effectHarness is a real activity worker, a real queue, a real store, and a
// real HTTP service, on one isolated shard.
type effectHarness struct {
	*startHarness

	svc    *extsim.Service
	server *httptest.Server
	queue  *taskq.Queue
	worker *ActivityWorker

	// dedupKey is the key the engine will derive for k=0 of this run, computed
	// the same way BuildDecision computes it so the test can ask the service
	// about that exact key.
	dedupKey   string
	workflowID string
	runID      core.RunID
}

// fastRetry is the default retry policy for these tests: the real shape, with
// the waiting taken out.
//
// THERE ARE TWO RETRY LAYERS and the tests below distinguish them, because
// which one absorbs a failure turned out to be a finding in itself:
//
//  1. The executor's own loop inside ONE worker invocation (MaximumAttempts).
//  2. The task being requeued and re-polled, possibly by another node, which is
//     what happens when layer 1 is exhausted or when the worker dies.
//
// A SERVICE-SIDE lost response can only ever reach layer 1. The service records
// the effect before failing, so the very next attempt finds the key in its
// ledger and succeeds — the key does its job so promptly that the task is never
// requeued at all. Reaching layer 2 therefore needs either a failure BEFORE the
// effect, or the ENGINE to be the thing that dies; both have tests of their
// own, and the crash-and-replay case uses the fault injector at the real commit
// boundary.
func fastRetry() activity.RetryPolicy {
	return activity.RetryPolicy{
		InitialInterval:    time.Millisecond,
		BackoffCoefficient: 1,
		MaximumInterval:    time.Millisecond,
		MaximumAttempts:    3,
	}
}

func newEffectHarness(t *testing.T, beh extsim.Behaviour) *effectHarness {
	return newEffectHarnessWithRetry(t, beh, fastRetry())
}

func newEffectHarnessWithRetry(t *testing.T, beh extsim.Behaviour, retry activity.RetryPolicy) *effectHarness {
	t.Helper()
	h := newStartHarness(t)

	svc := extsim.New(beh)
	server := httptest.NewServer(svc)
	t.Cleanup(server.Close)

	reg := activity.NewRegistry()
	// A short client timeout so the hang case does not make the test slow.
	def := extsim.PayDefinition(&http.Client{Timeout: 300 * time.Millisecond}, server.URL)
	def.Retry = retry
	if err := reg.Register(def); err != nil {
		t.Fatalf("register %s: %v", extsim.PayActivityName, err)
	}

	q := taskq.New(h.pool, taskq.DefaultConfig())
	h.engine.Tasks = q
	h.engine.Activities = reg
	h.engine.Executor = activity.NewExecutor(reg)

	wf := "wf-effect-" + uuid.NewString()[:8]
	eh := &effectHarness{
		startHarness: h,
		svc:          svc,
		server:       server,
		queue:        q,
		worker:       &ActivityWorker{Engine: h.engine, Shard: h.shard},
		workflowID:   wf,
		runID:        core.RunID(uuid.NewString()),
	}
	eh.dedupKey = activity.DedupKey(wf, eh.runID, 0)
	eh.seedScheduledActivity(t, 100, "acme-ltd")
	return eh
}

// seedScheduledActivity puts the run into the state an activity worker meets:
// a started execution, an ActivityTaskScheduled event at k=0, and a pollable
// ACTIVITY task pointing at it. It is written through CommitDecision, so the
// rows are exactly the ones the engine itself would create.
func (h *effectHarness) seedScheduledActivity(t *testing.T, amount int64, payee string) {
	t.Helper()
	ctx := context.Background()

	input, err := json.Marshal(extsim.PayInput{Amount: amount, Payee: payee})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	sched, err := json.Marshal(core.ActivityScheduled{
		K:            0,
		ActivityType: extsim.PayActivityName,
		Input:        input,
		DedupKey:     h.dedupKey,
		Mode:         core.ModeExact,
	})
	if err != nil {
		t.Fatalf("marshal scheduled: %v", err)
	}
	started, _ := json.Marshal(map[string]any{"workflow_type": "ExternalPay", "input": nil})

	d := core.Decision{
		ShardID: h.shard, WorkflowID: h.workflowID, RunID: h.runID, Token: h.token,
		CreateExecution: &core.NewExecution{WorkflowType: "ExternalPay"},
		Events: []core.Event{
			{ShardID: h.shard, WorkflowID: h.workflowID, RunID: h.runID, Seq: 1,
				Type: core.EvWorkflowStarted, Payload: started, CreatedAt: testNow},
			{ShardID: h.shard, WorkflowID: h.workflowID, RunID: h.runID, Seq: 2,
				Type: core.EvActivityScheduled, Payload: sched, CreatedAt: testNow},
		},
		NewTasks: []core.Task{{
			ShardID: h.shard, Type: core.TaskActivity,
			WorkflowID: h.workflowID, RunID: h.runID, ScheduledEventSeq: 2,
		}},
		Status: core.StatusRunning, LastSeq: 2,
	}
	if err := h.store.CommitDecision(ctx, d); err != nil {
		t.Fatalf("seed the scheduled activity: %v", err)
	}
}

// runOnce polls one ACTIVITY task and processes it. It reports whether there
// was a task to do, so a test can drive the worker until the queue is quiet
// exactly as the real loop does.
func (h *effectHarness) runOnce(t *testing.T) bool {
	t.Helper()
	ctx := context.Background()
	task, err := h.queue.Poll(ctx, h.shard, core.TaskActivity)
	if errors.Is(err, core.ErrNoTask) {
		return false
	}
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	// The worker's own error returns are loop control (shard gone, injected
	// crash); the test asserts on the database and the service, not on these.
	_ = h.worker.process(ctx, task)
	return true
}

// drive runs up to limit attempts, making each released task immediately
// visible so the test does not have to wait out the backoff.
func (h *effectHarness) drive(t *testing.T, limit int) int {
	t.Helper()
	attempts := 0
	for i := 0; i < limit; i++ {
		h.makeTasksVisible(t)
		if !h.runOnce(t) {
			break
		}
		attempts++
	}
	return attempts
}

// extsimQuiet is a perfectly behaved service, named so a test that only needs
// the harness's plumbing says so rather than passing an anonymous zero value.
func extsimQuiet() extsim.Behaviour { return extsim.Behaviour{} }

// seedExtraActivityTasks adds n more pollable ACTIVITY tasks for the same
// scheduling event, which is enough to prove a drained pool does not take new
// work. They are deliberately left behind for the next node.
func (h *effectHarness) seedExtraActivityTasks(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := h.pool.Exec(context.Background(),
			`INSERT INTO tasks (shard_id, task_type, workflow_id, run_id, scheduled_event_seq)
			 VALUES ($1, 'ACTIVITY', $2, $3::uuid, 2)`,
			int32(h.shard), h.workflowID, string(h.runID)); err != nil {
			t.Fatalf("seed extra activity task: %v", err)
		}
	}
}

func (h *effectHarness) makeTasksVisible(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE tasks SET visible_at = now() - interval '1 second', locked_until = NULL, lock_token = NULL
		  WHERE shard_id = $1 AND task_type = 'ACTIVITY'`, int32(h.shard)); err != nil {
		t.Fatalf("make tasks visible: %v", err)
	}
}

// journaled reports what the ENGINE recorded for k=0: the number of result
// events, and the effect id inside the last one.
func (h *effectHarness) journaled(t *testing.T) (completed int, failed int, out extsim.PayOutput) {
	t.Helper()
	hist, err := h.store.Load(context.Background(), h.shard, h.workflowID, h.runID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	for _, ev := range hist {
		switch ev.Type {
		case core.EvActivityCompleted:
			var p core.ActivityCompleted
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatalf("decode completion: %v", err)
			}
			completed++
			_ = json.Unmarshal(p.Result, &out)
		case core.EvActivityFailed:
			failed++
		}
	}
	return completed, failed, out
}

// report prints the comparison that the project's evidence has never contained.
func (h *effectHarness) report(t *testing.T, name string) {
	t.Helper()
	c := h.svc.Counters()
	completed, failed, _ := h.journaled(t)
	t.Logf("%s: service requests=%d performed=%d replayed=%d unkeyed=%d injected=%d | engine completions=%d failures=%d | effects for this key=%d",
		name, c.Requests, c.Performed, c.Replayed, c.Unkeyed, c.Injected,
		completed, failed, h.svc.EffectsForKey(h.dedupKey))
}

// ---- 1. the mechanism actually runs --------------------------------------

// The baseline, and the first time in this repository that a dedup key has
// left the process.
func TestTheDedupKeyReachesTheExternalServiceAsAnIdempotencyKey(t *testing.T) {
	h := newEffectHarness(t, extsim.Behaviour{})

	if !h.runOnce(t) {
		t.Fatal("no activity task to run")
	}
	completed, failed, out := h.journaled(t)
	if completed != 1 || failed != 0 {
		t.Fatalf("engine journaled completed=%d failed=%d, want 1 and 0", completed, failed)
	}
	if out.DedupKeyUsed != h.dedupKey {
		t.Fatalf("activity forwarded key %q, want %q", out.DedupKeyUsed, h.dedupKey)
	}
	if c := h.svc.Counters(); c.Performed != 1 || c.Unkeyed != 0 {
		t.Fatalf("service performed=%d unkeyed=%d, want 1 and 0", c.Performed, c.Unkeyed)
	}
	if n := h.svc.EffectsForKey(h.dedupKey); n != 1 {
		t.Fatalf("%d effects recorded for the key, want 1", n)
	}
	h.report(t, "healthy")
}

// An activity that cannot find its key must REFUSE rather than call without
// one. Calling unkeyed is how a retry becomes a second charge, and the service
// counter proves such a call would have been undeduplicable.
func TestAnActivityWithNoDedupKeyRefusesToPerformTheEffect(t *testing.T) {
	svc := extsim.New(extsim.Behaviour{})
	server := httptest.NewServer(svc)
	defer server.Close()

	def := extsim.PayDefinition(nil, server.URL)
	in, _ := json.Marshal(extsim.PayInput{Amount: 1, Payee: "x"})

	// No activity.Info in the context at all.
	_, err := def.Fn(context.Background(), in)
	if !errors.Is(err, extsim.ErrNoDedupKey) {
		t.Fatalf("got %v, want ErrNoDedupKey", err)
	}
	if !core.IsNonRetryable(err) {
		t.Error("the refusal must be non-retryable: retrying cannot produce a key")
	}
	if c := svc.Counters(); c.Requests != 0 {
		t.Fatalf("the service received %d request(s); the activity must not call without a key", c.Requests)
	}
}

// ---- 2. the effect happens and the engine never records it ---------------

// THE REQUIRED SCENARIO. The service performs the effect and then loses the
// response, so the worker cannot journal a result. The task comes back, the
// activity runs AGAIN with the same key, and the service answers from its own
// ledger. Two invocations, one effect.
func TestAnEffectThatLandsWithoutAResultIsNotDuplicatedOnReplay(t *testing.T) {
	h := newEffectHarness(t, extsim.Behaviour{LoseResponseAfterEffect: 1})

	h.drive(t, 4)

	// The ledger is the authority, and it is not the engine's.
	if n := h.svc.EffectsForKey(h.dedupKey); n != 1 {
		t.Fatalf("%d effects for one logical payment, want 1 — the dedup key did not absorb the retry", n)
	}
	c := h.svc.Counters()
	if c.Requests < 2 {
		t.Fatalf("service saw %d request(s), want at least 2 so the duplicate invocation really happened", c.Requests)
	}
	if c.Replayed < 1 {
		t.Fatal("the service never answered from its ledger, so the Idempotency-Key was not honoured")
	}

	// And the engine ends up with exactly one recorded completion, carrying the
	// SAME effect id the first call created.
	completed, _, out := h.journaled(t)
	if completed != 1 {
		t.Fatalf("engine journaled %d completions, want 1", completed)
	}
	if !out.ServiceReplayed {
		t.Error("the recorded result does not say the service replayed it; an operator cannot see the repeat")
	}
	effects := h.svc.Effects()
	if len(effects) != 1 || effects[0].EffectID != out.EffectID {
		t.Fatalf("recorded effect id %q, service holds %+v", out.EffectID, effects)
	}
	if !h.svc.Remembers(h.dedupKey) {
		t.Error("the service does not remember the key, so the repeat was absorbed by luck rather than by the key")
	}
	h.report(t, "lost response after effect")
}

// The same shape from the OTHER side: the service answers perfectly and the
// ENGINE is the one that dies, at the seam where the result is computed and
// nothing is durable. faultinj.BeforeDecisionCommit is now wired to that real
// boundary, which is what makes this testable at all.
func TestAnEffectWhoseResultCommitIsLostIsNotDuplicatedOnReplay(t *testing.T) {
	h := newEffectHarness(t, extsim.Behaviour{})

	inj := faultinj.New(true)
	inj.CrashAt(faultinj.BeforeDecisionCommit)
	h.store.UseFaultInjector(inj)
	t.Cleanup(func() { h.store.UseFaultInjector(nil) })

	// First attempt: the payment is made, the commit is rolled back.
	if !h.runOnce(t) {
		t.Fatal("no activity task to run")
	}
	if n := h.svc.EffectsForKey(h.dedupKey); n != 1 {
		t.Fatalf("%d effects after the first attempt, want 1", n)
	}
	if completed, _, _ := h.journaled(t); completed != 0 {
		t.Fatalf("engine journaled %d completions although the commit was rolled back, want 0", completed)
	}
	if inj.Fired() != 1 {
		t.Fatalf("injector fired %d times, want 1", inj.Fired())
	}

	// Recovery: the task was never deleted, so it comes back.
	h.makeTasksVisible(t)
	if !h.runOnce(t) {
		t.Fatal("the task did not come back after the rolled-back commit")
	}

	if n := h.svc.EffectsForKey(h.dedupKey); n != 1 {
		t.Fatalf("%d effects after recovery, want 1 — the money moved twice", n)
	}
	completed, _, out := h.journaled(t)
	if completed != 1 {
		t.Fatalf("engine journaled %d completions after recovery, want 1", completed)
	}
	if !out.ServiceReplayed {
		t.Error("the recorded result does not record that the service absorbed a repeat")
	}
	h.report(t, "engine commit lost after effect")
}

// ---- 3. timeouts ---------------------------------------------------------

// A timeout is indistinguishable from a lost response to the caller, and it is
// the common real-world shape. The effect lands, the client gives up, and the
// retry must not repeat it.
func TestAnEffectThatTimesOutIsNotDuplicatedOnRetry(t *testing.T) {
	h := newEffectHarness(t, extsim.Behaviour{
		HangAfterEffect: 1,
		Hang:            600 * time.Millisecond, // longer than the 300ms client timeout
	})

	h.drive(t, 4)

	if n := h.svc.EffectsForKey(h.dedupKey); n != 1 {
		t.Fatalf("%d effects after a timeout and a retry, want 1", n)
	}
	if c := h.svc.Counters(); c.Replayed < 1 {
		t.Fatal("the service never answered from its ledger after the timeout")
	}
	if completed, _, _ := h.journaled(t); completed != 1 {
		t.Fatalf("engine journaled %d completions, want 1", completed)
	}
	h.report(t, "timeout after effect")
}

// A failure BEFORE the effect is the benign case, and it is worth separating:
// here the retry SHOULD reach the service and perform the payment, because
// nothing happened the first time. A mechanism that suppressed this would be
// losing work rather than deduplicating it.
func TestAFailureBeforeTheEffectIsRetriedAndThePaymentHappensOnce(t *testing.T) {
	h := newEffectHarness(t, extsim.Behaviour{FailBeforeEffect: 2})

	h.drive(t, 6)
	if c := h.svc.Counters(); c.Injected < 2 {
		t.Fatalf("only %d pre-effect failure(s) were injected, want 2", c.Injected)
	}
	if n := h.svc.EffectsForKey(h.dedupKey); n != 1 {
		t.Fatalf("%d effects, want exactly 1: the payment must happen once, not zero times", n)
	}
	if completed, _, _ := h.journaled(t); completed != 1 {
		t.Fatalf("engine journaled %d completions, want 1", completed)
	}
	h.report(t, "failure before effect")
}

// ---- 4. concurrent invocations ------------------------------------------

// Two workers running the same logical step at once — which the engine permits
// for a window when a task lock lapses — must still produce one effect. The
// engine's task-row lock normally prevents it; this asserts the downstream
// half, with the lock deliberately out of the picture.
func TestConcurrentInvocationsOfOneKeyProduceOneEffect(t *testing.T) {
	h := newEffectHarness(t, extsim.Behaviour{})

	const callers = 12
	type result struct {
		out extsim.PayOutput
		err error
	}
	results := make([]result, callers)
	def := extsim.PayDefinition(nil, h.server.URL)
	in, _ := json.Marshal(extsim.PayInput{Amount: 100, Payee: "acme-ltd"})

	done := make(chan struct{})
	gate := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			<-gate
			ctx := activity.WithInfo(context.Background(), activity.Info{
				ActivityType: extsim.PayActivityName,
				DedupKey:     h.dedupKey,
				Attempt:      i + 1,
			})
			raw, err := def.Fn(ctx, in)
			results[i].err = err
			if err == nil {
				_ = json.Unmarshal(raw, &results[i].out)
			}
		}(i)
	}
	close(gate)
	for i := 0; i < callers; i++ {
		<-done
	}

	if n := h.svc.EffectsForKey(h.dedupKey); n != 1 {
		t.Fatalf("%d effects from %d concurrent invocations of one key, want 1", n, callers)
	}
	// Every caller must learn the SAME effect id, or a workflow could record a
	// payment that the service does not have under that identity.
	ids := map[string]int{}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("caller %d failed: %v", i, r.err)
		}
		ids[r.out.EffectID]++
	}
	if len(ids) != 1 {
		t.Fatalf("callers saw %d distinct effect ids, want 1: %v", len(ids), ids)
	}
	c := h.svc.Counters()
	t.Logf("concurrent: %d invocations of one key -> %d effect, %d answered from the ledger",
		c.Requests, c.Performed, c.Replayed)
}

// ---- 5. key expiry: where the guarantee FAILS ---------------------------

// This test asserts a DUPLICATE EFFECT, on purpose, and it is the most
// important one in the file.
//
// Real providers keep idempotency keys for a bounded window. A durable
// execution engine is the system most likely to exceed it: a step can be
// retried after a multi-day timer or a long outage. Past the window the key is
// unknown and the effect happens AGAIN, and no property of this engine prevents
// it — the key is stable forever, but the service has forgotten it.
//
// The engine cannot fix this by itself. Fixing it requires the activity to
// reconcile: to ask the service whether an effect already exists under the
// caller's own reference before performing a new one, which needs the service
// to be queryable that way. Nothing in this engine does that today. So this is
// recorded as a measured limitation rather than written up as a guarantee.
func TestKeyExpiryCausesADuplicateEffectAndIsNotPreventedByTheEngine(t *testing.T) {
	clock := testNow
	h := newEffectHarness(t, extsim.Behaviour{KeyTTL: time.Hour})
	h.svc.SetClock(func() time.Time { return clock })

	// The effect lands and the ENGINE loses the result, so the task survives
	// for a later attempt. A service-side failure would not do here: the key
	// makes the very next attempt succeed, so the task is never requeued and
	// no time passes.
	inj := faultinj.New(true)
	inj.CrashAt(faultinj.BeforeDecisionCommit)
	h.store.UseFaultInjector(inj)
	t.Cleanup(func() { h.store.UseFaultInjector(nil) })

	if !h.runOnce(t) {
		t.Fatal("no activity task to run")
	}
	if n := h.svc.EffectsForKey(h.dedupKey); n != 1 {
		t.Fatalf("%d effects after the first attempt, want 1", n)
	}
	if completed, _, _ := h.journaled(t); completed != 0 {
		t.Fatal("the engine recorded a completion although its commit was rolled back")
	}
	if !h.svc.Remembers(h.dedupKey) {
		t.Fatal("the service does not remember the key yet, so expiry is not what this test would measure")
	}

	// Two hours pass — a timer, an outage, an operator's lunch — and the
	// service's memory of the key is gone.
	clock = clock.Add(2 * time.Hour)
	h.makeTasksVisible(t)
	if !h.runOnce(t) {
		t.Fatal("the task did not come back")
	}

	n := h.svc.EffectsForKey(h.dedupKey)
	if n != 2 {
		t.Fatalf("%d effects after the key expired, want 2 — if this is 1 the simulator is not modelling expiry", n)
	}
	// The engine, meanwhile, is perfectly consistent and perfectly wrong about
	// the world: one completion, one recorded effect id, two payments made.
	completed, _, out := h.journaled(t)
	if completed != 1 {
		t.Fatalf("engine journaled %d completions, want 1", completed)
	}
	t.Logf("MEASURED LIMITATION: the key expired, so the payment was performed %d times "+
		"while the engine recorded 1 completion (effect %s). The engine cannot prevent this; "+
		"an activity would have to reconcile against the service, which nothing here does.",
		n, out.EffectID)
}

// The service also ignores a changed body under a replayed key, exactly as real
// providers do — so sending a different amount under the same key silently
// returns the FIRST amount. The engine's protection against that is the replay
// signature now covering the activity input (see internal/workflow); the
// service offers none, and this records which layer is doing the work.
func TestTheServiceIgnoresAChangedAmountUnderAReplayedKey(t *testing.T) {
	svc := extsim.New(extsim.Behaviour{})
	server := httptest.NewServer(svc)
	defer server.Close()
	def := extsim.PayDefinition(nil, server.URL)

	key := "k-fixed"
	ctx := activity.WithInfo(context.Background(), activity.Info{
		ActivityType: extsim.PayActivityName, DedupKey: key, Attempt: 1,
	})
	first, _ := json.Marshal(extsim.PayInput{Amount: 100, Payee: "acme-ltd"})
	second, _ := json.Marshal(extsim.PayInput{Amount: 200, Payee: "acme-ltd"})

	if _, err := def.Fn(ctx, first); err != nil {
		t.Fatalf("first call: %v", err)
	}
	raw, err := def.Fn(ctx, second)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	var out extsim.PayOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Amount != 100 {
		t.Fatalf("second call under the same key returned amount %d, want the first call's 100 "+
			"— if this changed, the simulator no longer models a real provider", out.Amount)
	}
	if !out.ServiceReplayed {
		t.Error("the service did not report the answer as replayed")
	}
	t.Log("MEASURED: a changed amount under a replayed key returns the FIRST amount with no error. " +
		"The downstream service cannot catch this; only the engine's input digest can.")
}

// ---- 6. the counter the engine lacks -------------------------------------

// A plain statement of F-02's structural point, asserted rather than argued:
// the engine's journal records one completion per k no matter how many times
// the activity was invoked, so the journal can never be evidence about
// invocations.
func TestTheEngineJournalCannotShowHowManyTimesAnActivityRan(t *testing.T) {
	h := newEffectHarness(t, extsim.Behaviour{FailBeforeEffect: 3})

	h.drive(t, 8)

	completed, failed, _ := h.journaled(t)
	c := h.svc.Counters()
	if completed != 1 {
		t.Fatalf("engine journaled %d completions, want 1", completed)
	}
	if c.Requests < 4 {
		t.Fatalf("service saw %d requests, want at least 4 so there is a gap to demonstrate", c.Requests)
	}
	t.Logf("STRUCTURAL: the activity was invoked %d times and the event log records %d completion(s) "+
		"and %d failure(s). No retry path in the engine journals anything, so the gap of %d "+
		"invocations is invisible to the log and to assert_no_duplicates.sql.",
		c.Requests, completed, failed, c.Requests-completed)

	if n := fmt.Sprint(h.svc.EffectsForKey(h.dedupKey)); n != "1" {
		t.Fatalf("effects for the key = %s, want 1", n)
	}
}

// compile-time proof the simulator is a real http.Handler, so the activity is
// exercising an actual HTTP round trip rather than a function call.
var _ http.Handler = (*extsim.Service)(nil)

// and that the store satisfies the committer the harness feeds it.
var _ core.Committer = (*eventlog.Store)(nil)

// testdb is referenced by the shared harness; this keeps the import honest if
// the harness ever moves.
var _ = testdb.DSN
