package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// checkerFor builds a checker whose verdict for one activity call is decided
// by the similarity the test names.
func checkerFor(sim float64, intentText string, thresholds core.Thresholds) (*semantic.Checker, *fakeIntents) {
	intents := &fakeIntents{rows: []core.Intent{{
		ShardID: 1, WorkflowID: "wf-1", RunID: "run-1", ScheduledSeq: 2,
		ActivityType: "BookFlight", IntentText: "earlier call", Embedding: candidateVector(),
	}}}
	emb := &scriptedEmbedder{sims: map[string]float64{intentText: sim}}
	return &semantic.Checker{
		Embedder: emb, Intents: intents, Thresholds: thresholds, Clock: fixedClock{testNow},
	}, intents
}

// historyWithPriorBooking is a run that has already booked something at k=0,
// so a second booking has a candidate to be compared against.
func historyWithPriorBooking() []core.Event {
	return []core.Event{
		event(1, core.EvWorkflowStarted, map[string]any{"workflow_type": "TravelAgent", "input": json.RawMessage(`{}`)}),
		event(2, core.EvActivityScheduled, core.ActivityScheduled{
			K: 0, ActivityType: "BookFlight", Input: json.RawMessage(`{"args":"first"}`), Mode: core.ModeSemantic,
		}),
		event(3, core.EvActivityCompleted, core.ActivityCompleted{
			K: 0, Result: json.RawMessage(`{"booking_id":"bk_1"}`),
		}),
	}
}

// secondBooking is the schedule command the workflow is about to emit.
func secondBooking() workflow.Result {
	return workflow.Result{
		Status: core.StatusRunning,
		Commands: []core.Command{{
			Type: core.CmdScheduleActivity, K: 1, ActivityType: "BookFlight",
			Input: json.RawMessage(`{"args":"second"}`), Mode: core.ModeSemantic,
		}},
	}
}

// secondBookingIntent is the exact text the checker will embed for it, built
// by the same function the checker uses so the two cannot drift apart.
func secondBookingIntent(t *testing.T) string {
	t.Helper()
	text, err := semantic.IntentText("BookFlight", json.RawMessage(`{"args":"second"}`))
	if err != nil {
		t.Fatalf("IntentText: %v", err)
	}
	return text
}

func TestResolveSemanticFollowsTheVerdict(t *testing.T) {
	thresholds := core.Thresholds{TauLow: 0.85, TauHigh: 0.90}
	intentText := secondBookingIntent(t)

	tests := []struct {
		name string
		sim  float64
		// blocks is the ESCALATION_BLOCKS setting under test.
		blocks bool

		wantVerdict core.DedupVerdict
		// wantReuse is whether the prior result is reused in place of running
		// the activity.
		wantReuse bool
		// wantBlocked is whether the activity is held for a human.
		wantBlocked bool
		// wantEscalated is whether the index is recorded in the band, which
		// must happen regardless of which policy is in force.
		wantEscalated bool
	}{
		{
			// Below TauLow. Genuinely new work, so the schedule command is
			// left exactly as the workflow produced it.
			name: "distinct runs the activity", sim: 0.10,
			wantVerdict: core.VerdictDistinct,
		},
		{
			// At or above TauHigh. The engine has done this before in this
			// run, so the recorded result is reused and the side effect never
			// happens a second time.
			name: "duplicate reuses the recorded result", sim: 0.97,
			wantVerdict: core.VerdictDuplicate, wantReuse: true,
		},
		{
			// Inside the band, default policy: the activity runs. Being wrong
			// in this direction costs a repeated call that the dedup key still
			// protects; being wrong the other way silently drops a real
			// action, which is the severe failure.
			name: "escalate executes by default", sim: 0.87,
			wantVerdict: core.VerdictEscalate, wantEscalated: true,
		},
		{
			// Inside the band, strict policy: the activity is held and the run
			// waits for a person.
			name: "escalate blocks when configured", sim: 0.87, blocks: true,
			wantVerdict: core.VerdictEscalate, wantEscalated: true, wantBlocked: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checker, _ := checkerFor(tc.sim, intentText, thresholds)
			e := &Engine{Checker: checker, EscalationBlocks: tc.blocks, Clock: fixedClock{testNow}}

			res := secondBooking()
			hist := historyWithPriorBooking()
			out, err := e.resolveSemantic(context.Background(), hist, &res, semanticParams{
				ShardID: 1, WorkflowID: "wf-1", RunID: "run-1",
			})
			if err != nil {
				t.Fatalf("resolveSemantic: %v", err)
			}

			if len(out.verdicts) != 1 {
				t.Fatalf("want one journaled verdict, got %d", len(out.verdicts))
			}
			got := out.verdicts[0]
			if got.Type != core.CmdRecordDedup || got.Dedup == nil {
				t.Fatalf("verdict command is not a dedup record: %+v", got)
			}
			if got.Dedup.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q (similarity %.2f)", got.Dedup.Verdict, tc.wantVerdict, tc.sim)
			}

			// The schedule command is never removed. Whether the activity
			// actually runs is decided by applyOutcome, from the outcome.
			if len(res.Commands) != 1 || res.Commands[0].Type != core.CmdScheduleActivity {
				t.Fatalf("the schedule command must survive the check: %+v", res.Commands)
			}

			if gotReuse := len(out.reuse) > 0; gotReuse != tc.wantReuse {
				t.Errorf("reuse = %v, want %v", gotReuse, tc.wantReuse)
			}
			if tc.wantReuse {
				if string(out.reuse[1]) != `{"booking_id":"bk_1"}` {
					t.Errorf("reused result = %s, want the prior booking", out.reuse[1])
				}
			}
			if gotBlocked := out.block[1]; gotBlocked != tc.wantBlocked {
				t.Errorf("blocked = %v, want %v", gotBlocked, tc.wantBlocked)
			}
			if gotEscalated := len(out.escalate) > 0; gotEscalated != tc.wantEscalated {
				t.Errorf("escalate recorded = %v, want %v", gotEscalated, tc.wantEscalated)
			}
		})
	}
}

