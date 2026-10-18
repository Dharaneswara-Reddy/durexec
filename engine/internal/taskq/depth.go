package taskq

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// DepthInterval is how often the queue depth is re-counted.
//
// Five seconds, because that is the scrape interval in deploy/prometheus.yml.
// Counting faster produces numbers nothing ever reads; counting slower leaves
// every scrape returning a value older than the scrape before it, which turns a
// gauge that is meant to say "right now" into a staircase.
//
// The cost is one aggregate query per node per interval — not per poll, and not
// per task. Putting a COUNT on the poll path was the obvious alternative and it
// is the wrong one: polling is the hottest statement in the engine, it runs once
// per task per worker per shard, and adding a second query to it would spend
// more database time measuring the queue than draining it. Depth is also a
// quantity nobody needs to the millisecond — it matters that it is climbing,
// not what it was 300ms ago.
const DepthInterval = 5 * time.Second

// depthTaskTypes is every task type a shard can hold.
//
// It is listed explicitly so a queue that has just drained reports zero. A
// GROUP BY returns no row for a type with no tasks, and a gauge keeps its last
// value until something overwrites it, so publishing only what the query found
// would leave the last non-zero depth on the dashboard for as long as the node
// ran — exactly the reading that matters most, frozen at exactly the wrong
// moment.
var depthTaskTypes = [...]core.TaskType{core.TaskWorkflow, core.TaskActivity}

// DepthObserver receives the sampled depths. It is declared here, as the
// narrowest interface the sampler needs, rather than importing the metrics
// package, so this package stays testable with no Prometheus registry.
// internal/obs.Metrics satisfies it.
type DepthObserver interface {
	SetQueueDepth(shard core.ShardID, taskType core.TaskType, depth float64)
	ClearQueueDepth(shard core.ShardID)
}

// DepthConfig configures the sampling loop.
type DepthConfig struct {
	// Interval is how often to count. Zero means DepthInterval.
	Interval time.Duration
	// Shards returns the shards this node currently owns. It is a function, not
	// a list, because ownership changes underneath the sampler, and reporting a
	// depth for a shard another node now owns would make the dashboard — which
	// sums across nodes — count that shard's queue twice.
	Shards func() []core.ShardID
	// Observer receives the depths. Nil disables sampling entirely, which is
	// what a node running with no registry wants.
	Observer DepthObserver
	// OnError observes query failures. A failed pass never stops the loop; the
	// next one tries again.
	OnError func(err error)
}

func (c DepthConfig) withDefaults() DepthConfig {
	if c.Interval <= 0 {
		c.Interval = DepthInterval
	}
	return c
}

// DepthSampler reports how many tasks are waiting in each owned shard's queue.
//
// It exists because queue depth is the one signal that distinguishes a healthy
// engine from an overwhelmed one: throughput looks the same whether tasks are
// being finished as fast as they arrive or a backlog is quietly growing behind
// them.
type DepthSampler struct {
	q   *Queue
	cfg DepthConfig

	// mu guards reported, which is read and written by Sample. Sample is
	// exported so a test can drive one pass directly while Run is going, so the
	// map needs a lock even though in production only Run touches it.
	mu sync.Mutex
	// reported is the shards this node has published a depth for, so a shard
	// that leaves the owned set can have its series removed rather than left on
	// the dashboard at whatever its depth was when ownership moved.
	reported map[core.ShardID]struct{}
}

// NewDepthSampler builds a sampler over the same queue the workers poll. It
// does not start anything; call Run.
func NewDepthSampler(q *Queue, cfg DepthConfig) *DepthSampler {
	return &DepthSampler{q: q, cfg: cfg.withDefaults(), reported: map[core.ShardID]struct{}{}}
}

// Run samples on the configured interval until ctx is cancelled.
//
// The first pass waits for the first tick rather than running immediately: at
// start-up this node owns no shards yet, so an eager pass would publish nothing
// and only make the loop harder to reason about.
func (s *DepthSampler) Run(ctx context.Context) error {
	if s.cfg.Observer == nil || s.cfg.Shards == nil {
		return nil
	}
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.Sample(ctx); err != nil && s.cfg.OnError != nil {
				s.cfg.OnError(err)
			}
		}
	}
}

