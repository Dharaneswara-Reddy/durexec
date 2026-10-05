package runtime

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/obs"
	"github.com/dharaneswar339/durexec/internal/taskq"
)

// logger returns the engine's logger, or a discarding one, so no call site has
// to test for nil.
func (e *Engine) logger() *obs.Logger {
	if e.Log != nil {
		return e.Log
	}
	return obs.NewLoggerFrom(nil)
}

// backoff returns the retry spacing for a released task.
func (e *Engine) backoff() taskq.Backoff {
	b := e.Backoff
	if b.Base <= 0 {
		b = taskq.DefaultBackoff()
	}
	return b
}

// randFraction draws the jitter fraction. Full jitter matters here for the
// same reason it does in the queue: every worker in the fleet fails the same
// task at the same moment when a downstream service is down, and retrying in
// lockstep would rebuild the herd on every cycle.
func randFraction() float64 { return rand.Float64() }

// WorkerPool starts and stops per-shard workers as leases come and go.
//
// Shard ownership is the unit of everything here. A node runs workers only for
// the shards it holds, and the moment a lease is lost those workers must stop
// — not because their writes would be accepted (the fencing token in the
// commit refuses them) but because a node that keeps polling a shard it does
// not own burns queries and delays the node that does.
type WorkerPool struct {
	Engine *Engine

	// WorkflowWorkers and ActivityWorkers are goroutines PER SHARD. Splitting
	// them is what stops a slow activity from starving workflow decisions:
	// they are separate queues polled by separate workers, so a shard whose
	// activities are all waiting on a slow API still makes workflow progress.
	WorkflowWorkers int
	ActivityWorkers int

	mu      sync.Mutex
	running map[core.ShardID]*shardWorkers
	stopped bool

	wg sync.WaitGroup
}

type shardWorkers struct {
	cancel context.CancelFunc
}

// NewWorkerPool builds a pool. Counts below one are raised to one, because a
// shard with no workers is a shard whose work silently never happens.
func NewWorkerPool(e *Engine, workflowWorkers, activityWorkers int) *WorkerPool {
	if workflowWorkers < 1 {
		workflowWorkers = 1
	}
	if activityWorkers < 1 {
		activityWorkers = 1
	}
	return &WorkerPool{
		Engine:          e,
		WorkflowWorkers: workflowWorkers,
		ActivityWorkers: activityWorkers,
		running:         make(map[core.ShardID]*shardWorkers),
	}
}

// Start begins work on a shard. Calling it for a shard already running is a
// no-op, which is what lets the supervisor loop call it on every tick without
// having to remember anything.
func (p *WorkerPool) Start(ctx context.Context, shard core.ShardID) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	if _, ok := p.running[shard]; ok {
		p.mu.Unlock()
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	p.running[shard] = &shardWorkers{cancel: cancel}
	p.mu.Unlock()

	p.Engine.logger().Info(ctx, "starting shard workers",
		"shard", int(shard), "workflow_workers", p.WorkflowWorkers, "activity_workers", p.ActivityWorkers)

	for i := 0; i < p.WorkflowWorkers; i++ {
		w := &WorkflowWorker{Engine: p.Engine, Shard: shard}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			w.Run(sctx)
		}()
	}
	for i := 0; i < p.ActivityWorkers; i++ {
		w := &ActivityWorker{Engine: p.Engine, Shard: shard}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			w.Run(sctx)
		}()
	}
}

// Stop ends work on one shard.
//
// It returns as soon as the workers have been told to stop, without waiting
// for them, because it is called from the lease manager's OnLost callback and
// that callback must not block: the manager is mid-pass over every shard, and
// holding it up would delay renewals for the shards this node still owns.
// Correctness does not depend on the wait — a worker that is mid-decision when
// its shard is taken has its commit refused by the fencing check.
func (p *WorkerPool) Stop(shard core.ShardID) {
	p.mu.Lock()
	sw, ok := p.running[shard]
	delete(p.running, shard)
	p.mu.Unlock()
	if !ok {
		return
	}
	p.Engine.logger().Info(context.Background(), "stopping shard workers", "shard", int(shard))
	sw.cancel()
}

// Owned reports which shards currently have workers, for the health endpoint
// and for tests.
func (p *WorkerPool) Owned() []core.ShardID {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.ShardID, 0, len(p.running))
	for s := range p.running {
		out = append(out, s)
	}
	return out
}

// StopAll ends every shard's workers and waits for them, which is the "drain
// in-flight tasks" half of a graceful shutdown: a worker that is mid-decision
// finishes its commit, and one that is merely idle returns at once.
func (p *WorkerPool) StopAll() {
	p.mu.Lock()
	p.stopped = true
	for _, sw := range p.running {
		sw.cancel()
	}
	p.running = make(map[core.ShardID]*shardWorkers)
	p.mu.Unlock()

	p.wg.Wait()
}

// Supervise keeps the running set equal to the owned set.
//
// It is a reconciliation loop rather than an event feed on purpose. The lease
// manager already tells us the moment a shard is LOST, which is the urgent
// direction — work must stop immediately. Acquisition is not urgent, and
// polling for it means the pool's state is derived from ownership rather than
// accumulated from a stream of events that could be missed.
func (p *WorkerPool) Supervise(ctx context.Context, owned func() map[core.ShardID]core.FencingToken, every time.Duration) {
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for shard := range owned() {
			p.Start(ctx, shard)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
