package runtime

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
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
	// ctx is the context every worker for this shard runs under, kept so a
	// replacement worker can be started under the same cancellation as the
	// ones it joins. Stop cancels it, which stops originals and replacements
	// alike.
	ctx    context.Context
	cancel context.CancelFunc

	// liveWorkflow and liveActivity count the worker goroutines still running.
	// Each goroutine decrements its own on the way out, which is what lets
	// Start tell "already running" from "running, but short of some".
	liveWorkflow atomic.Int64
	liveActivity atomic.Int64
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

// Start begins work on a shard, and tops it back up to strength if some of its
// workers have since exited.
//
// Calling it for a shard that is already at strength is a no-op, which is what
// lets the supervisor loop call it on every tick without having to remember
// anything.
//
// # Why it replaces workers at all
//
// A worker returns from Run and is gone. Before this, nothing ever replaced
// one: the shard stayed in p.running, so every later Start saw "already
// running" and did nothing, and a shard could end up in the running set with
// no workers in it — reporting healthy, holding the lease, making no progress.
// That is the shape the sequence-race defect took, and the specific bug is
// fixed in workflow_worker.go, but "a worker exited and was never replaced"
// is a hole worth closing once rather than once per cause.
//
// # Why this cannot fight the lease manager
//
// Three things, and all three are needed:
//
//  1. The only caller is Supervise, which iterates the lease manager's own
//     owned set. A shard this node has legitimately lost is not in that set,
//     so it is never topped up — the pool can only ever replace workers for a
//     shard the manager says we still hold.
//  2. Replacements run under the shard's EXISTING context, not a fresh one, so
//     a Stop that lands a moment later cancels them exactly as it cancels the
//     originals.
//  3. If that context is already cancelled, nothing is started. Stop deletes
//     the map entry before cancelling, so the ordinary path never gets here;
//     this is the belt for a parent context that ended underneath us.
//
// The shortfall is computed and claimed under p.mu, so two concurrent Starts
// cannot both decide to replace the same missing worker.
func (p *WorkerPool) Start(ctx context.Context, shard core.ShardID) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	sw, existing := p.running[shard]
	if !existing {
		sctx, cancel := context.WithCancel(ctx)
		sw = &shardWorkers{ctx: sctx, cancel: cancel}
		p.running[shard] = sw
	}
	if sw.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	// Claim the shortfall while still holding the lock. Each goroutine gives
	// its count back as it exits, so these two numbers stay honest.
	wantWorkflow := p.WorkflowWorkers - int(sw.liveWorkflow.Load())
	wantActivity := p.ActivityWorkers - int(sw.liveActivity.Load())
	if wantWorkflow < 0 {
		wantWorkflow = 0
	}
	if wantActivity < 0 {
		wantActivity = 0
	}
	sw.liveWorkflow.Add(int64(wantWorkflow))
	sw.liveActivity.Add(int64(wantActivity))
	sctx := sw.ctx
	p.mu.Unlock()

	if wantWorkflow == 0 && wantActivity == 0 {
		return
	}
	if existing {
		// Worth an Info, not a Debug: in steady state this never happens, so a
		// node logging it is a node whose workers are dying for some reason.
		p.Engine.logger().Info(ctx, "replacing exited shard workers",
			"shard", int(shard), "workflow_workers", wantWorkflow, "activity_workers", wantActivity)
	} else {
		p.Engine.logger().Info(ctx, "starting shard workers",
			"shard", int(shard), "workflow_workers", wantWorkflow, "activity_workers", wantActivity)
	}

	for i := 0; i < wantWorkflow; i++ {
		w := &WorkflowWorker{Engine: p.Engine, Shard: shard}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer sw.liveWorkflow.Add(-1)
			w.Run(sctx)
		}()
	}
	for i := 0; i < wantActivity; i++ {
		w := &ActivityWorker{Engine: p.Engine, Shard: shard}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer sw.liveActivity.Add(-1)
			w.Run(sctx)
		}()
	}
}

// LiveWorkers reports how many workers of each kind are still running for a
// shard. It exists for the supervision test and for a health check that wants
// to know the difference between "owns the shard" and "is working on it".
func (p *WorkerPool) LiveWorkers(shard core.ShardID) (workflow, activity int) {
	p.mu.Lock()
	sw, ok := p.running[shard]
	p.mu.Unlock()
	if !ok {
		return 0, 0
	}
	return int(sw.liveWorkflow.Load()), int(sw.liveActivity.Load())
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

// Supervise keeps the running set equal to the owned set, and keeps each owned
// shard at full worker strength.
//
// It is a reconciliation loop rather than an event feed on purpose. The lease
// manager already tells us the moment a shard is LOST, which is the urgent
// direction — work must stop immediately. Acquisition is not urgent, and
// polling for it means the pool's state is derived from ownership rather than
// accumulated from a stream of events that could be missed.
//
// Because it is a reconciliation loop, replacing a worker that exited costs
// nothing extra: Start already runs on every tick for every owned shard, and
// it now tops the shard up instead of returning early. A shard whose workers
// all died is back at strength within one tick. A shard this node no longer
// owns is not in owned(), so it is never restarted.
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
