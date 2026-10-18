package core

import (
	"encoding/json"
	"time"
)

// EventType names a fact that has happened. Events are appended, never updated.
type EventType string

const (
	EvWorkflowStarted   EventType = "WorkflowExecutionStarted"
	EvWorkflowCompleted EventType = "WorkflowExecutionCompleted"
	EvWorkflowFailed    EventType = "WorkflowExecutionFailed"

	EvActivityScheduled EventType = "ActivityTaskScheduled"
	EvActivityCompleted EventType = "ActivityTaskCompleted"
	EvActivityFailed    EventType = "ActivityTaskFailed"

	EvTimerStarted EventType = "TimerStarted"
	EvTimerFired   EventType = "TimerFired"

	EvSignalReceived EventType = "SignalReceived"
	EvSideEffect     EventType = "SideEffectRecorded"

	EvDedupEvaluated EventType = "SemanticDedupEvaluated"

	EvCompensationScheduled EventType = "CompensationScheduled"
	EvCompensationCompleted EventType = "CompensationCompleted"
	EvCompensationFailed    EventType = "CompensationFailed"
)

// Event is one immutable row of history.
type Event struct {
	ShardID    ShardID         `json:"shard_id"`
	WorkflowID string          `json:"workflow_id"`
	RunID      RunID           `json:"run_id"`
	Seq        Seq             `json:"seq"`
	Type       EventType       `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ---- payloads -------------------------------------------------------------

// ActivityScheduled records the intention to run an activity. K is the
// program-order index that joins live execution to replay.
type ActivityScheduled struct {
	K            int             `json:"k"`
	ActivityType string          `json:"activity_type"`
	Input        json.RawMessage `json:"input"`
	DedupKey     string          `json:"dedup_key"`
	Mode         IdempotencyMode `json:"mode"`
}

type ActivityCompleted struct {
	K      int             `json:"k"`
	Result json.RawMessage `json:"result"`
}

type ActivityFailed struct {
	K            int    `json:"k"`
	Error        string `json:"error"`
	Attempt      int    `json:"attempt"`
	NonRetryable bool   `json:"non_retryable"`
}

type TimerStarted struct {
	K        int           `json:"k"`
	Duration time.Duration `json:"duration_ns"`
	FireAt   time.Time     `json:"fire_at"`
}

type TimerFired struct {
	K int `json:"k"`
}

type SignalReceived struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

type SideEffectRecorded struct {
	K     int             `json:"k"`
	Value json.RawMessage `json:"value"`
}

// DedupEvaluated is the journaled verdict. Replay reads this and performs no
// embedding call, so recovery never depends on the model being reachable.
type DedupEvaluated struct {
	K          int          `json:"k"`
	Verdict    DedupVerdict `json:"verdict"`
	MatchedK   int          `json:"matched_k"`
	Similarity float64      `json:"similarity"`
	TauHigh    float64      `json:"tau_high"`
	TauLow     float64      `json:"tau_low"`
	Model      string       `json:"model"`
	IntentText string       `json:"intent_text"`

	// Detail is prose that explains a verdict the similarity score alone
	// cannot explain, written by whichever comparator produced the verdict.
	//
	// It exists because a score answers "how close were they?" and some
	// comparators decide on something else entirely. The hybrid comparator
	// (see internal/idem/semantic) rejects a candidate when one DECIDING
	// FIELD differs — a date, a route, a passenger count — and a reader of
	// the log holding only "similarity 0.9712, verdict DISTINCT" would
	// reasonably conclude the engine had malfunctioned. The field names what
	// actually differed, so a skipped or repeated action stays explainable
	// from the log alone, which is the property this whole mechanism is
	// written around.
	//
	// It is additive and optional. Events journaled before it existed decode
	// with Detail empty, and the pure-semantic path still leaves it empty, so
	// the payload those verdicts produce is byte-for-byte what it always was.
	Detail string `json:"detail,omitempty"`
}

type WorkflowCompleted struct {
	Result json.RawMessage `json:"result"`
}

type WorkflowFailed struct {
	Error string `json:"error"`
}
