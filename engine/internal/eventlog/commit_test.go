package eventlog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// fakeCommitter returns whatever error it is given, so the wrapper can be
// driven through every outcome with no database.
type fakeCommitter struct {
	err   error
	calls int
}

func (f *fakeCommitter) CommitDecision(context.Context, core.Decision) error {
	f.calls++
	return f.err
}

// countingObserver records the shards a rejection was attributed to.
type countingObserver struct{ shards []core.ShardID }

func (c *countingObserver) FencingRejection(shard core.ShardID) {
	c.shards = append(c.shards, shard)
}

// TestFencedErrorIsStillLostOwnership is the compatibility guard. The typed
// error exists only so the rejection can be counted; every caller that already
// treats lost ownership as "stop at once" must keep doing exactly that.
func TestFencedErrorIsStillLostOwnership(t *testing.T) {
	err := error(&FencedError{Shard: 3, Stored: 9, Held: 8})
	if !errors.Is(err, core.ErrLostOwnership) {
		t.Fatal("a fenced write no longer reports lost ownership")
	}
	// The text is what an operator reads in the logs, and it said this before.
	for _, want := range []string{"shard 3", "token is 9", "we hold 8", "lost shard ownership"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text %q is missing %q", err.Error(), want)
		}
	}
}

// TestFencingCounterCountsExactlyTheRejections is the point of the whole
// wrapper. Three different failures in this package report lost ownership and
// only one of them is the fencing check firing; counting the others would put
// ordinary faults into the one number that is evidence the safety mechanism
// works.
func TestFencingCounterCountsExactlyTheRejections(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []core.ShardID
	}{
		{
			name: "a stale token is counted, on the shard it was refused for",
			err:  &FencedError{Shard: 4, Stored: 12, Held: 11},
			want: []core.ShardID{4},
		},
		{
			name: "and is still found when a caller has wrapped it",
			err:  fmt.Errorf("worker: %w", &FencedError{Shard: 7, Stored: 2, Held: 1}),
			want: []core.ShardID{7},
		},
		{
			name: "a shard with no lease row is an operational fault, not a rejection",
			err:  fmt.Errorf("eventlog: shard 4 has no lease row: %w", core.ErrLostOwnership),
			want: nil,
		},
		{
			name: "nor is the duplicate-seq race, though it also means ownership is stale",
			err:  fmt.Errorf("eventlog: seq 9 already appended: %w", core.ErrLostOwnership),
			want: nil,
		},
		{
			name: "an ordinary database error is not a rejection",
			err:  errors.New("connection refused"),
			want: nil,
		},
		{
			name: "and a commit that succeeded certainly is not",
			err:  nil,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := &fakeCommitter{err: tc.err}
			seen := &countingObserver{}
			c := NewFencingCounter(inner, seen)

			err := c.CommitDecision(context.Background(), core.Decision{ShardID: 99, Token: 1})

			// Observing must never change what the caller sees.
			if !errors.Is(err, tc.err) {
				t.Fatalf("CommitDecision returned %v, want %v unchanged", err, tc.err)
			}
			if inner.calls != 1 {
				t.Fatalf("inner committer called %d times, want 1", inner.calls)
			}
			if len(seen.shards) != len(tc.want) {
				t.Fatalf("counted %v rejections, want %v", seen.shards, tc.want)
			}
			for i := range tc.want {
				// The shard comes from the error, not from the decision, which
				// is why the decision above names a shard nothing happened on.
				if seen.shards[i] != tc.want[i] {
					t.Fatalf("rejection attributed to shard %d, want %d", seen.shards[i], tc.want[i])
				}
			}
		})
	}
}

// A node with no registry must still be able to commit.
func TestFencingCounterWithNoObserver(t *testing.T) {
	c := NewFencingCounter(&fakeCommitter{err: &FencedError{Shard: 1}}, nil)
	if err := c.CommitDecision(context.Background(), core.Decision{}); !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("CommitDecision = %v, want lost ownership", err)
	}
}
