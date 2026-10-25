package runtime

// Regression tests for finding F-15: a task whose workflow type this binary
// does not register was retried for the life of the cluster.
//
// The dispatcher returns *workflow.NotRegisteredError, which fell into the
// generic error branch and released the task for another attempt — with no
// cap and no dead letter. The queue polled it every backoff interval forever,
// the execution stayed RUNNING, and nothing in the engine ever gave up or said
// anything an operator could act on.
//
// Some retrying IS correct: the common cause is deployment skew, a task for a
// new workflow type reaching a node that has not been upgraded, and another
// node can serve it. The two causes are indistinguishable from inside the
// worker, so the response is staged rather than chosen.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Below the cap: park and retry, because a rolling deployment would explain it
// and another node may well have the code.
func TestAnUnregisteredWorkflowTypeIsRetriedWhileADeploymentCouldExplainIt(t *testing.T) {
	hist := startedHistory("NoSuchWorkflow")
	e, commits, queue := engineForWorker(t, hist, runningExecution("NoSuchWorkflow", 1))
	w := &WorkflowWorker{Engine: e, Shard: 1}

	task := workflowTask()
	task.Attempt = 1
	if err := w.process(context.Background(), task); err != nil {
		t.Fatalf("process: %v", err)
	}
	if n := commits.commitCount(); n != 0 {
		t.Fatalf("%d decision(s) committed, want 0: nothing is known about this run yet", n)
	}
	if queue.releaseCount() != 1 {
		t.Fatalf("task released %d times, want 1", queue.releaseCount())
	}
	if rel := queue.released[0]; rel.visibleIn <= 0 {
		t.Errorf("released with visibleIn=%dms, want a backoff", rel.visibleIn)
	}
}

// At the cap, with nothing done in the world: dead letter. The run is failed,
// because a run that is undeliverable should be visibly failed rather than
// perpetually RUNNING, and there is nothing to compensate.
func TestAnUnregisteredWorkflowTypeIsDeadLetteredAfterTheCap(t *testing.T) {
	hist := startedHistory("NoSuchWorkflow")
	e, commits, queue := engineForWorker(t, hist, runningExecution("NoSuchWorkflow", 1))
	w := &WorkflowWorker{Engine: e, Shard: 1}

	task := workflowTask()
	task.Attempt = maxUnregisteredAttempts
	if err := w.process(context.Background(), task); err != nil {
		t.Fatalf("process: %v", err)
	}
	if queue.releaseCount() != 0 {
		t.Fatalf("task released %d times, want 0: the retrying has to stop somewhere", queue.releaseCount())
	}
	d, ok := commits.last()
	if !ok {
		t.Fatal("nothing was committed: the run is still RUNNING and still undeliverable")
	}
	if d.Status != core.StatusFailed {
		t.Fatalf("status = %q, want FAILED", d.Status)
	}
	if len(d.Events) != 1 || d.Events[0].Type != core.EvWorkflowFailed {
		t.Fatalf("want one WorkflowExecutionFailed event, got %+v", d.Events)
	}
	// The reason has to name the type, or an operator cannot tell which
	// deployment is missing.
	var p core.WorkflowFailed
	if err := json.Unmarshal(d.Events[0].Payload, &p); err != nil {
		t.Fatalf("decode failure: %v", err)
	}
	if !contains(p.Error, "NoSuchWorkflow") {
		t.Errorf("failure reason %q does not name the workflow type", p.Error)
	}
	if d.CompleteTaskID != 42 {
		t.Errorf("the task row must be cleared, got CompleteTaskID=%d", d.CompleteTaskID)
	}
}

// THE IMPORTANT CASE. At the cap, but the run has already done work: it must
// NOT be failed, because a status written from outside the workflow never runs
// the workflow's compensations, so a completed charge would be stranded with no
// refund. That is exactly the mistake the divergence path used to make. It
// parks permanently instead, loudly, for a human.
func TestAnUnregisteredTypeIsNeverFailedOnceTheRunHasDoneWork(t *testing.T) {
	hist := append(startedHistory("NoSuchWorkflow"),
		event(2, core.EvActivityScheduled, core.ActivityScheduled{
			K: 0, ActivityType: "ChargeCard", Input: json.RawMessage(`{"amount":100}`),
			DedupKey: "k0", Mode: core.ModeExact,
		}),
		event(3, core.EvActivityCompleted, core.ActivityCompleted{
			K: 0, Result: json.RawMessage(`{"charged":100}`),
		}))
	e, commits, queue := engineForWorker(t, hist, runningExecution("NoSuchWorkflow", 3))
	w := &WorkflowWorker{Engine: e, Shard: 1}

	task := workflowTask()
	task.Attempt = maxUnregisteredAttempts * 5 // far past the cap
	if err := w.process(context.Background(), task); err != nil {
		t.Fatalf("process: %v", err)
	}
	if n := commits.commitCount(); n != 0 {
		d, _ := commits.last()
		t.Fatalf("%d decision(s) committed for a run with completed work, want 0; last was %+v", n, d)
	}
	if queue.releaseCount() != 1 {
		t.Fatalf("task released %d times, want 1: it must stay parked, not be failed", queue.releaseCount())
	}
}

// hasCompletedWork is conservative on purpose: an activity that was SCHEDULED
// but never resolved still counts, because the effect may have happened without
// its result being journaled — which is precisely the engine's documented
// at-least-once ceiling.
func TestAScheduledButUnresolvedActivityCountsAsWorkDone(t *testing.T) {
	hist := append(startedHistory("NoSuchWorkflow"),
		event(2, core.EvActivityScheduled, core.ActivityScheduled{
			K: 0, ActivityType: "ChargeCard", Mode: core.ModeExact,
		}))
	if !hasCompletedWork(hist) {
		t.Fatal("a scheduled activity must count as work done: its effect may have landed unrecorded")
	}
	if hasCompletedWork(startedHistory("X")) {
		t.Fatal("a run with only a start event has done nothing")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