// Sample runs one pass: count the owned shards' queues and publish the result.
// It is exported so a test can drive a pass at a time rather than waiting on a
// real ticker.
func (s *DepthSampler) Sample(ctx context.Context) error {
	if s.cfg.Observer == nil || s.cfg.Shards == nil {
		return nil
	}
	shards := s.cfg.Shards()

	// Shards we have lost go first, and unconditionally. They are stale whether
	// or not the count below succeeds, and leaving them published is the one
	// failure that silently corrupts another node's reading of the same shard.
	s.forget(shards)
	if len(shards) == 0 {
		return nil
	}

	counts, err := s.count(ctx, shards)
	if err != nil {
		// Deliberately nothing is published. A transient database error does not
		// mean the queues are empty, and writing zeros would turn a brief outage
		// into a dashboard that says the backlog cleared. The previous values
		// stand for one interval, which is the smaller lie.
		return err
	}

	for _, shard := range shards {
		for _, tt := range depthTaskTypes {
			k := depthKey{shard, tt}
			s.cfg.Observer.SetQueueDepth(shard, tt, float64(counts[k]))
			delete(counts, k)
		}
	}
	// Anything left is a task type the schema has grown since depthTaskTypes
	// was written. Report it rather than drop it: an unknown type with a
	// backlog is precisely the thing worth seeing.
	for k, n := range counts {
		s.cfg.Observer.SetQueueDepth(k.shard, k.taskType, float64(n))
	}

	s.remember(shards)
	return nil
}

// depthKey is one (shard, task_type) pair, which is the grain of the gauge.
type depthKey struct {
	shard    core.ShardID
	taskType core.TaskType
}

// countSQL counts the tasks that are pollable right now.
//
// The predicate is deliberately the same one pollSQL selects on. "Waiting"
// means a worker could take this row this instant: a task whose visibility is
// still in the future is backing off, not queued, and a task under a live lock
// is in flight, not waiting. Counting either would make an engine that is
// keeping up look like one falling behind.
//
// One statement covers every owned shard, so the cost of a pass is one round
// trip rather than one per shard. The tasks_pollable index covers the
// shard_id/task_type/visible_at leg of the predicate.
const countSQL = `
SELECT shard_id, task_type, count(*)
  FROM tasks
 WHERE shard_id = ANY($1)
   AND visible_at <= now()
   AND (locked_until IS NULL OR locked_until < now())
 GROUP BY shard_id, task_type`

func (s *DepthSampler) count(ctx context.Context, shards []core.ShardID) (map[depthKey]int64, error) {
	ids := make([]int32, len(shards))
	for i, sh := range shards {
		ids[i] = int32(sh)
	}

	rows, err := s.q.db.Query(ctx, countSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("taskq: count queue depth: %w", err)
	}
	defer rows.Close()

	out := make(map[depthKey]int64, len(shards)*len(depthTaskTypes))
	for rows.Next() {
		var (
			shard    int32
			taskType string
			n        int64
		)
		if err := rows.Scan(&shard, &taskType, &n); err != nil {
			return nil, fmt.Errorf("taskq: scan queue depth: %w", err)
		}
		out[depthKey{core.ShardID(shard), core.TaskType(taskType)}] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("taskq: count queue depth: %w", err)
	}
	return out, nil
}

// forget removes the series for every shard we have published but no longer
// own.
func (s *DepthSampler) forget(owned []core.ShardID) {
	keep := make(map[core.ShardID]struct{}, len(owned))
	for _, sh := range owned {
		keep[sh] = struct{}{}
	}

	s.mu.Lock()
	gone := make([]core.ShardID, 0, len(s.reported))
	for sh := range s.reported {
		if _, ok := keep[sh]; !ok {
			gone = append(gone, sh)
			delete(s.reported, sh)
		}
	}
	s.mu.Unlock()

	// Outside the lock: the observer is a caller's code, and holding a lock
	// across it buys nothing and invites a deadlock.
	for _, sh := range gone {
		s.cfg.Observer.ClearQueueDepth(sh)
	}
}

func (s *DepthSampler) remember(owned []core.ShardID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sh := range owned {
		s.reported[sh] = struct{}{}
	}
}
