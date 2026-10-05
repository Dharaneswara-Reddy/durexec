package workflow

import (
	"errors"
	"fmt"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ActivityError is what a workflow sees when an activity it called has failed
// for good — the activity worker exhausted the retry policy, or the failure was
// marked non-retryable and so was never retried at all.
//
// It is reconstructed from the recorded ActivityTaskFailed event rather than
// carried across from the worker, because by the time the workflow observes the
// failure the process that produced it may be long gone. That is also why the
// cause is a plain message: an error's Go type does not survive a trip through
// JSON, so what survives is the text plus the one bit that changes behaviour,
// "was this retryable".
type ActivityError struct {
	ActivityType string
	K            int
	Message      string
	NonRetryable bool

	cause error
}

func newActivityError(activityType string, k int, message string, nonRetryable bool) *ActivityError {
	e := &ActivityError{
		ActivityType: activityType,
		K:            k,
		Message:      message,
		NonRetryable: nonRetryable,
	}
	// Wrapping in core.NonRetryableError preserves the one property workflow
	// code actually branches on, so core.IsNonRetryable keeps working on a
	// failure that arrived through the log.
	base := errors.New(message)
	if nonRetryable {
		e.cause = &core.NonRetryableError{Cause: base}
	} else {
		e.cause = base
	}
	return e
}

func (e *ActivityError) Error() string {
	name := e.ActivityType
	if name == "" {
		name = "activity"
	}
	return fmt.Sprintf("%s (k=%d) failed: %s", name, e.K, e.Message)
}

func (e *ActivityError) Unwrap() error { return e.cause }
