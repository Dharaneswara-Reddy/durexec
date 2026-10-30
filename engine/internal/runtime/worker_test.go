package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/taskq"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// ---- a workflow registry for the tests ------------------------------------

// twoStepWorkflow schedules "Alpha", then "Beta", then returns.
func twoStepWorkflow(ctx workflow.Context, _ json.RawMessage) (json.RawMessage, error) {
	if _, err := ctx.ExecuteActivity("Alpha", map[string]string{"n": "1"}).Get(); err != nil {
		return nil, err
	}
	if _, err := ctx.ExecuteActivity("Beta", map[string]string{"n": "2"}).Get(); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"ok":true}`), nil
}

// divergentWorkflow asks for a different activity at k=0 than history records,
// which is what a redeployed, edited workflow looks like to the engine.
func divergentWorkflow(ctx workflow.Context, _ json.RawMessage) (json.RawMessage, error) {
	if _, err := ctx.ExecuteActivity("Gamma", nil).Get(); err != nil {
		return nil, err
	}
	return json.RawMessage(`null`), nil
}

func testDispatcher(t *testing.T) *workflow.Dispatcher {
	t.Helper()
	r := workflow.NewRegistry()
	r.MustRegister("TwoStep", twoStepWorkflow)
	r.MustRegister("Divergent", divergentWorkflow)
	return workflow.NewDispatcher(r)
}

// startedHistory is a run that has only just begun.
func startedHistory(wfType string) []core.Event {
	return []core.Event{event(1, core.EvWorkflowStarted, map[string]any{
		"workflow_type": wfType,
		"input":         json.RawMessage(`{}`),
	})}
}

func workflowTask() core.Task {
	return core.Task{
		ShardID: 1, TaskID: 42, Type: core.TaskWorkflow,
		WorkflowID: "wf-1", RunID: "run-1", ScheduledEventSeq: 1,
		LockToken: "8f14e45f-ceea-467a-9f6f-6a1b9c9d4e11", Attempt: 1,
	}
}

// engineForWorker assembles an engine whose every dependency is a fake.
func engineForWorker(t *testing.T, hist []core.Event, ex core.Execution) (*Engine, *fakeCommitter, *fakeQueue) {
	t.Helper()
	commits := &fakeCommitter{}
	queue := &fakeQueue{}
	return &Engine{
		Events:     &fakeEvents{hist: hist},
		Executions: &fakeExecutions{ex: ex},
		Committer:  commits,
		Tasks:      queue,
		Leases:     &fakeLeases{},
		Dispatcher: testDispatcher(t),
		ShardCount: 4,
		NodeID:     "node-test",
		Clock:      fixedClock{testNow},
		TokenFor:   func(core.ShardID) (core.FencingToken, bool) { return 7, true },
	}, commits, queue
}

func runningExecution(wfType string, lastSeq core.Seq) core.Execution {
	return core.Execution{
		ShardID: 1, WorkflowID: "wf-1", RunID: "run-1",
		WorkflowType: wfType, Status: core.StatusRunning, LastSeq: lastSeq,
	}
}

// ---- ownership ------------------------------------------------------------

func TestWorkflowWorkerStopsWhenTheShardIsLost(t *testing.T) {
	tests := []struct {
		name string
		// tokenPresent is whether this node still believes it owns the shard.
		tokenPresent bool
		// commitErr is what the committer says when the transaction runs.
		commitErr error

		wantStop bool
		// wantReleases is how many times the task was handed back. A fenced
		// commit must NOT release: our transaction rolled back, so the row is
		// still there, and the shard's new owner will poll it.
		wantReleases int
		wantCommits  int
	}{
		{
			// The lease went away before we even started. Hand the task
			// straight back rather than sitting on the lock until it expires.
			name:         "no token releases the task and stops",
			tokenPresent: false,
			wantStop:     true, wantReleases: 1, wantCommits: 0,
		},
		{
			// Fenced out mid-decision. This is expected, not exceptional:
			// another node owns the shard, our write was refused, and there is
			// nothing to retry and nothing to log as an error.
			name:         "a fenced commit is abandoned quietly",
			tokenPresent: true, commitErr: core.ErrLostOwnership,
			wantStop: true, wantReleases: 0, wantCommits: 0,
		},
		{
			// An ordinary database failure is different: we may well still own
			// the shard, so the task goes back with a backoff and is tried
			// again.
			name:         "an ordinary commit failure releases the task for retry",
			tokenPresent: true, commitErr: errBoom,
			wantStop: false, wantReleases: 1, wantCommits: 0,
		},
		{
			name:         "a healthy task commits once",
			tokenPresent: true,
			wantStop:     false, wantReleases: 0, wantCommits: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, commits, queue := engineForWorker(t, startedHistory("TwoStep"), runningExecution("TwoStep", 1))
			commits.err = tc.commitErr
			e.TokenFor = func(core.ShardID) (core.FencingToken, bool) {
				return 7, tc.tokenPresent
			}

			w := &WorkflowWorker{Engine: e, Shard: 1}
			err := w.process(context.Background(), workflowTask())

			if gotStop := errors.Is(err, errShardGone); gotStop != tc.wantStop {
				t.Errorf("stopped = %v, want %v (err %v)", gotStop, tc.wantStop, err)
			}
			if got := queue.releaseCount(); got != tc.wantReleases {
				t.Errorf("releases = %d, want %d", got, tc.wantReleases)
			}
			if got := len(commits.decisions); got != tc.wantCommits {
				t.Errorf("commits = %d, want %d", got, tc.wantCommits)
			}
		})
	}
}

// A healthy first task must schedule the first activity and nothing else.
func TestWorkflowWorkerCommitsTheFirstSchedule(t *testing.T) {
	e, commits, _ := engineForWorker(t, startedHistory("TwoStep"), runningExecution("TwoStep", 1))
	w := &WorkflowWorker{Engine: e, Shard: 1}

	if err := w.process(context.Background(), workflowTask()); err != nil {
		t.Fatalf("process: %v", err)
	}
	d, ok := commits.last()
	if !ok {
		t.Fatal("nothing was committed")
	}
	if d.Token != 7 {
		t.Errorf("decision carries token %d, want the one this node holds (7)", d.Token)
	}
	if d.CompleteTaskID != 42 {
		t.Errorf("decision completes task %d, want 42: the task row must be deleted in the same transaction",
			d.CompleteTaskID)
	}
	if len(d.Events) != 1 || d.Events[0].Type != core.EvActivityScheduled {
		t.Fatalf("want one ActivityTaskScheduled event, got %+v", d.Events)
	}
	if d.Events[0].Seq != 2 {
		t.Errorf("first new event at seq %d, want 2 (last_seq was 1)", d.Events[0].Seq)
	}
	if len(d.NewTasks) != 1 || d.NewTasks[0].Type != core.TaskActivity {
		t.Fatalf("want one activity task, got %+v", d.NewTasks)
	}
	if d.NewTasks[0].ScheduledEventSeq != 2 {
		t.Errorf("activity task points at seq %d, want 2", d.NewTasks[0].ScheduledEventSeq)
	}
}

// ---- non-determinism ------------------------------------------------------

// Divergence PARKS the run; it is never journaled as a terminal failure.
//
// This test asserted the opposite until the fix, and the old expectation was
// the defect: committing EvWorkflowFailed and StatusFailed set the status from
// outside the workflow, so the workflow's own error handling never ran and no
// saga compensation happened. A rolling deployment therefore terminally failed
// every in-flight run that landed on a new node and stranded whatever its
// forward steps had already done — a charged card with no refund. It also
// contradicted the dispatcher's own contract, which says returning the
// divergence "leaves history untouched, which is what lets an operator deploy
// the old code back and resume the run".
//
// The retry loop the old comment worried about is real: the task does come
// back and will diverge again. That is a visible stall with a counter on it
// (reasonDiverged) and an operator decision, which is strictly better than an
// invisible loss of money. Nothing automatic can be correct here, because only
// a human knows which deployment told the truth.
func TestWorkflowWorkerParksAndDoesNotJournalOnNonDeterminism(t *testing.T) {
	hist := append(startedHistory("Divergent"), event(2, core.EvActivityScheduled, core.ActivityScheduled{
		K: 0, ActivityType: "Alpha", Mode: core.ModeExact,
	}), event(3, core.EvActivityCompleted, core.ActivityCompleted{
		K: 0, Result: json.RawMessage(`"done"`),
	}))

	e, commits, queue := engineForWorker(t, hist, runningExecution("Divergent", 3))
	w := &WorkflowWorker{Engine: e, Shard: 1}

	if err := w.process(context.Background(), workflowTask()); err != nil {
		t.Fatalf("process: %v", err)
	}

	// Nothing was written. History is exactly as the old code left it, which is
	// what makes redeploying the old binary a recovery rather than a guess.
	if n := commits.commitCount(); n != 0 {
		d, _ := commits.last()
		t.Fatalf("%d decision(s) committed on divergence, want 0; last was %+v", n, d)
	}
	// And the task is back, so the run is parked rather than lost.
	if queue.releaseCount() != 1 {
		t.Fatalf("task released %d times, want 1: a parked run must return to the queue",
			queue.releaseCount())
	}
	// With a backoff, so a divergent run does not spin the fleet.
	if rel := queue.released[0]; rel.visibleIn <= 0 {
		t.Errorf("task released with visibleIn=%dms, want a backoff", rel.visibleIn)
	}
}

// The park backoff must hold for EVERY jitter draw, not most of them. With
// full jitter on attempt 1 (ceiling 100ms) a draw below 0.01 truncated to a
// visibility delay of 0ms, so the test above failed about once per hundred
// runs and a parked run came straight back. Pinning the draw to its minimum
// makes that case deterministic, for both reasons a run is parked.
func TestParkedRunBackoffHasAFloorEvenOnTheMinimumJitterDraw(t *testing.T) {
	saved := randFraction
	randFraction = func() float64 { return 0 }
	t.Cleanup(func() { randFraction = saved })

	floor := taskq.DefaultBackoff().Base.Milliseconds()

	hist := append(startedHistory("Divergent"), event(2, core.EvActivityScheduled, core.ActivityScheduled{
		K: 0, ActivityType: "Alpha", Mode: core.ModeExact,
	}), event(3, core.EvActivityCompleted, core.ActivityCompleted{
		K: 0, Result: json.RawMessage(`"done"`),
	}))
	e, _, queue := engineForWorker(t, hist, runningExecution("Divergent", 3))
	if err := (&WorkflowWorker{Engine: e, Shard: 1}).process(context.Background(), workflowTask()); err != nil {
		t.Fatalf("process: %v", err)
	}
	if queue.releaseCount() != 1 || queue.released[0].visibleIn < floor {
		t.Fatalf("divergent park released %+v, want one release with visibleIn >= %dms", queue.released, floor)
	}

	e, _, queue = engineForWorker(t, startedHistory("Divergent"), runningExecution("Divergent", 1))
	e.Dispatcher.CodeVersion = "v2"
	e.Dispatcher.AcceptVersion = func(recorded string) bool { return recorded == "v2" }
	if err := (&WorkflowWorker{Engine: e, Shard: 1}).process(context.Background(), workflowTask()); err != nil {
		t.Fatalf("process: %v", err)
	}
	if queue.releaseCount() != 1 || queue.released[0].visibleIn < floor {
		t.Fatalf("version park released %+v, want one release with visibleIn >= %dms", queue.released, floor)
	}
}

// A run this binary refuses to replay on version grounds is parked the same
// way, and for the same reason: another node running compatible code must be
// able to take the task, which is what makes a mixed-version rollout
// survivable instead of destructive.
func TestWorkflowWorkerParksARunItRefusesOnVersionGrounds(t *testing.T) {
	hist := startedHistory("Divergent")
	e, commits, queue := engineForWorker(t, hist, runningExecution("Divergent", 1))
	e.Dispatcher.CodeVersion = "v2"
	e.Dispatcher.AcceptVersion = func(recorded string) bool { return recorded == "v2" }

	w := &WorkflowWorker{Engine: e, Shard: 1}
	if err := w.process(context.Background(), workflowTask()); err != nil {
		t.Fatalf("process: %v", err)
	}
	if n := commits.commitCount(); n != 0 {
		t.Fatalf("%d decision(s) committed for a refused version, want 0", n)
	}
	if queue.releaseCount() != 1 {
		t.Fatalf("task released %d times, want 1", queue.releaseCount())
	}

	// Accepting the version lets the very same task proceed, so the refusal was
	// about the version and nothing else.
	e.Dispatcher.AcceptVersion = func(string) bool { return true }
	if err := w.process(context.Background(), workflowTask()); err != nil {
		t.Fatalf("process after accepting the version: %v", err)
	}
	if commits.commitCount() != 1 {
		t.Fatalf("%d decision(s) committed once the version is accepted, want 1", commits.commitCount())
	}
}

// ---- the activity worker --------------------------------------------------

func activityTask(schedSeq core.Seq) core.Task {
	return core.Task{
		ShardID: 1, TaskID: 43, Type: core.TaskActivity,
		WorkflowID: "wf-1", RunID: "run-1", ScheduledEventSeq: schedSeq,
		LockToken: "8f14e45f-ceea-467a-9f6f-6a1b9c9d4e11", Attempt: 1,
	}
}

func scheduledHistory() []core.Event {
	return append(startedHistory("TwoStep"), event(2, core.EvActivityScheduled, core.ActivityScheduled{
		K: 0, ActivityType: "Alpha", Input: json.RawMessage(`{"n":"1"}`),
		DedupKey: "dx1:abc", Mode: core.ModeExact,
	}))
}

// activityEngine wires an engine whose one activity behaves as the test says.
func activityEngine(t *testing.T, fn activity.Fn, policy activity.RetryPolicy) (*Engine, *fakeCommitter, *fakeQueue) {
	t.Helper()
	reg := activity.NewRegistry()
	reg.MustRegister(activity.Definition{Name: "Alpha", Fn: fn, Retry: policy})

	e, commits, queue := engineForWorker(t, scheduledHistory(), runningExecution("TwoStep", 2))
	e.Activities = reg
	e.Executor = &activity.Executor{
		Registry: reg,
		// No real waiting: the retry sequence under test is about which path
		// is taken, not about how long it takes.
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	return e, commits, queue
}

func TestActivityWorkerJournalsTheResultAndWakesTheWorkflow(t *testing.T) {
	e, commits, _ := activityEngine(t,
		func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"charged":true}`), nil
		}, activity.DefaultRetryPolicy())

	w := &ActivityWorker{Engine: e, Shard: 1}
	if err := w.process(context.Background(), activityTask(2)); err != nil {
		t.Fatalf("process: %v", err)
	}

	d, ok := commits.last()
	if !ok {
		t.Fatal("nothing was committed")
	}
	if len(d.Events) != 1 || d.Events[0].Type != core.EvActivityCompleted {
		t.Fatalf("want one ActivityTaskCompleted event, got %+v", d.Events)
	}
	if d.Events[0].Seq != 3 {
		t.Errorf("result written at seq %d, want 3", d.Events[0].Seq)
	}
	var p core.ActivityCompleted
	if err := json.Unmarshal(d.Events[0].Payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.K != 0 {
		t.Errorf("result recorded at k=%d, want the same k the schedule used (0)", p.K)
	}
	// The workflow is not running and cannot notice the result by itself.
	// Waking it has to be part of the same transaction.
	if len(d.NewTasks) != 1 || d.NewTasks[0].Type != core.TaskWorkflow {
		t.Fatalf("want one workflow task to wake the run, got %+v", d.NewTasks)
	}
	if d.CompleteTaskID != 43 {
		t.Errorf("the activity task must be deleted in the same transaction, got %d", d.CompleteTaskID)
	}
}

