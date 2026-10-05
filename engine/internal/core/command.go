package core

import (
	"encoding/json"
	"time"
)

// CommandType is an intention produced by workflow code. A command is never
// executed directly — it is only ever turned into events inside the commit.
type CommandType string

const (
	CmdScheduleActivity CommandType = "ScheduleActivity"
	CmdStartTimer       CommandType = "StartTimer"
	CmdWaitSignal       CommandType = "WaitSignal"
	CmdRecordSideEffect CommandType = "RecordSideEffect"
	CmdRecordDedup      CommandType = "RecordDedupVerdict"
	CmdCompleteWorkflow CommandType = "CompleteWorkflow"
	CmdFailWorkflow     CommandType = "FailWorkflow"
)

// Command is what one workflow task produces before it blocks.
type Command struct {
	Type CommandType
	K    int

	ActivityType string
	Input        json.RawMessage
	DedupKey     string
	Mode         IdempotencyMode

	Duration time.Duration
	Signal   string
	Value    json.RawMessage
	Result   json.RawMessage
	Err      string

	Dedup *DedupEvaluated
}

// Decision is the entire outcome of one workflow task. Committer applies it in
// a single database transaction: either all of it lands, or none of it does.
type Decision struct {
	ShardID    ShardID
	WorkflowID string
	RunID      RunID

	// Token is re-verified FOR UPDATE inside the same transaction as the writes.
	// This is what makes a fenced-out node unable to mutate shard state.
	Token FencingToken

	Events    []Event
	NewTasks  []Task
	NewTimers []Timer
	Intents   []Intent

	Status  ExecutionStatus
	LastSeq Seq

	// CompleteTaskID is the workflow task row to delete, proving the work is done.
	CompleteTaskID int64
}
