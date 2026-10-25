package workflow

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Every test in this package builds its history by hand out of core.Event
// values. There is no database, no worker and no queue anywhere in here — the
// dispatcher's entire input is a slice of events and its entire output is a
// slice of commands, which is exactly what makes deterministic replay provable
// in isolation.

const (
	testShard = core.ShardID(7)
	testWFID  = "order-42"
	testRunID = core.RunID("11111111-1111-1111-1111-111111111111")
)

// baseTime is a fixed instant. Nothing in these tests reads a real clock, so a
// run in 2026 and a run in 2030 produce identical results.
var baseTime = time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)

// histBuilder appends events with increasing seq and increasing CreatedAt, one
// minute apart, so a test can assert on the workflow clock without arithmetic.
type histBuilder struct {
	t      *testing.T
	events []core.Event
	seq    core.Seq
}

func newHistory(t *testing.T) *histBuilder {
	t.Helper()
	b := &histBuilder{t: t}
	return b.add(core.EvWorkflowStarted, map[string]string{"workflow_type": "test"})
}

// startedAtVersion rewrites the start event to record a code version, which is
// how a real run carries the version of the code that began it. It edits the
// existing event rather than appending one, because a run has exactly one
// start event and its seq must stay 1.
func (b *histBuilder) startedAtVersion(version string) *histBuilder {
	b.t.Helper()
	if len(b.events) == 0 || b.events[0].Type != core.EvWorkflowStarted {
		b.t.Fatalf("startedAtVersion: event 1 is %v, not a start event", b.events[0].Type)
	}
	raw, err := json.Marshal(map[string]string{
		"workflow_type": "test",
		"code_version":  version,
	})
	if err != nil {
		b.t.Fatalf("marshalling start payload: %v", err)
	}
	b.events[0].Payload = raw
	return b
}

// at returns the CreatedAt that the nth appended event was given.
func at(n int) time.Time { return baseTime.Add(time.Duration(n) * time.Minute) }

func (b *histBuilder) add(typ core.EventType, payload any) *histBuilder {
	b.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		b.t.Fatalf("marshalling %s payload: %v", typ, err)
	}
	b.events = append(b.events, core.Event{
		ShardID:    testShard,
		WorkflowID: testWFID,
		RunID:      testRunID,
		Seq:        b.seq + 1,
		Type:       typ,
		Payload:    raw,
		CreatedAt:  at(int(b.seq)),
	})
	b.seq++
	return b
}

// activityScheduled records a call with no argument. Most tests want that;
// the replay signature now includes a digest of the input, so one that passes
// an argument has to say so with activityScheduledWith — a fixture recording
// "null" for a call the code makes with an argument is a divergence, and the
// engine is right to report it.
func (b *histBuilder) activityScheduled(k int, activityType string) *histBuilder {
	return b.activityScheduledWith(k, activityType, "null")
}

func (b *histBuilder) activityScheduledWith(k int, activityType, input string) *histBuilder {
	return b.add(core.EvActivityScheduled, core.ActivityScheduled{
		K:            k,
		ActivityType: activityType,
		Input:        json.RawMessage(input),
		DedupKey:     "test-key",
		Mode:         core.ModeExact,
	})
}

func (b *histBuilder) activityCompleted(k int, result string) *histBuilder {
	return b.add(core.EvActivityCompleted, core.ActivityCompleted{
		K: k, Result: json.RawMessage(result),
	})
}

func (b *histBuilder) activityFailed(k int, msg string, nonRetryable bool) *histBuilder {
	return b.add(core.EvActivityFailed, core.ActivityFailed{
		K: k, Error: msg, Attempt: 3, NonRetryable: nonRetryable,
	})
}

// activityDone is the common case: scheduled with no argument, then completed.
func (b *histBuilder) activityDone(k int, activityType, result string) *histBuilder {
	return b.activityScheduled(k, activityType).activityCompleted(k, result)
}

// activityDoneWith is activityDone for a call that carried an argument.
func (b *histBuilder) activityDoneWith(k int, activityType, input, result string) *histBuilder {
	return b.activityScheduledWith(k, activityType, input).activityCompleted(k, result)
}

func (b *histBuilder) timerStarted(k int, d time.Duration) *histBuilder {
	return b.add(core.EvTimerStarted, core.TimerStarted{
		K: k, Duration: d, FireAt: baseTime.Add(d),
	})
}

func (b *histBuilder) timerFired(k int) *histBuilder {
	return b.add(core.EvTimerFired, core.TimerFired{K: k})
}

func (b *histBuilder) signal(name, payload string) *histBuilder {
	return b.add(core.EvSignalReceived, core.SignalReceived{
		Name: name, Payload: json.RawMessage(payload),
	})
}

func (b *histBuilder) sideEffect(k int, value string) *histBuilder {
	return b.add(core.EvSideEffect, core.SideEffectRecorded{
		K: k, Value: json.RawMessage(value),
	})
}

func (b *histBuilder) done() []core.Event { return b.events }

// dispatcherFor registers one workflow under the name "test" and returns a
// dispatcher for it.
func dispatcherFor(t *testing.T, fn Fn) *Dispatcher {
	t.Helper()
	r := NewRegistry()
	if err := r.Register("test", fn); err != nil {
		t.Fatalf("registering workflow: %v", err)
	}
	return NewDispatcher(r)
}

func run(t *testing.T, d *Dispatcher, events []core.Event) Result {
	t.Helper()
	res, err := d.Execute(events, "test", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	return res
}

// commandSummary renders commands as compact strings so a test can assert on a
// whole task's output in one comparison.
func commandSummary(cmds []core.Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		switch c.Type {
		case core.CmdScheduleActivity:
			out = append(out, fmt.Sprintf("ScheduleActivity k=%d %s", c.K, c.ActivityType))
		case core.CmdStartTimer:
			out = append(out, fmt.Sprintf("StartTimer k=%d %s", c.K, c.Duration))
		case core.CmdWaitSignal:
			out = append(out, fmt.Sprintf("WaitSignal k=%d %s", c.K, c.Signal))
		case core.CmdRecordSideEffect:
			out = append(out, fmt.Sprintf("RecordSideEffect k=%d %s", c.K, string(c.Value)))
		case core.CmdCompleteWorkflow:
			out = append(out, fmt.Sprintf("CompleteWorkflow %s", string(c.Result)))
		case core.CmdFailWorkflow:
			out = append(out, fmt.Sprintf("FailWorkflow %s", c.Err))
		default:
			out = append(out, string(c.Type))
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
