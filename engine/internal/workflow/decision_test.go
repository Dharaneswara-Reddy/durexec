package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
)

// commitTime is the wall clock at the moment the decision is built. It is
// deliberately different from baseTime so the tests can tell the workflow's
// replay clock and the real clock apart.
var commitTime = time.Date(2026, time.March, 4, 9, 30, 0, 0, time.UTC)

func testParams() DecisionParams {
	return DecisionParams{
		ShardID:        testShard,
		WorkflowID:     testWFID,
		RunID:          testRunID,
		Token:          core.FencingToken(9),
		LastSeq:        core.Seq(3),
		CompleteTaskID: 1234,
		Now:            commitTime,
	}
}

func TestBuildDecisionScheduleActivity(t *testing.T) {
	res := Result{
		Status: core.StatusRunning,
		Commands: []core.Command{{
			Type:         core.CmdScheduleActivity,
			K:            2,
			ActivityType: "Charge",
			Input:        json.RawMessage(`{"amount":100}`),
			Mode:         core.ModeSemantic,
		}},
	}

	d, err := BuildDecision(testParams(), res)
	if err != nil {
		t.Fatalf("BuildDecision: %v", err)
	}

	if d.ShardID != testShard || d.WorkflowID != testWFID || d.RunID != testRunID || d.Token != 9 {
		t.Fatalf("identity not carried through: %+v", d)
	}
	if d.CompleteTaskID != 1234 {
		t.Fatalf("CompleteTaskID = %d", d.CompleteTaskID)
	}
	if len(d.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(d.Events))
	}

	ev := d.Events[0]
	if ev.Type != core.EvActivityScheduled {
		t.Fatalf("event type = %q", ev.Type)
	}
	// Numbering continues from the history that was replayed. The primary key
	// on (shard, workflow, run, seq) is what makes two workers racing on the
	// same task collide rather than both succeeding.
	if ev.Seq != 4 || d.LastSeq != 4 {
		t.Fatalf("seq = %d, LastSeq = %d, want 4 and 4", ev.Seq, d.LastSeq)
	}
	if !ev.CreatedAt.Equal(commitTime) {
		t.Fatalf("CreatedAt = %v, want the commit-time clock", ev.CreatedAt)
	}

	var p core.ActivityScheduled
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.K != 2 || p.ActivityType != "Charge" || string(p.Input) != `{"amount":100}` {
		t.Fatalf("payload = %+v", p)
	}
	if p.Mode != core.ModeSemantic {
		t.Fatalf("mode = %q", p.Mode)
	}
	// The dedup key is stamped here because it needs the run identity, which
	// the dispatcher deliberately does not have.
	if want := activity.DedupKey(testWFID, testRunID, 2); p.DedupKey != want {
		t.Fatalf("dedup key = %q, want %q", p.DedupKey, want)
	}

	if len(d.NewTasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(d.NewTasks))
	}
	task := d.NewTasks[0]
	if task.Type != core.TaskActivity || task.ScheduledEventSeq != 4 {
		t.Fatalf("task = %+v", task)
	}
	if task.WorkflowID != testWFID || task.RunID != testRunID || task.ShardID != testShard {
		t.Fatalf("task identity = %+v", task)
	}
}

