// Package bench is the performance harness for the two speed requirements in
// the design document.
//
//	NFR-6  Replay latency                     under 50 ms at 200 events
//	NFR-7  Added latency of the semantic check p99 under 40 ms
//
// It builds everything it needs in memory. There is no database, no network and
// no model server, which is what lets these numbers be reproduced on any
// machine by running `go test -bench . ./internal/bench/`.
//
// # What the numbers here do and do not include
//
// Being honest about the boundary of the measurement matters more than the
// measurement, so it is stated up front.
//
// NFR-6 is specified as the time to LOAD a run's history and then replay it.
// This harness measures only the replay half: the history is already a slice in
// memory when the timer starts. The database read is not included. So the
// figures below are a lower bound on NFR-6, and they answer a narrower
// question: how does the cost of re-executing workflow code grow as history
// gets longer? That question is worth answering on its own, because it is the
// part of the cost the engine's own design controls. The load half is a single
// indexed range scan and has to be measured against a real PostgreSQL instance.
//
// NFR-7 is the extra time one semantic check adds to scheduling an activity.
// The check is: render the intent text, embed it, fetch the candidate intents,
// and compare the vectors. This harness measures all of that EXCEPT the real
// cost of embedding, because it runs against semantic.FakeEmbedder, which
// hashes text instead of running a model. A real sentence encoder takes
// milliseconds; sha256 takes microseconds. So the measurement here is the
// engine's own overhead around the model, and the honest way to read it is:
//
//	real added latency  ≈  (what this harness measures)  +  (one model inference)
//
// The second term dominates and can only be measured against a real encoder.
// What this harness can and does establish is that the first term is small
// enough not to matter — that the engine is not itself the bottleneck.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// ---------------------------------------------------------------------------
// The workflow under test.
// ---------------------------------------------------------------------------

const (
	// ChainWorkflow runs a straight line of activities, one after another.
	//
	// A straight chain is the right shape for this measurement because replay
	// cost is driven by how many recorded decisions have to be walked, not by
	// the shape of the control flow. A chain gives the longest history per line
	// of workflow code and keeps the benchmark measuring the engine rather than
	// a contrived workflow.
	ChainWorkflow = "BenchChain"

	// ChainActivity is the single activity type the chain schedules.
	ChainActivity = "BenchStep"
)

// ChainInput tells the chain workflow how many activities to run.
type ChainInput struct {
	Steps int `json:"steps"`
}

// chain is the workflow function. It has no side effects of its own: every
// blocking call is answered from history during replay.
func chain(ctx workflow.Context, input json.RawMessage) (json.RawMessage, error) {
	var in ChainInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	last := json.RawMessage(`null`)
	for i := 0; i < in.Steps; i++ {
		v, err := ctx.ExecuteActivity(ChainActivity, map[string]int{"step": i}).Get()
		if err != nil {
			return nil, err
		}
		last = v
	}
	return last, nil
}

// NewDispatcher builds a dispatcher with the chain workflow registered.
func NewDispatcher() *workflow.Dispatcher {
	r := workflow.NewRegistry()
	r.MustRegister(ChainWorkflow, chain)
	return workflow.NewDispatcher(r)
}

// ---------------------------------------------------------------------------
// In-memory histories.
// ---------------------------------------------------------------------------

const (
	benchShard = core.ShardID(3)
	benchWFID  = "bench-workflow"
	benchRunID = core.RunID("22222222-2222-2222-2222-222222222222")
)

// benchBase is a fixed instant. Nothing here reads a real clock, so a run today
// and a run next year produce byte-identical histories.
var benchBase = time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)

// Scenario is one prepared replay: a history of a known length and the input
// that drives the workflow through it.
type Scenario struct {
	// Events is the history, in seq order, exactly as core.EventStore.Load
	// would hand it back.
	Events []core.Event

	// Input is the workflow's original argument.
	Input json.RawMessage

	// Activities is how many activities the history records as completed. It is
	// the number of recorded decisions replay has to walk.
	Activities int

	// Blocking is true when the history ends with an activity that was
	// scheduled but has not finished yet, so replaying it suspends the workflow
	// instead of completing it.
	//
	// That is the ordinary steady state: a worker picks up a workflow task,
	// replays everything that already happened, reaches the call it is still
	// waiting on, and stops. Benchmarking that shape measures the cost the
	// engine actually pays on its hot path.
	Blocking bool
}

