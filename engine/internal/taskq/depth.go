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

// DepthObserver receives the samples. It is declared here, as the narrowest
// interface the sampler needs, rather than importing the metrics package, so
// this package stays testable with no Prometheus registry.
// internal/obs.Metrics satisfies it.
type DepthObserver interface {
	// SetQueueAge publishes how long the oldest claimable task in this
	// (shard, task_type) has been waiting, in seconds. Zero means nothing is
	// claimable — an empty queue and a fully in-flight queue both read as
	// zero, which is correct: in neither case is work going unstarted.
	SetQueueAge(shard core.ShardID, taskType core.TaskType, seconds float64)
	ClearQueueAge(shard core.ShardID)
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

	ages, err := s.age(ctx, shards)
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
			s.cfg.Observer.SetQueueAge(shard, tt, ages[k])
			delete(ages, k)
		}
	}
	// Anything left is a task type the schema has grown since depthTaskTypes
	// was written. Report it rather than drop it: an unknown type with a
	// backlog is precisely the thing worth seeing.
	for k, seconds := range ages {
		s.cfg.Observer.SetQueueAge(k.shard, k.taskType, seconds)
	}

	s.remember(shards)
	return nil
}

// depthKey is one (shard, task_type) pair, which is the grain of the gauge.
type depthKey struct {
	shard    core.ShardID
	taskType core.TaskType
}

// ageSQL reports how long the oldest claimable task in each owned
// (shard, task_type) has been waiting, in seconds. NULL means nothing is
// claimable, which the caller publishes as zero.
//
// It replaced a count, and the reason is measured rather than stylistic.
//
// The count was `count(*) ... WHERE visible_at <= now() AND (locked_until IS
// NULL OR locked_until < now()) GROUP BY shard_id, task_type`, and the comment
// above it claimed tasks_pollable covered it. It did not: the query has no
// task_type predicate at all, so the index's second column was skipped and
// PostgreSQL 16 has no skip scan. Measured on an isolated 16-shard database:
//
//	rows in tasks   this query (count)        ageSQL
//	    16,000       2.68 ms    2.0 MiB     0.41 ms   0.8 MiB
//	   160,000      17.43 ms   20.6 MiB     0.99 ms   1.0 MiB
//	 1,600,000     130.92 ms  205.1 MiB     0.97 ms   1.0 MiB
//
// Adding the missing `task_type = ANY(...)` — the fix the audit proposed — was
// measured and makes it WORSE, 202.74 ms and 231.8 MiB at 1,600,000 rows,
// because the planner then builds a bitmap over every matching row. So does
// counting through the new tasks_poll_ready index (135.45 ms, 219.8 MiB), and
// so does capping the count (652.16 ms, 2266.1 MiB). None of that is a
// planning mistake: counting N matching rows costs O(N) whatever the plan,
// because every row has to be visited, and roughly a fifth of a deep queue is
// claimable. **An exact queue depth cannot be made cheap by any index**, and
// at 128MB of shared_buffers a 205 MiB pass every five seconds on each of
// three nodes evicts the whole cache — precisely when the cluster is already
// in trouble and the number is most wanted.
//
// Age can be made cheap, because it needs one row rather than all of them: one
// descent of tasks_poll_ready per (shard, task_type) pair, which is why the
// measurement above is flat from 16,000 rows to 1,600,000. It is also the
// better signal. Depth cannot tell a deep queue that is draining from a queue
// that is stuck, and the second is the outage; age says so directly, and it is
// in the unit an SLO is written in. It is the same argument timers/store.go:59
// makes for draining timers oldest-first.
//
// One statement still covers every owned shard, so a pass is one round trip.
var ageSQL = `
SELECT p.shard_id, p.task_type,
       COALESCE(EXTRACT(EPOCH FROM (now() - (
           SELECT ` + readyAtOf("k") + `
             FROM tasks k
            WHERE k.shard_id = p.shard_id AND k.task_type = p.task_type
              AND ` + readyAtOf("k") + ` <= now()
            ORDER BY k.shard_id, k.task_type, ` + readyAtOf("k") + `
            LIMIT 1))), 0)
  FROM (SELECT s, t FROM unnest($1::int[]) s, unnest($2::text[]) t)
       AS p(shard_id, task_type)`

// readyAtOf qualifies the readyAt expression with a table alias. The index is
// built on the unqualified form, and PostgreSQL matches an index expression
// after resolving column references, so the two still match — but ageSQL joins
// tasks to a derived table, where unqualified column names would be ambiguous.
func readyAtOf(alias string) string {
	return `GREATEST(` + alias + `.visible_at, COALESCE(` + alias + `.locked_until, '-infinity'::timestamptz))`
}

// age returns the waiting time of the oldest claimable task per
// (shard, task_type), in seconds, with zero for "nothing claimable".
func (s *DepthSampler) age(ctx context.Context, shards []core.ShardID) (map[depthKey]float64, error) {
	ids := make([]int32, len(shards))
	for i, sh := range shards {
		ids[i] = int32(sh)
	}
	types := make([]string, 0, len(depthTaskTypes))
	for _, tt := range depthTaskTypes {
		types = append(types, string(tt))
	}

	rows, err := s.q.db.Query(ctx, ageSQL, ids, types)
	if err != nil {
		return nil, fmt.Errorf("taskq: sample queue age: %w", err)
	}
	defer rows.Close()

	out := make(map[depthKey]float64, len(shards)*len(depthTaskTypes))
	for rows.Next() {
		var (
			shard    int32
			taskType string
			seconds  float64
		)
		if err := rows.Scan(&shard, &taskType, &seconds); err != nil {
			return nil, fmt.Errorf("taskq: scan queue age: %w", err)
		}
		out[depthKey{core.ShardID(shard), core.TaskType(taskType)}] = seconds
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("taskq: sample queue age: %w", err)
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
		s.cfg.Observer.ClearQueueAge(sh)
	}
}

func (s *DepthSampler) remember(owned []core.ShardID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sh := range owned {
		s.reported[sh] = struct{}{}
	}
}
