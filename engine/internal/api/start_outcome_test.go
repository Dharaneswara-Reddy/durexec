package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// TestStartOutcomeClassification pins the mapping from a start error to a
// metric label. The cases that matter are the wrapped ones: an error that
// reaches the handler has usually passed through a layer that added context,
// and a classifier using a type assertion instead of errors.As would silently
// fall through to "refused" and report every failover as a commit failure.
func TestStartOutcomeClassification(t *testing.T) {
	notOwner := &core.NotOwnerError{ShardID: 7, CurrentOwner: "durexec-1"}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil is not counted", nil, ""},
		{"not owner", notOwner, OutcomeNotOwned},
		{"not owner, wrapped", fmt.Errorf("start workflow: %w", notOwner), OutcomeNotOwned},
		{"not owner, wrapped twice", fmt.Errorf("a: %w", fmt.Errorf("b: %w", notOwner)), OutcomeNotOwned},
		{"lost ownership", core.ErrLostOwnership, OutcomeNotOwned},
		{"lost ownership, wrapped", fmt.Errorf("commit: %w", core.ErrLostOwnership), OutcomeNotOwned},
		{"database down", errors.New("dial tcp: connection refused"), OutcomeRefused},
		{"context deadline", fmt.Errorf("begin: %w", context.DeadlineExceeded), OutcomeRefused},
		{"orphaned run is not an ownership problem", core.ErrWorkflowOrphaned, OutcomeRefused},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := startOutcome(c.err); got != c.want {
				t.Fatalf("startOutcome(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestStartOutcomeVocabularyIsBounded is the cardinality guard. It asserts the
// property the comment on startOutcome claims: whatever error arrives, the
// label returned is one of a fixed, known set. An implementation that ever
// returned err.Error() would fail here rather than being discovered later as
// a time-series database full of one series per workflow id.
func TestStartOutcomeVocabularyIsBounded(t *testing.T) {
	allowed := map[string]bool{
		"":                 true, // not counted
		OutcomeNotOwned:    true,
		OutcomeRefused:     true,
		OutcomeAccepted:    true,
		OutcomeAlreadyOpen: true,
		OutcomeInvalid:     true,
		OutcomeUnavailable: true,
	}

	// Errors whose messages embed identifiers. If any of these leaked into the
	// label, the series count would grow with traffic.
	errs := []error{
		nil,
		&core.NotOwnerError{ShardID: 3, CurrentOwner: "durexec-0"},
		&core.NonDeterminismError{Seq: 9, K: 2, Recorded: "A", Requested: "B"},
		&core.VersionIncompatibleError{WorkflowID: "wf-abc-123", Recorded: "v1", Running: "v2"},
		&core.CompensationFailedError{WorkflowID: "wf-xyz-999", Activity: "Refund", Attempts: 5, Cause: errors.New("boom")},
		&core.RetryableError{Cause: errors.New("transient")},
		&core.NonRetryableError{Cause: errors.New("bad input")},
		core.ErrLostOwnership,
		core.ErrWorkflowNotFound,
		errors.New("some unique string 8f3a2b"),
	}

	for _, e := range errs {
		got := startOutcome(e)
		if !allowed[got] {
			t.Fatalf("startOutcome(%v) returned %q, which is not in the bounded vocabulary", e, got)
		}
	}
}
