package timers

import (
	"context"
	"fmt"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ScannerConfig configures the loop that wakes sleeping workflows.
type ScannerConfig struct {
	// Interval is how often to look for due timers. Zero means 1s.
	//
	// One second is the resolution of every durable sleep in the engine: a timer
	// fires somewhere between its due moment and one interval later. That is the
	// honest trade. Scanning faster costs a query per shard per pass for work that
	// is almost always empty; scanning slower adds latency to every wake-up.
	Interval time.Duration
	// Batch is how many timers to take from one shard in one pass. Zero means the
	// store's default.
	Batch int
	// Clock is injected so a test can decide what time it is. Zero means the
	// system clock.
	Clock core.Clock
	// Shards returns the shards this node currently owns. It is a function, not a
	// list, because ownership changes underneath the scanner: it is called fresh
	// on every pass so a shard lost half a second ago is not scanned again.
	Shards func() []core.ShardID
	// OnError observes failures. A failure on one shard never stops the loop or
	// the other shards; the timers stay in the table and the next pass retries.
	OnError func(shard core.ShardID, err error)
}

func (c ScannerConfig) withDefaults() ScannerConfig {
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	if c.Batch <= 0 {
		c.Batch = defaultDueLimit
	}
	if c.Clock == nil {
		c.Clock = core.SystemClock{}
	}
	return c
}

// Scanner turns due timer rows into pollable workflow tasks.
//
// It takes core.TimerStore and core.TaskQueue as interfaces rather than the
// concrete packages, so it can be driven entirely by fakes in a test, and so
// this package never has to import the queue implementation.
type Scanner struct {
	store core.TimerStore
	queue core.TaskQueue
	cfg   ScannerConfig
}

// NewScanner builds a scanner. It does not start anything; call Run.
func NewScanner(store core.TimerStore, queue core.TaskQueue, cfg ScannerConfig) *Scanner {
	return &Scanner{store: store, queue: queue, cfg: cfg.withDefaults()}
}

// Run scans on the configured interval until ctx is cancelled.
func (s *Scanner) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick runs one pass over every owned shard. It is exported so a test can drive
// the scanner a pass at a time rather than waiting on a real ticker.
func (s *Scanner) Tick(ctx context.Context) {
	if s.cfg.Shards == nil {
		return
	}
	for _, shard := range s.cfg.Shards() {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.ScanShard(ctx, shard); err != nil && s.cfg.OnError != nil {
			s.cfg.OnError(shard, err)
		}
	}
}

// ScanShard fires every due timer in one shard and reports how many fired.
//
// The order is deliberate: enqueue the task first, delete the timer second.
//
// If the process dies between the two, the timer row is still there and the next
// pass enqueues a second task for the same wake-up. That is a duplicate, and a
// duplicate is harmless — the workflow's history is the authority on whether the
// timer already fired, so the second task replays, finds nothing to do and goes
// away. Doing it the other way round would risk the opposite: the row deleted,
// the task never enqueued, and a workflow asleep forever with nothing left to
// wake it. Between a duplicate and a permanent hang, we choose the duplicate.
func (s *Scanner) ScanShard(ctx context.Context, shard core.ShardID) (int, error) {
	due, err := s.store.Due(ctx, shard, s.cfg.Batch)
	if err != nil {
		return 0, fmt.Errorf("timers: scan shard %d: %w", shard, err)
	}
	if len(due) == 0 {
		return 0, nil
	}

	if err := s.queue.Enqueue(ctx, TasksForTimers(due, s.cfg.Clock.Now())); err != nil {
		return 0, fmt.Errorf("timers: enqueue wake-ups for shard %d: %w", shard, err)
	}

	fired := 0
	for _, t := range due {
		if err := s.store.Delete(ctx, t); err != nil {
			// The task is already enqueued, so stopping here would only delay the
			// remaining deletions to the next pass — where they are retried anyway.
			// Report and keep going.
			return fired, fmt.Errorf("timers: delete fired timer %d: %w", t.TimerSeq, err)
		}
		fired++
	}
	return fired, nil
}

// TasksForTimers converts due timers into the workflow tasks that wake them.
//
// It is pure and exported so the mapping — which is the part with a real chance
// of being subtly wrong — can be table-tested with no database and no clock.
// ScheduledEventSeq carries the timer's own seq through, which is how the
// workflow, on replay, matches the task back to the TimerStarted event that
// created it.
func TasksForTimers(due []core.Timer, now time.Time) []core.Task {
	out := make([]core.Task, 0, len(due))
	for _, t := range due {
		out = append(out, core.Task{
			ShardID:           t.ShardID,
			Type:              core.TaskWorkflow,
			WorkflowID:        t.WorkflowID,
			RunID:             t.RunID,
			ScheduledEventSeq: t.TimerSeq,
			VisibleAt:         now,
		})
	}
	return out
}
