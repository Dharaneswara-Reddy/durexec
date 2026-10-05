package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// heldBookingHistory is a run whose second booking was held for a person: the
// activity was scheduled, an ESCALATE verdict was journaled, and no task was
// created for it. That state — scheduled, no result, no task, with a reason in
// the log — is what "awaiting adjudication" actually looks like on disk.
func heldBookingHistory() []core.Event {
	return []core.Event{
		event(1, core.EvWorkflowStarted, map[string]any{"workflow_type": "TravelAgent", "input": json.RawMessage(`{}`)}),
		event(2, core.EvActivityScheduled, core.ActivityScheduled{
			K: 0, ActivityType: "BookFlight", Mode: core.ModeSemantic,
		}),
		event(3, core.EvActivityCompleted, core.ActivityCompleted{
			K: 0, Result: json.RawMessage(`{"booking_id":"bk_1"}`),
		}),
		event(4, core.EvDedupEvaluated, core.DedupEvaluated{
			K: 1, Verdict: core.VerdictEscalate, MatchedK: 0, Similarity: 0.87,
			TauLow: 0.85, TauHigh: 0.90, Model: "scripted",
		}),
		event(5, core.EvActivityScheduled, core.ActivityScheduled{
			K: 1, ActivityType: "BookFlight", Mode: core.ModeSemantic,
		}),
	}
}

func adjudication(a Adjudication) core.Event {
	return event(6, core.EvSignalReceived, core.SignalReceived{
		Name: AdjudicationSignal, Payload: mustJSON(a),
	})
}

func TestBuildPrefaceAdjudication(t *testing.T) {
	tests := []struct {
		name string
		hist []core.Event

		wantEvents []core.EventType
		wantTasks  int
	}{
		{
			// Nobody has answered yet, so nothing happens. The workflow stays
			// parked, which is the whole point of the strict policy.
			name: "an unanswered escalation produces nothing",
			hist: heldBookingHistory(),

			wantEvents: nil, wantTasks: 0,
		},
		{
			// Released. A superseding DISTINCT verdict is written so a second
			// workflow task cannot release the same activity twice, and the
			// activity task is finally created.
			name: "approval releases the activity",
			hist: append(heldBookingHistory(), adjudication(Adjudication{K: 1, Approved: true, Reviewer: "ops"})),

			wantEvents: []core.EventType{core.EvDedupEvaluated}, wantTasks: 1,
		},
		{
			// Confirmed a repeat. The matched call's recorded result becomes
			// this call's result, so the future resolves from history and the
			// side effect never happens.
			name: "rejection reuses the matched result",
			hist: append(heldBookingHistory(), adjudication(Adjudication{K: 1, Approved: false, Reviewer: "ops"})),

			wantEvents: []core.EventType{core.EvActivityCompleted}, wantTasks: 0,
		},
		{
			// Answering twice must not act twice: the first answer already
			// superseded the ESCALATE verdict.
			name: "a second answer changes nothing",
			hist: func() []core.Event {
				h := append(heldBookingHistory(), event(6, core.EvDedupEvaluated, core.DedupEvaluated{
					K: 1, Verdict: core.VerdictDistinct, MatchedK: 0, Model: AdjudicatedPrefix + "scripted",
				}))
				return append(h, adjudication(Adjudication{K: 1, Approved: true}))
			}(),

			wantEvents: nil, wantTasks: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := buildPreface(tc.hist, 10, testNow)

			if len(p.events) != len(tc.wantEvents) {
				t.Fatalf("events = %d, want %d (%+v)", len(p.events), len(tc.wantEvents), p.events)
			}
			for i, want := range tc.wantEvents {
				if p.events[i].Type != want {
					t.Errorf("event %d is %s, want %s", i, p.events[i].Type, want)
				}
				if p.events[i].Seq != core.Seq(10+i) {
					t.Errorf("event %d at seq %d, want %d", i, p.events[i].Seq, 10+i)
				}
			}
			if len(p.tasks) != tc.wantTasks {
				t.Errorf("tasks = %d, want %d", len(p.tasks), tc.wantTasks)
			}
			if tc.wantTasks > 0 && p.tasks[0].ScheduledEventSeq != 5 {
				t.Errorf("released task points at seq %d, want the scheduling event (5)",
					p.tasks[0].ScheduledEventSeq)
			}
		})
	}
}

// The timer scanner only enqueues a workflow task; it writes no event, because
// appending to history is the workflow worker's job alone. So the worker is
// the one that journals TimerFired, and until it does, the sleeping workflow
// replays and parks again.
func TestBuildPrefaceFiresDueTimers(t *testing.T) {
	sleeping := []core.Event{
		event(1, core.EvWorkflowStarted, map[string]any{"workflow_type": "TwoStep", "input": json.RawMessage(`{}`)}),
		event(2, core.EvTimerStarted, core.TimerStarted{
			K: 0, Duration: time.Hour, FireAt: testNow.Add(-time.Minute),
		}),
	}

	p := buildPreface(sleeping, 3, testNow)
	if len(p.events) != 1 || p.events[0].Type != core.EvTimerFired {
		t.Fatalf("a due timer must be journaled as fired, got %+v", p.events)
	}
	if p.events[0].Seq != 3 {
		t.Errorf("TimerFired at seq %d, want 3", p.events[0].Seq)
	}

	// Not yet due: firing early would hand the workflow an answer it has not
	// earned. A workflow task can arrive for other reasons — a signal, a
	// retried row — so this case is reachable in normal operation.
	early := []core.Event{
		sleeping[0],
		event(2, core.EvTimerStarted, core.TimerStarted{
			K: 0, Duration: time.Hour, FireAt: testNow.Add(time.Minute),
		}),
	}
	if p := buildPreface(early, 3, testNow); !p.empty() {
		t.Fatalf("a timer that is not due must not fire, got %+v", p.events)
	}

	// Already fired: a duplicate wake-up task survives a crash between the
	// scanner's enqueue and its delete, and firing twice would give the
	// workflow two answers at the same k.
	fired := append(sleeping, event(3, core.EvTimerFired, core.TimerFired{K: 0}))
	if p := buildPreface(fired, 4, testNow); !p.empty() {
		t.Fatalf("an already-fired timer must not fire again, got %+v", p.events)
	}
}

// stamp fills in the identity buildPreface deliberately does not know, so the
// builder can be tested from a slice of events alone.
func TestPrefaceStampAndLastSeq(t *testing.T) {
	p := buildPreface(append(heldBookingHistory(),
		adjudication(Adjudication{K: 1, Approved: true})), 10, testNow)
	p.stamp(3, "wf-x", "run-x")

	if got := p.lastSeq(10); got != 10 {
		t.Errorf("lastSeq = %d, want 10 (one event written, numbered 10)", got)
	}
	for _, ev := range p.events {
		if ev.ShardID != 3 || ev.WorkflowID != "wf-x" || ev.RunID != "run-x" {
			t.Errorf("event not stamped with the run identity: %+v", ev)
		}
	}
	for _, task := range p.tasks {
		if task.ShardID != 3 || task.WorkflowID != "wf-x" || task.RunID != "run-x" {
			t.Errorf("task not stamped with the run identity: %+v", task)
		}
	}
}