func TestActivityWorkerFailurePaths(t *testing.T) {
	tests := []struct {
		name string
		fn   activity.Fn
		// attempt is the task-level attempt count the queue reports.
		attempt int
		policy  activity.RetryPolicy

		wantCommits  int
		wantReleases int
		// wantNonRetryable is asserted only when a failure was journaled.
		wantNonRetryable bool
	}{
		{
			// Retries are left. Handing the row back is strictly better than
			// journaling a failure the workflow would have to compensate for:
			// the outage may be over by the time it becomes visible again.
			name: "a retryable failure with budget left releases the task",
			fn: func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return nil, errBoom
			},
			attempt: 1,
			policy:  activity.RetryPolicy{InitialInterval: time.Millisecond, BackoffCoefficient: 2, MaximumAttempts: 5},

			wantCommits: 0, wantReleases: 1,
		},
		{
			// The durable budget is spent. Now the failure is final and must
			// be journaled, so the workflow's own error handling can run.
			name: "an exhausted budget journals the failure",
			fn: func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return nil, errBoom
			},
			attempt: 5,
			policy:  activity.RetryPolicy{InitialInterval: time.Millisecond, BackoffCoefficient: 2, MaximumAttempts: 5},

			wantCommits: 1, wantReleases: 0,
		},
		{
			// No amount of waiting makes bad input good, so a non-retryable
			// failure skips both retry budgets.
			name: "a non-retryable failure is journaled immediately",
			fn: func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return nil, &core.NonRetryableError{Cause: errBoom}
			},
			attempt: 1,
			policy:  activity.RetryPolicy{InitialInterval: time.Millisecond, BackoffCoefficient: 2, MaximumAttempts: 5},

			wantCommits: 1, wantReleases: 0, wantNonRetryable: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, commits, queue := activityEngine(t, tc.fn, tc.policy)
			task := activityTask(2)
			task.Attempt = tc.attempt

			w := &ActivityWorker{Engine: e, Shard: 1}
			if err := w.process(context.Background(), task); err != nil {
				t.Fatalf("process: %v", err)
			}

			if got := len(commits.decisions); got != tc.wantCommits {
				t.Errorf("commits = %d, want %d", got, tc.wantCommits)
			}
			if got := queue.releaseCount(); got != tc.wantReleases {
				t.Errorf("releases = %d, want %d", got, tc.wantReleases)
			}
			if tc.wantCommits == 0 {
				return
			}
			d, _ := commits.last()
			if d.Events[0].Type != core.EvActivityFailed {
				t.Fatalf("want an ActivityTaskFailed event, got %s", d.Events[0].Type)
			}
			var p core.ActivityFailed
			if err := json.Unmarshal(d.Events[0].Payload, &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.NonRetryable != tc.wantNonRetryable {
				t.Errorf("non_retryable = %v, want %v: replay has to be able to tell a validation error from a timeout",
					p.NonRetryable, tc.wantNonRetryable)
			}
		})
	}
}

