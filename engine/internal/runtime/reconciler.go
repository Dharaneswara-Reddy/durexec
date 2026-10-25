package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
)

// Reconciler repairs RUNNING workflows that nothing in the engine will ever
// drive again.
//
// WHY IT IS NOT A WORKAROUND. The defect that produced most of these — a start
// written in two transactions — is fixed at its source: an execution row is now
// opened inside the same fenced transaction as its start event, so the engine
// can no longer create an abandoned start. This component is NOT that fix and
// must not be mistaken for it. It exists because two other things are also
// true:
//
//  1. The rows already written cannot be un-written. Nineteen abandoned starts
//     are sitting on the demonstration cluster, two days stale, reported as
//     RUNNING, and no code path in the engine looks at them.
//  2. "RUNNING with no task and no timer" is reachable by means the engine does
//     not control at all. One live workflow reached it with a COMPLETE,
//     successful history and only its final completion event missing; the
//     audit could prove it was stuck and deliberately refused to name a cause,
//     because a restore, an operator or a test writing into the live database
//     would all produce it and one of those is known to have happened. Whatever
//     writes that state, the engine had NO recovery path for it, and it was
//     reported as healthy.
//
// So the invariant this component defends is the operator-facing one: any stuck
// state has a route back, and nothing is silently abandoned. It is the
// component that would have recovered all twenty stuck workflows on the live
// cluster.
//
// WHAT IT DOES, AND WHAT IT REFUSES TO DO. Two states, opposite treatments:
//
//   - NO EVENTS AT ALL. The start was abandoned, so nothing happened in the
//     world and there is nothing to compensate. The row is deleted, which frees
//     the business key so the caller's retry produces a real run. Every
//     condition is re-tested inside the DELETE, so a row that has come alive
//     since it was listed is left alone.
//   - EVENTS PRESENT. A real run lost its task row. A WORKFLOW task is
//     enqueued and replay finishes it. This is safe by construction rather than
//     by luck: replay is a pure function of history, so re-driving a workflow
//     that is already complete simply writes the completion event it was
//     missing, and re-driving one that is mid-flight recomputes the decision it
//     would have made anyway. The events primary key catches it if a real
//     worker turns up at the same moment.
//
// It NEVER fails a run, and it never writes an event. Marking something FAILED
// from out here would set a status from outside the workflow, which skips every
// compensation the workflow owes — the same mistake the divergence path used to
// make. If a run cannot be repaired by replay, a human decides.
type Reconciler struct {
	Engine *Engine

	// Scanner reads the stuck rows. It is the narrowest interface this
	// component needs rather than core.ExecutionStore, so adding the queries
	// did not widen a port that every fake in the codebase implements.
	Scanner StuckScanner

	// Shards reports which shards this node currently owns. The reconciler
	// only ever touches a shard this node holds the lease for, so two nodes
	// cannot repair the same row, and every write it causes still goes through
	// the fencing check.
	Shards func() []core.ShardID

	// Every is how often to scan. Zero takes DefaultReconcileEvery.
	Every time.Duration

	// Stale is how long a row must have been untouched to be considered stuck.
	// Zero takes DefaultReconcileStale.
	//
	// It must be comfortably longer than the task lock, or a workflow whose
	// task is merely claimed by a busy worker could be mistaken for abandoned.
	// A claimed task still has a tasks row, so the scan excludes it anyway;
	// the threshold is the second line of defence and the reason the default
	// is minutes rather than seconds.
	Stale time.Duration

	// Limit caps how many rows one scan repairs per shard, so a shard with
	// thousands of stuck rows cannot turn one pass into a long transaction
	// storm. The rest are picked up on the next pass, oldest first.
	Limit int
}

// StuckScanner is the database half of the reconciler. eventlog.Store
// satisfies it.
type StuckScanner interface {
	StuckRunning(ctx context.Context, shard core.ShardID, olderThan time.Duration, limit int) ([]eventlog.StuckExecution, error)
	DeleteAbandonedStart(ctx context.Context, shard core.ShardID, workflowID string, olderThan time.Duration) (bool, error)
}

const (
	// DefaultReconcileEvery is deliberately slow. Nothing it repairs is
	// urgent — these runs have already been stuck for minutes at least — and a
	// frequent scan over executions buys nothing.
	DefaultReconcileEvery = 60 * time.Second

	// DefaultReconcileStale is well past any legitimate gap between a commit
	// and the next poll, which is milliseconds, and past the default task lock
	// of thirty seconds with room to spare.
	DefaultReconcileStale = 5 * time.Minute

	// DefaultReconcileLimit bounds one pass per shard.
	DefaultReconcileLimit = 100
)

func (r *Reconciler) every() time.Duration {
	if r.Every > 0 {
		return r.Every
	}
	return DefaultReconcileEvery
}