// BuildScenario makes a history containing exactly events events.
//
// The layout is one WorkflowExecutionStarted, then a scheduled/completed pair
// per activity, then — when the count is even — one final scheduled event with
// no completion, which is what makes the replay suspend.
//
// An even count therefore produces a workflow that is mid-flight, and an odd
// count produces one that runs to completion. Even counts are the interesting
// ones and are what the benchmarks use.
func BuildScenario(events int) (Scenario, error) {
	if events < 2 {
		return Scenario{}, fmt.Errorf("bench: a history needs at least 2 events, got %d", events)
	}

	var (
		completed int
		blocking  bool
	)
	if events%2 == 0 {
		completed, blocking = (events-2)/2, true
	} else {
		completed, blocking = (events-1)/2, false
	}

	b := newBuilder()
	b.add(core.EvWorkflowStarted, map[string]string{"workflow_type": ChainWorkflow})
	for k := 0; k < completed; k++ {
		b.scheduled(k)
		b.completed(k)
	}
	steps := completed
	if blocking {
		b.scheduled(completed) // scheduled, never completed
		steps = completed + 1
	}

	if len(b.events) != events {
		return Scenario{}, fmt.Errorf("bench: built %d events, wanted %d", len(b.events), events)
	}

	in, err := json.Marshal(ChainInput{Steps: steps})
	if err != nil {
		return Scenario{}, err
	}
	return Scenario{
		Events:     b.events,
		Input:      in,
		Activities: completed,
		Blocking:   blocking,
	}, nil
}

// MustBuildScenario is BuildScenario for package-level fixtures, where a
// failure is a programming error rather than something to handle.
func MustBuildScenario(events int) Scenario {
	s, err := BuildScenario(events)
	if err != nil {
		panic(err)
	}
	return s
}

// HistoryLengths are the sizes the design document's NFR-6 is measured at. 200
// is the one with a stated budget; the others are there to show the shape of
// the growth curve, which is what tells you whether the cost is linear.
var HistoryLengths = []int{10, 50, 100, 200, 500}

type builder struct {
	events []core.Event
	seq    core.Seq
}

func newBuilder() *builder { return &builder{} }

func (b *builder) add(typ core.EventType, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("bench: marshalling %s payload: %v", typ, err))
	}
	b.events = append(b.events, core.Event{
		ShardID:    benchShard,
		WorkflowID: benchWFID,
		RunID:      benchRunID,
		Seq:        b.seq + 1,
		Type:       typ,
		Payload:    raw,
		// One second apart, so the workflow's derived clock advances the way it
		// would in a real run.
		CreatedAt: benchBase.Add(time.Duration(b.seq) * time.Second),
	})
	b.seq++
}

func (b *builder) scheduled(k int) {
	b.add(core.EvActivityScheduled, core.ActivityScheduled{
		K:            k,
		ActivityType: ChainActivity,
		Input:        json.RawMessage(fmt.Sprintf(`{"step":%d}`, k)),
		DedupKey:     fmt.Sprintf("bench-key-%d", k),
		Mode:         core.ModeExact,
	})
}

func (b *builder) completed(k int) {
	b.add(core.EvActivityCompleted, core.ActivityCompleted{
		K:      k,
		Result: json.RawMessage(fmt.Sprintf(`{"step":%d,"ok":true}`, k)),
	})
}

// ---------------------------------------------------------------------------
// In-memory intent store, for the semantic benchmarks.
// ---------------------------------------------------------------------------

// MemIntents is a core.IntentStore held entirely in a slice.
//
// A slice rather than a map is deliberate: Candidates is meant to return rows
// in scheduled_seq order, and the benchmark should pay the same linear scan the
// real code pays rather than being flattered by a lookup the database does not
// have.
type MemIntents struct {
	intents []core.Intent
}

var _ core.IntentStore = (*MemIntents)(nil)

func NewMemIntents() *MemIntents { return &MemIntents{} }

func (m *MemIntents) Put(_ context.Context, in core.Intent) error {
	m.intents = append(m.intents, in)
	return nil
}

func (m *MemIntents) Candidates(_ context.Context, shard core.ShardID, wf string, run core.RunID, activityType string) ([]core.Intent, error) {
	out := make([]core.Intent, 0, len(m.intents))
	for _, in := range m.intents {
		if in.ShardID == shard && in.WorkflowID == wf && in.RunID == run && in.ActivityType == activityType {
			out = append(out, in)
		}
	}
	return out, nil
}

// Len reports how many intents are stored, so a benchmark can state the size of
// the candidate set it measured against.
func (m *MemIntents) Len() int { return len(m.intents) }