func TestBuildDecisionStartTimer(t *testing.T) {
	res := Result{
		Status: core.StatusRunning,
		Commands: []core.Command{{
			Type: core.CmdStartTimer, K: 0, Duration: 72 * time.Hour,
		}},
	}

	d, err := BuildDecision(testParams(), res)
	if err != nil {
		t.Fatalf("BuildDecision: %v", err)
	}
	if len(d.NewTimers) != 1 {
		t.Fatalf("timers = %d, want 1", len(d.NewTimers))
	}
	tm := d.NewTimers[0]
	// FireAt is computed from the real clock, not from the workflow's replay
	// clock. Using the replay clock would schedule a three-day sleep relative
	// to an instant three days ago, and it would fire at once.
	if want := commitTime.Add(72 * time.Hour); !tm.FireAt.Equal(want) {
		t.Fatalf("FireAt = %v, want %v", tm.FireAt, want)
	}
	if tm.TimerSeq != d.Events[0].Seq {
		t.Fatalf("timer must be keyed by the seq of its own TimerStarted event")
	}

	var p core.TimerStarted
	if err := json.Unmarshal(d.Events[0].Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Duration != 72*time.Hour || !p.FireAt.Equal(tm.FireAt) {
		t.Fatalf("payload = %+v", p)
	}
}

func TestBuildDecisionTerminalAndSideEffect(t *testing.T) {
	res := Result{
		Status: core.StatusCompleted,
		Output: json.RawMessage(`{"ok":true}`),
		Commands: []core.Command{
			{Type: core.CmdRecordSideEffect, K: 0, Value: json.RawMessage(`"uuid"`)},
			{Type: core.CmdCompleteWorkflow, K: terminalK, Result: json.RawMessage(`{"ok":true}`)},
		},
	}

	d, err := BuildDecision(testParams(), res)
	if err != nil {
		t.Fatalf("BuildDecision: %v", err)
	}
	if len(d.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(d.Events))
	}
	if d.Events[0].Type != core.EvSideEffect || d.Events[1].Type != core.EvWorkflowCompleted {
		t.Fatalf("event types = %q, %q", d.Events[0].Type, d.Events[1].Type)
	}
	if d.Events[0].Seq != 4 || d.Events[1].Seq != 5 || d.LastSeq != 5 {
		t.Fatalf("seqs = %d, %d, last %d", d.Events[0].Seq, d.Events[1].Seq, d.LastSeq)
	}
	if d.Status != core.StatusCompleted {
		t.Fatalf("status = %q", d.Status)
	}
	// A completion creates no follow-up work.
	if len(d.NewTasks) != 0 || len(d.NewTimers) != 0 {
		t.Fatalf("a terminal decision must schedule nothing")
	}
}

// TestBuildDecisionWaitSignalWritesNothing: a signal wait has no event of its
// own. Writing one would append a fresh row on every workflow task forever,
// since nothing in history would ever suppress it.
func TestBuildDecisionWaitSignalWritesNothing(t *testing.T) {
	res := Result{
		Status:   core.StatusRunning,
		Commands: []core.Command{{Type: core.CmdWaitSignal, K: 0, Signal: "approval"}},
	}
	d, err := BuildDecision(testParams(), res)
	if err != nil {
		t.Fatalf("BuildDecision: %v", err)
	}
	if len(d.Events) != 0 || len(d.NewTasks) != 0 {
		t.Fatalf("expected an empty decision, got %+v", d)
	}
	if d.LastSeq != 3 {
		t.Fatalf("LastSeq = %d, want the history's own last seq", d.LastSeq)
	}
}

// TestExecuteThenBuildDecision is the end-to-end shape a worker uses: replay,
// then turn the outcome into one atomic unit of work.
func TestExecuteThenBuildDecision(t *testing.T) {
	d := dispatcherFor(t, threeActivities)
	h := newHistory(t).activityDone(0, "Reserve", `{"hold":"h1"}`)

	res := run(t, d, h.done())
	dec, err := BuildDecision(DecisionParams{
		ShardID:    testShard,
		WorkflowID: testWFID,
		RunID:      testRunID,
		Token:      1,
		LastSeq:    core.Seq(len(h.done())),
		Now:        commitTime,
	}, res)
	if err != nil {
		t.Fatalf("BuildDecision: %v", err)
	}

	if dec.Status != core.StatusRunning {
		t.Fatalf("status = %q", dec.Status)
	}
	if len(dec.Events) != 1 || dec.Events[0].Seq != 4 {
		t.Fatalf("events = %+v", dec.Events)
	}
	if len(dec.NewTasks) != 1 || dec.NewTasks[0].ScheduledEventSeq != 4 {
		t.Fatalf("tasks = %+v", dec.NewTasks)
	}
}

func TestBuildDecisionRejectsDedupCommandWithoutVerdict(t *testing.T) {
	res := Result{Commands: []core.Command{{Type: core.CmdRecordDedup, K: 1}}}
	if _, err := BuildDecision(testParams(), res); err == nil {
		t.Fatal("expected an error for a dedup command with no verdict attached")
	}
}