func (r *Reconciler) stale() time.Duration {
	if r.Stale > 0 {
		return r.Stale
	}
	return DefaultReconcileStale
}

func (r *Reconciler) limit() int {
	if r.Limit > 0 {
		return r.Limit
	}
	return DefaultReconcileLimit
}

// Run scans until ctx is cancelled. One Reconciler per node, not per shard:
// the scan is cheap and sharing it keeps the number of background goroutines
// independent of how many shards this node happens to own.
func (r *Reconciler) Run(ctx context.Context) {
	// One pass at startup, before the first tick. A node that has just taken
	// over a shard from a crashed one is exactly when stuck rows are most
	// likely, and waiting a full interval to look would be perverse.
	r.Once(ctx)

	t := time.NewTicker(r.every())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Once(ctx)
		}
	}
}

// ReconcileResult is what one pass did. It is returned rather than only logged
// so a test can assert on it and so an acceptance check can report it.
type ReconcileResult struct {
	// Scanned is how many stuck rows were found.
	Scanned int
	// Deleted is abandoned starts removed: rows with no history at all.
	Deleted int
	// Redriven is real runs handed a fresh workflow task.
	Redriven int
	// Skipped is rows that changed between the scan and the repair, so another
	// writer got there first. Not an error; the point of re-testing.
	Skipped int
}

// Once runs a single pass over every shard this node owns.
func (r *Reconciler) Once(ctx context.Context) ReconcileResult {
	var total ReconcileResult
	if r.Scanner == nil || r.Shards == nil {
		return total
	}
	for _, shard := range r.Shards() {
		if ctx.Err() != nil {
			return total
		}
		res := r.onceShard(ctx, shard)
		total.Scanned += res.Scanned
		total.Deleted += res.Deleted
		total.Redriven += res.Redriven
		total.Skipped += res.Skipped
	}
	return total
}

func (r *Reconciler) onceShard(ctx context.Context, shard core.ShardID) ReconcileResult {
	var res ReconcileResult
	e := r.Engine
	log := e.logger().WithShard(shard)

	// Ownership is checked before scanning, so a shard this node has handed
	// over is not even looked at. It is checked again implicitly on every write
	// the repair causes, because those go through the queue and the committer.
	if _, ok := e.TokenFor(shard); !ok {
		return res
	}

	stuck, err := r.Scanner.StuckRunning(ctx, shard, r.stale(), r.limit())
	if err != nil {
		if ctx.Err() == nil {
			log.Error(ctx, "scan for stuck executions", "error", err)
		}
		return res
	}
	res.Scanned = len(stuck)

	for _, s := range stuck {
		if ctx.Err() != nil {
			return res
		}
		wlog := log.WithWorkflow(s.WorkflowID, s.RunID)

		if s.Events == 0 {
			// An abandoned start: no history, so nothing happened.
			removed, derr := r.Scanner.DeleteAbandonedStart(ctx, shard, s.WorkflowID, r.stale())
			switch {
			case derr != nil:
				wlog.Error(ctx, "remove abandoned start", "error", derr)
			case removed:
				res.Deleted++
				wlog.Warn(ctx, "removed an abandoned start: RUNNING with no history, no task and no timer",
					"stale_for", time.Since(s.UpdatedAt).String(),
					"effect", "the business key is free again, so a retry will produce a real run")
			default:
				// The row changed under us, which is the DELETE's conditions
				// doing their job.
				res.Skipped++
			}
			continue
		}

		// A real run that lost its task row. Replay is idempotent, so handing
		// it a task is safe whatever put it in this state.
		task := core.Task{
			ShardID:    shard,
			Type:       core.TaskWorkflow,
			WorkflowID: s.WorkflowID,
			RunID:      s.RunID,
			// No scheduling event: this task is not the consequence of one. The
			// column is nullable precisely for tasks the timer scanner and this
			// component create.
			VisibleAt: e.now().Now(),
		}
		if err := e.Tasks.Enqueue(ctx, []core.Task{task}); err != nil {
			if !errors.Is(err, ctx.Err()) {
				wlog.Error(ctx, "re-drive a stuck workflow", "error", err)
			}
			continue
		}
		res.Redriven++
		wlog.Warn(ctx, "re-drove a stuck workflow: RUNNING with history but no task and no timer",
			"last_seq", int64(s.LastSeq), "events", s.Events,
			"stale_for", time.Since(s.UpdatedAt).String())
	}

	if res.Deleted > 0 || res.Redriven > 0 {
		log.Info(ctx, "reconciled stuck executions",
			"scanned", res.Scanned, "deleted", res.Deleted,
			"redriven", res.Redriven, "skipped", res.Skipped)
	}
	return res
}