func TestActivityWorkerStopsWhenFenced(t *testing.T) {
	e, commits, queue := activityEngine(t,
		func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"ok"`), nil
		}, activity.DefaultRetryPolicy())
	commits.err = core.ErrLostOwnership

	w := &ActivityWorker{Engine: e, Shard: 1}
	err := w.process(context.Background(), activityTask(2))
	if !errors.Is(err, errShardGone) {
		t.Fatalf("a fenced activity commit must stop the worker, got %v", err)
	}
	if queue.releaseCount() != 0 {
		t.Error("a fenced worker must not release the task: the row was never touched")
	}
}

// An activity whose result is already in history must never run again. A
// duplicate task row is exactly what survives a crash between the commit and
// the delete, and acting on one would be the duplicate side effect the whole
// engine exists to prevent.
func TestActivityWorkerSkipsWorkThatIsAlreadyDone(t *testing.T) {
	called := false
	e, commits, _ := activityEngine(t,
		func(context.Context, json.RawMessage) (json.RawMessage, error) {
			called = true
			return json.RawMessage(`"ran twice"`), nil
		}, activity.DefaultRetryPolicy())

	hist := append(scheduledHistory(), event(3, core.EvActivityCompleted, core.ActivityCompleted{
		K: 0, Result: json.RawMessage(`"already"`),
	}))
	e.Events = &fakeEvents{hist: hist}

	w := &ActivityWorker{Engine: e, Shard: 1}
	if err := w.process(context.Background(), activityTask(2)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if called {
		t.Fatal("the activity ran a second time for a result that was already journaled")
	}
	d, ok := commits.last()
	if !ok {
		t.Fatal("the duplicate task row was not cleared")
	}
	if len(d.Events) != 0 {
		t.Errorf("clearing a duplicate task must write no events, got %+v", d.Events)
	}
	if d.CompleteTaskID != 43 {
		t.Errorf("CompleteTaskID = %d, want 43", d.CompleteTaskID)
	}
}

// ---- loop shutdown --------------------------------------------------------

func TestWorkerLoopsHonourContextCancellation(t *testing.T) {
	e, _, _ := engineForWorker(t, startedHistory("TwoStep"), runningExecution("TwoStep", 1))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{}, 2)
	go func() { (&WorkflowWorker{Engine: e, Shard: 1}).Run(ctx); done <- struct{}{} }()
	go func() { (&ActivityWorker{Engine: e, Shard: 1}).Run(ctx); done <- struct{}{} }()

	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("a worker did not return after its context was cancelled")
		}
	}
}