// A verdict already in history is the answer. Recomputing it would make
// recovery depend on the model being reachable, which is exactly what
// journaling it was meant to avoid — and a model that answered differently the
// second time would rewrite the past.
func TestResolveSemanticNeverRecomputesAJournaledVerdict(t *testing.T) {
	intentText := secondBookingIntent(t)
	checker, _ := checkerFor(0.97, intentText, core.Thresholds{TauLow: 0.85, TauHigh: 0.90})
	// If the checker is consulted at all, this error surfaces.
	checker.Embedder = &scriptedEmbedder{err: errBoom}

	e := &Engine{Checker: checker, Clock: fixedClock{testNow}}

	hist := append(historyWithPriorBooking(), event(4, core.EvDedupEvaluated, core.DedupEvaluated{
		K: 1, Verdict: core.VerdictDuplicate, MatchedK: 0, Similarity: 0.97,
		TauLow: 0.85, TauHigh: 0.90, Model: "scripted",
	}))

	res := secondBooking()
	out, err := e.resolveSemantic(context.Background(), hist, &res, semanticParams{
		ShardID: 1, WorkflowID: "wf-1", RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("resolveSemantic consulted the model on replay: %v", err)
	}
	if len(out.verdicts) != 0 {
		t.Fatalf("a recorded verdict must not be written again, got %d new verdicts", len(out.verdicts))
	}
	if len(out.reuse) != 0 || len(out.block) != 0 {
		t.Fatalf("replay must make no new decisions: reuse=%v block=%v", out.reuse, out.block)
	}
}

// An exact-mode activity is never sent to the model at all. Semantic matching
// is opt-in because its failure mode is asymmetric, and a test that let an
// exact activity slip through would hide that.
func TestResolveSemanticIgnoresExactModeActivities(t *testing.T) {
	checker, _ := checkerFor(0.99, "unused", core.DefaultThresholds())
	checker.Embedder = &scriptedEmbedder{err: errBoom}
	e := &Engine{Checker: checker, Clock: fixedClock{testNow}}

	res := workflow.Result{Commands: []core.Command{{
		Type: core.CmdScheduleActivity, K: 0, ActivityType: "ChargePayment",
		Input: json.RawMessage(`{"amount_cents":2450}`), Mode: core.ModeExact,
	}}}

	out, err := e.resolveSemantic(context.Background(), nil, &res, semanticParams{ShardID: 1})
	if err != nil {
		t.Fatalf("an exact-mode activity reached the model: %v", err)
	}
	if len(out.verdicts) != 0 {
		t.Fatalf("no verdict should be produced for an exact-mode activity, got %d", len(out.verdicts))
	}
}

// ---- applyOutcome ---------------------------------------------------------

// decisionForTest is one workflow task's decision: last_seq was 3, the task
// scheduled one activity at k=1, and the activity task points at its own
// scheduling event.
func decisionForTest() core.Decision {
	return core.Decision{
		ShardID: 1, WorkflowID: "wf-1", RunID: "run-1", Token: 7,
		Events: []core.Event{event(4, core.EvActivityScheduled, core.ActivityScheduled{
			K: 1, ActivityType: "BookFlight", Mode: core.ModeSemantic,
		})},
		NewTasks: []core.Task{{
			ShardID: 1, Type: core.TaskActivity, WorkflowID: "wf-1", RunID: "run-1",
			ScheduledEventSeq: 4,
		}},
		Status: core.StatusRunning, LastSeq: 4,
	}
}

func TestApplyOutcome(t *testing.T) {
	tests := []struct {
		name string
		out  semanticOutcome

		wantTasks   int
		wantEvents  int
		wantLastSeq core.Seq
		// wantCompletionAt is the seq the reused completion must land on, or 0
		// for "no completion expected".
		wantCompletionAt core.Seq
	}{
		{
			// Nothing was deduplicated, so the decision is untouched: the
			// activity task stands and no event is invented.
			name: "no verdicts leaves the decision alone",
			out:  semanticOutcome{reuse: map[int]json.RawMessage{}, block: map[int]bool{}},

			wantTasks: 1, wantEvents: 1, wantLastSeq: 4,
		},
		{
			// DUPLICATE: the task is dropped and a completion carrying the
			// reused result is journaled at the NEXT sequence number, so the
			// following replay resolves the future from history exactly as if
			// the activity had run.
			name: "duplicate drops the task and journals a completion",
			out: semanticOutcome{
				reuse: map[int]json.RawMessage{1: json.RawMessage(`{"booking_id":"bk_1"}`)},
				block: map[int]bool{},
			},

			wantTasks: 0, wantEvents: 2, wantLastSeq: 5, wantCompletionAt: 5,
		},
		{
			// Held for a human: the task is dropped and NOTHING replaces it.
			// last_seq must not move, because no event was written, and the
			// workflow stays parked on an unresolved future.
			name: "blocked escalation drops the task and writes nothing",
			out: semanticOutcome{
				reuse: map[int]json.RawMessage{},
				block: map[int]bool{1: true},
			},

			wantTasks: 0, wantEvents: 1, wantLastSeq: 4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := decisionForTest()
			if err := applyOutcome(&d, tc.out, testNow.UnixNano()); err != nil {
				t.Fatalf("applyOutcome: %v", err)
			}
			if len(d.NewTasks) != tc.wantTasks {
				t.Errorf("tasks = %d, want %d", len(d.NewTasks), tc.wantTasks)
			}
			if len(d.Events) != tc.wantEvents {
				t.Errorf("events = %d, want %d", len(d.Events), tc.wantEvents)
			}
			if d.LastSeq != tc.wantLastSeq {
				t.Errorf("last_seq = %d, want %d", d.LastSeq, tc.wantLastSeq)
			}
			if tc.wantCompletionAt == 0 {
				return
			}
			last := d.Events[len(d.Events)-1]
			if last.Type != core.EvActivityCompleted {
				t.Fatalf("last event is %s, want %s", last.Type, core.EvActivityCompleted)
			}
			if last.Seq != tc.wantCompletionAt {
				t.Errorf("completion seq = %d, want %d", last.Seq, tc.wantCompletionAt)
			}
			var p core.ActivityCompleted
			if err := json.Unmarshal(last.Payload, &p); err != nil {
				t.Fatalf("decode completion: %v", err)
			}
			if p.K != 1 {
				t.Errorf("completion k = %d, want 1", p.K)
			}
		})
	}
}

