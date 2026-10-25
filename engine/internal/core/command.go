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

// NewExecution asks the commit to open the executions row as part of this same
// transaction. It carries only the workflow type: the shard, workflow id and
// run id come from the enclosing Decision, so the row can never be opened for a
// different run than the one whose first event is being appended beside it.
//
// It exists because opening the row in a transaction of its own was a genuine
// data-loss defect. A start used to be two round trips — INSERT INTO executions
// on its own connection, then the Decision carrying the first event and the
// first task — and anything that went wrong in between (a crash, a lost lease,
// a failed commit) left the row behind as RUNNING with last_seq 0, no events,
// no task and no timer. Nothing in the engine ever looked at such a row again,
// and because the executions primary key deliberately excludes run_id, the
// client's retry was answered "already open, RUNNING" forever. Nineteen of them
// accumulated on the demonstration cluster before anyone queried for them.
type NewExecution struct {
	WorkflowType string
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

	// CreateExecution, when non-nil, opens the executions row inside this
	// transaction, after the fencing check and before the events. Only
	// StartWorkflow sets it; every later decision for the run leaves it nil and
	// merely updates the row's status pointer.
	CreateExecution *NewExecution

	Events    []Event
	NewTasks  []Task
	NewTimers []Timer
	Intents   []Intent

	Status  ExecutionStatus
	LastSeq Seq

	// CompleteTaskID is the workflow task row to delete, proving the work is done.
	CompleteTaskID int64
}
