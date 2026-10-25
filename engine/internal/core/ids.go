// Package core holds the vocabulary every other package in the engine speaks:
// identifiers, events, commands, errors and the ports that separate the
// decision-making code from PostgreSQL. It depends on nothing but the standard
// library, and nothing in core may import another internal package.
package core

import "time"

type (
	// ShardID is the fixed partition a workflow belongs to for its whole life.
	ShardID int32
	// RunID identifies one attempt at a WorkflowID. A business key may be retried.
	RunID string
	// Seq is the position of an event in one run's history. Starts at 1.
	Seq int64
	// FencingToken increases on every ownership change and never goes backwards.
	FencingToken int64
)

// ExecutionStatus is the mutable state of a workflow, derived from its history.
type ExecutionStatus string

const (
	StatusRunning            ExecutionStatus = "RUNNING"
	StatusCompleted          ExecutionStatus = "COMPLETED"
	StatusFailed             ExecutionStatus = "FAILED"
	StatusCompensating       ExecutionStatus = "COMPENSATING"
	StatusCompensatingFailed ExecutionStatus = "COMPENSATING_FAILED"
)

// IsTerminal reports whether no further work will ever be scheduled.
//
// StatusCompensatingFailed is terminal, and deliberately stays so: real-world
// state is known to be inconsistent and nothing automatic should touch it. What
// changed is that it is no longer a DEAD END — SignalResumeCompensation is
// accepted on it, which records a human's decision to retry the compensations
// the run still owes and moves it to StatusCompensating. See Engine.Signal.
func (s ExecutionStatus) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCompensatingFailed
}

// Reserved signal names. They travel through exactly the same durable path as
// any application signal — appended to history before any workflow code runs —
// rather than being second mechanisms that would each have to be made
// crash-safe on their own.
const (
	// SignalCancel asks a running workflow to stop.
	SignalCancel = "__cancel"

	// SignalResumeCompensation is the only way back from
	// COMPENSATING_FAILED, and the only engine API that accepts a terminal run.
	//
	// It exists because a failed rollback used to be unreachable. The status is
	// terminal, Signal refused every terminal run, Cancel went through Signal,
	// and a fresh start was answered "already open" — so the compensations the
	// run still owed were owed and unreachable, and recovery meant hand-written
	// SQL against the one table the design declares append-only. The machinery
	// to finish the rollback already existed: the compensation stack is a pure
	// function of history and resumes at the step that failed. Only the door
	// was locked.
	SignalResumeCompensation = "__resume_compensation"
)

// TaskType distinguishes the two queues that share the tasks table.
type TaskType string

const (
	TaskWorkflow TaskType = "WORKFLOW"
	TaskActivity TaskType = "ACTIVITY"
)

// Task is one unit of pollable work.
type Task struct {
	ShardID           ShardID
	TaskID            int64
	Type              TaskType
	WorkflowID        string
	RunID             RunID
	ScheduledEventSeq Seq
	VisibleAt         time.Time
	LockedUntil       *time.Time
	LockToken         string
	Attempt           int
}

// Timer is a durable sleep: a row, not a goroutine.
type Timer struct {
	ShardID    ShardID
	WorkflowID string
	RunID      RunID
	TimerSeq   Seq
	FireAt     time.Time
}

// Lease records which node owns a shard and with which fencing token.
type Lease struct {
	ShardID   ShardID
	Owner     string
	Token     FencingToken
	ExpiresAt time.Time
}

// Clock is injected so tests can advance time without sleeping.
type Clock interface{ Now() time.Time }

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }
