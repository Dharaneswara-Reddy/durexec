package taskq

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Idle backoff bounds.
//
// An idle worker must not hammer the database, but it also must not sleep so
// long that a freshly enqueued task sits waiting. Starting at 10ms keeps the
// latency of the common case — work arrives while we are between polls —
// imperceptible, and doubling up to a 1s ceiling means a genuinely empty shard
// costs one query per second instead of a hundred.
const (
	IdleMin = 10 * time.Millisecond
	IdleMax = 1 * time.Second
)

// NextIdle returns how long to wait before the next poll.
//
// It is a pure function of the current wait and whether the last poll found
// work, with no clock, no sleeping and no state of its own, so the whole
// back-off policy can be tested by calling it in a loop. The reset on success is
// the important half: a worker coming out of a quiet period must return to full
// speed on the very first task, not climb back down gradually.
func NextIdle(current time.Duration, foundWork bool) time.Duration {
	if foundWork {
		return IdleMin
	}
	if current < IdleMin {
		return IdleMin
	}
	next := current * 2
	if next > IdleMax {
		return IdleMax
	}
	return next
}

// Handler processes one polled task.
//
// Returning nil means the task is finished and its row is deleted. Returning an
// error means it should be retried later, and the Poller applies the jittered
// backoff for the task's attempt count before making it visible again. A handler
// that has already deleted the row itself — a workflow task completed inside a
// Committer transaction — should return ErrTaskConsumed.
type Handler func(ctx context.Context, t core.Task) error

// ErrTaskConsumed tells the Poller that the row is already gone, because the
// handler completed it inside its own transaction. Deleting it a second time
// would be harmless but the error it returns would be misleading noise.
var ErrTaskConsumed = errors.New("taskq: task already consumed by handler")

// PollerConfig configures one polling loop.
type PollerConfig struct {
	Shard core.ShardID
	Type  core.TaskType
	// Rand is the source for backoff jitter. Nil means a private source seeded
	// from the clock; tests pass a fixed seed. It is not shared between pollers
	// because *rand.Rand is not safe for concurrent use.
	Rand *rand.Rand
	// OnError observes handler and queue failures. Nil discards them; the loop
	// itself never stops because of a task-level failure.
	OnError func(err error)
}

// Poller repeatedly polls one shard and one task type and hands each task to a
// Handler. One Poller drives one queue; a node running both workflow and
// activity workers runs two.
type Poller struct {
	q   *Queue
	cfg PollerConfig
	h   Handler
}

// NewPoller builds a polling loop. It does not start anything; call Run.
func NewPoller(q *Queue, cfg PollerConfig, h Handler) *Poller {
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Poller{q: q, cfg: cfg, h: h}
}

// Run polls until ctx is cancelled. It returns nil on cancellation: a stopped
// worker is a normal shutdown, not a failure.
func (p *Poller) Run(ctx context.Context) error {
	idle := IdleMin
	for {
		if ctx.Err() != nil {
			return nil
		}

		task, err := p.q.Poll(ctx, p.cfg.Shard, p.cfg.Type)
		switch {
		case errors.Is(err, core.ErrNoTask):
			idle = NextIdle(idle, false)
		case err != nil:
			// A database error is not an empty queue, but backing off is still the
			// right response: retrying a broken connection at full speed helps
			// nobody. The error is reported so the operator sees it.
			p.report(err)
			idle = NextIdle(idle, false)
		default:
			idle = NextIdle(idle, true)
			p.handle(ctx, task)
		}

		if !sleepCtx(ctx, idle) {
			return nil
		}
	}
}

// handle runs one task and settles its row. A panic in workflow code must not
// take the whole worker down, so it is recovered and turned into a retry: the
// lock will lapse or the release will push the task out, and the next attempt
// gets a longer backoff.
func (p *Poller) handle(ctx context.Context, t core.Task) {
	err := p.runHandler(ctx, t)
	switch {
	case errors.Is(err, ErrTaskConsumed):
		return
	case err == nil:
		if cerr := p.q.Complete(ctx, t.ShardID, t.TaskID, t.LockToken); cerr != nil {
			p.report(cerr)
		}
	default:
		p.report(err)
		// Attempt was already incremented by the poll, so it is 1-based here,
		// which is exactly what Backoff.Ceiling expects.
		delay := p.q.cfg.Backoff.Delay(t.Attempt, p.cfg.Rand)
		if rerr := p.q.ReleaseAfter(ctx, t.ShardID, t.TaskID, t.LockToken, delay); rerr != nil {
			p.report(rerr)
		}
	}
}

func (p *Poller) runHandler(ctx context.Context, t core.Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r}
		}
	}()
	return p.h(ctx, t)
}

// PanicError wraps a panic raised inside a task handler so the loop can treat it
// as an ordinary retryable failure instead of dying.
type PanicError struct{ Value any }

func (e *PanicError) Error() string { return fmt.Sprintf("taskq: handler panicked: %v", e.Value) }

func (p *Poller) report(err error) {
	if p.cfg.OnError != nil && err != nil {
		p.cfg.OnError(err)
	}
}

// sleepCtx waits for d or until ctx is done. It reports false when the context
// ended, which is the loop's signal to stop.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
