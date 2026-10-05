package core

import "context"

// The ports below are the only way engine code reaches PostgreSQL. They exist
// so the workflow dispatcher can be tested with no database at all, which is
// how deterministic replay is verified in isolation.

// EventStore is the append-only history.
type EventStore interface {
	// Load returns one run's full history in seq order.
	Load(ctx context.Context, shard ShardID, workflowID string, run RunID) ([]Event, error)
	// LastSeq returns the highest seq written for a run, or 0 if none.
	LastSeq(ctx context.Context, shard ShardID, workflowID string, run RunID) (Seq, error)
}

// Committer applies an entire workflow-task Decision atomically. This single
// method is the correctness argument of the whole engine.
type Committer interface {
	CommitDecision(ctx context.Context, d Decision) error
}

// TaskQueue is a lease queue, not a delete-on-read queue: a dead worker's lock
// simply expires and the task becomes visible again.
type TaskQueue interface {
	// Poll takes a time-boxed lock on one task, or returns ErrNoTask.
	Poll(ctx context.Context, shard ShardID, t TaskType) (Task, error)
	// Extend renews the lock on a task that is still being worked.
	Extend(ctx context.Context, shard ShardID, taskID int64, lockToken string) error
	// Release makes a task visible again immediately, with backoff applied.
	Release(ctx context.Context, shard ShardID, taskID int64, lockToken string, visibleIn int64) error
	// Enqueue adds tasks outside a Decision, used by the timer scanner.
	Enqueue(ctx context.Context, tasks []Task) error
}

// LeaseStore holds shard ownership. Four columns that replace ZooKeeper.
type LeaseStore interface {
	// Acquire takes an unowned or expired lease and bumps the fencing token.
	Acquire(ctx context.Context, shard ShardID, owner string, ttlSeconds int) (FencingToken, error)
	// Renew extends a lease we already hold. Zero rows means we were fenced out.
	Renew(ctx context.Context, shard ShardID, owner string, tok FencingToken, ttlSeconds int) error
	// Release voluntarily gives up a shard on graceful shutdown.
	Release(ctx context.Context, shard ShardID, owner string, tok FencingToken) error
	// Get reads current ownership, used by the gateway to answer NotOwner.
	Get(ctx context.Context, shard ShardID) (Lease, error)
	// List returns every shard's ownership, for the /v1/shards admin view.
	List(ctx context.Context) ([]Lease, error)
}

// TimerStore backs durable sleep.
type TimerStore interface {
	Due(ctx context.Context, shard ShardID, limit int) ([]Timer, error)
	Delete(ctx context.Context, t Timer) error
}

// IntentStore holds the meaning-vectors for the semantic check. Candidate sets
// are bounded by workflow history length, not corpus size.
type IntentStore interface {
	Put(ctx context.Context, in Intent) error
	// Candidates returns prior successful invocations of the same activity type
	// within the same run — tens of rows, so brute-force cosine is enough.
	Candidates(ctx context.Context, shard ShardID, workflowID string, run RunID, activityType string) ([]Intent, error)
}

// Embedder turns canonicalised intent text into a vector. It is called on the
// workflow path in new mode only, and never during replay.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Model() string
	Dim() int
}

// ExecutionStore is the mutable status pointer derived from the log.
type ExecutionStore interface {
	Create(ctx context.Context, shard ShardID, workflowID string, run RunID, wfType string) error
	Get(ctx context.Context, shard ShardID, workflowID string) (Execution, error)
	List(ctx context.Context, shard ShardID, status ExecutionStatus, limit int) ([]Execution, error)
}

// Execution is one row of the executions table.
type Execution struct {
	ShardID      ShardID
	WorkflowID   string
	RunID        RunID
	WorkflowType string
	Status       ExecutionStatus
	LastSeq      Seq
}
