package core

import (
	"errors"
	"fmt"
)

// Sentinel errors. Callers should use errors.Is / errors.As, never string matching.
var (
	// ErrLostOwnership means the fencing token moved while we were working.
	// The worker must abandon its transaction and stop touching the shard.
	ErrLostOwnership = errors.New("lost shard ownership")
	// ErrWorkflowAlreadyOpen is success, not failure: a duplicate start request
	// collided with a running execution, so we return the existing one.
	ErrWorkflowAlreadyOpen = errors.New("workflow already open")
	// ErrWorkflowNotFound is returned by query paths.
	ErrWorkflowNotFound = errors.New("workflow not found")
	// ErrWorkflowOrphaned means the executions row for a business key exists but
	// its run never became durable: RUNNING with last_seq 0, so no start event
	// and no first task were ever written.
	//
	// Since the executions row is now opened inside the same transaction as the
	// start event, the fixed engine cannot create this state. It survives only
	// as rows written by the two-transaction start path that preceded the fix.
	// It is a separate sentinel from ErrWorkflowAlreadyOpen because the two are
	// opposites to a caller: "already open" means someone else's identical
	// request succeeded and may be awaited, whereas this means nobody's did and
	// the request must not be reported as success. Answering a retry with
	// "already open, RUNNING" for one of these is exactly how the defect stayed
	// invisible for two days.
	ErrWorkflowOrphaned = errors.New("workflow execution row exists but its run was never started")
	// ErrNoTask means the queue poll found nothing; back off and retry.
	ErrNoTask = errors.New("no task available")
	// ErrEmbeddingUnavailable makes the semantic check fall back to exact
	// matching for one call, and the fallback is journaled.
	ErrEmbeddingUnavailable = errors.New("embedding service unavailable")
)

// NotOwnerError is returned when a request reaches a node that does not own the
// shard. The gateway is allowed to be briefly wrong: it refreshes and retries.
type NotOwnerError struct {
	ShardID      ShardID
	CurrentOwner string
}

func (e *NotOwnerError) Error() string {
	return fmt.Sprintf("shard %d is owned by %q", e.ShardID, e.CurrentOwner)
}

// NonDeterminismError is raised when replay diverges from recorded history.
// The engine cannot repair this — it has no way to know which version of the
// code told the truth — so it refuses to continue and names the divergence.
type NonDeterminismError struct {
	Seq       Seq
	K         int
	Recorded  string
	Requested string
}

func (e *NonDeterminismError) Error() string {
	return fmt.Sprintf("non-determinism at k=%d (seq %d): history recorded %q but code requested %q",
		e.K, e.Seq, e.Recorded, e.Requested)
}

// VersionIncompatibleError says that this binary declines to replay a run that
// was started by code it does not claim to be compatible with.
//
// It is a REFUSAL TO DECIDE, not a failure of the run, and that distinction is
// the whole point of the type. The engine has no way to know whether the newer
// code would reach the same decisions as the code that wrote this history, so
// it does not guess and it does not journal anything: the run keeps its history
// exactly as found, its task goes back on the queue, and either a node running
// compatible code picks it up or an operator redeploys the old version. A
// terminal failure here would strand whatever the forward steps already did,
// because a status set from outside the workflow never runs its compensations.
type VersionIncompatibleError struct {
	WorkflowID string
	// Recorded is the code version in the run's start event; Running is what
	// this binary declares. Either may be empty, which means "unversioned".
	Recorded string
	Running  string
}

func (e *VersionIncompatibleError) Error() string {
	recorded, running := e.Recorded, e.Running
	if recorded == "" {
		recorded = "(unversioned)"
	}
	if running == "" {
		running = "(unversioned)"
	}
	return fmt.Sprintf("workflow %s was started by code version %s, which this binary (%s) does not accept",
		e.WorkflowID, recorded, running)
}

// CompensationFailedError is terminal and requires human intervention. We
// surface it rather than pretending the rollback succeeded.
type CompensationFailedError struct {
	WorkflowID string
	Activity   string
	Attempts   int
	Cause      error
}

func (e *CompensationFailedError) Error() string {
	return fmt.Sprintf("compensation %q for workflow %q failed after %d attempts: %v",
		e.Activity, e.WorkflowID, e.Attempts, e.Cause)
}

func (e *CompensationFailedError) Unwrap() error { return e.Cause }

// RetryableError marks a failure the retry policy should keep trying.
type RetryableError struct{ Cause error }

func (e *RetryableError) Error() string { return e.Cause.Error() }
func (e *RetryableError) Unwrap() error { return e.Cause }

// NonRetryableError marks a failure that must not be retried, such as a
// validation error, so the workflow can compensate instead of looping.
type NonRetryableError struct{ Cause error }

func (e *NonRetryableError) Error() string { return e.Cause.Error() }
func (e *NonRetryableError) Unwrap() error { return e.Cause }

// IsNonRetryable reports whether the retry policy should give up immediately.
func IsNonRetryable(err error) bool {
	var nr *NonRetryableError
	return errors.As(err, &nr)
}
