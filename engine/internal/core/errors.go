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