// Two reused results must be numbered in a fixed order. Map iteration in Go is
// random, and these events go into an append-only log, so two workers
// replaying the same task have to agree on which seq belongs to which k.
func TestApplyOutcomeNumbersReusedResultsInIndexOrder(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := decisionForTest()
		d.Events = append(d.Events, event(5, core.EvActivityScheduled, core.ActivityScheduled{
			K: 2, ActivityType: "BookFlight", Mode: core.ModeSemantic,
		}))
		d.NewTasks = append(d.NewTasks, core.Task{
			ShardID: 1, Type: core.TaskActivity, WorkflowID: "wf-1", RunID: "run-1", ScheduledEventSeq: 5,
		})
		d.LastSeq = 5

		out := semanticOutcome{
			reuse: map[int]json.RawMessage{
				1: json.RawMessage(`"one"`),
				2: json.RawMessage(`"two"`),
			},
			block: map[int]bool{},
		}
		if err := applyOutcome(&d, out, testNow.UnixNano()); err != nil {
			t.Fatalf("applyOutcome: %v", err)
		}
		if d.LastSeq != 7 {
			t.Fatalf("last_seq = %d, want 7", d.LastSeq)
		}
		var first core.ActivityCompleted
		if err := json.Unmarshal(d.Events[len(d.Events)-2].Payload, &first); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if first.K != 1 {
			t.Fatalf("k=1 must be numbered before k=2, got k=%d at seq 6", first.K)
		}
	}
}
