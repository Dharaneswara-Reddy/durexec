// Package eventlog is the PostgreSQL half of the engine. It implements the
// three ports in core that touch durable state: EventStore (read history),
// ExecutionStore (read and create the mutable status pointer) and Committer
// (write everything a workflow task decided, atomically).
//
// Every write path in here is expected to be racing another node that believes
// it owns the same shard. The defence is always the same: re-read the fencing
// token FOR UPDATE inside the transaction doing the writing, and let the
// events primary key catch anything the token check misses.
package eventlog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/faultinj"
)

// Store is the single Postgres-backed implementation of the durable ports.
// One Store serves one shard database; the shard_id column still qualifies
// every statement so a shared database also works.
type Store struct {
	pool *pgxpool.Pool

	// faults is the deliberate-failure seam used by the recovery tests. A nil
	// *faultinj.Injector is a valid disabled injector, so the zero Store never
	// injects and the hot path costs one atomic load. Nothing can arm an
	// injector that configuration did not switch on.
	faults *faultinj.Injector
}

// New wraps an already-configured pool. Pool lifecycle stays with the caller,
// because the process may share one pool across several components.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// UseFaultInjector attaches the process's injector so CommitDecision can be
// stopped at the seam where its writes are staged but nothing is durable.
//
// It is a setter rather than a constructor argument so that every existing
// New(pool) call site stays correct and un-injectable by default: a node that
// never calls this can never crash at an injected seam. Call it once, during
// wiring, before any worker starts.
func (s *Store) UseFaultInjector(i *faultinj.Injector) { s.faults = i }

// Compile-time proof that we satisfy the ports other packages depend on.
var (
	_ core.EventStore     = (*Store)(nil)
	_ core.ExecutionStore = (*Store)(nil)
	_ core.Committer      = (*Store)(nil)
)

// Pool exposes the underlying pool for the other stores (leases, queue,
// timers) that the integrator builds on top of the same connection.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ---- EventStore -----------------------------------------------------------

const selectEventsSQL = `
SELECT shard_id, workflow_id, run_id::text, seq, event_type, payload, created_at
  FROM events
 WHERE shard_id = $1 AND workflow_id = $2 AND run_id = $3
 ORDER BY seq`

// Load returns one run's full history in seq order. An unknown run is an empty
// history, not an error: replay of a run with nothing recorded is legitimate.
func (s *Store) Load(ctx context.Context, shard core.ShardID, workflowID string, run core.RunID) ([]core.Event, error) {
	rows, err := s.pool.Query(ctx, selectEventsSQL, int32(shard), workflowID, string(run))
	if err != nil {
		return nil, fmt.Errorf("eventlog: load history: %w", err)
	}
	defer rows.Close()

	var out []core.Event
	for rows.Next() {
		var (
			ev      core.Event
			runText string
			payload []byte
		)
		if err := rows.Scan(&ev.ShardID, &ev.WorkflowID, &runText, &ev.Seq, &ev.Type, &payload, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("eventlog: scan event: %w", err)
		}
		ev.RunID = core.RunID(runText)
		ev.Payload = append([]byte(nil), payload...)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: load history: %w", err)
	}
	return out, nil
}

const lastSeqSQL = `
SELECT COALESCE(MAX(seq), 0)
  FROM events
 WHERE shard_id = $1 AND workflow_id = $2 AND run_id = $3`

// LastSeq returns the highest seq written for a run, or 0 if none. COALESCE
// keeps the "no history yet" case on the same code path as every other read.
func (s *Store) LastSeq(ctx context.Context, shard core.ShardID, workflowID string, run core.RunID) (core.Seq, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, lastSeqSQL, int32(shard), workflowID, string(run)).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("eventlog: last seq: %w", err)
	}
	return core.Seq(seq), nil
}

// ---- ExecutionStore -------------------------------------------------------

// An execution is opened by CommitDecision, from createExecutionSQL in
// commit.go, and there is deliberately no Create method here. The executions
// primary key is (shard_id, workflow_id) with no run_id, so at most one run can
// be open per business key, and that is what makes StartWorkflow idempotent at
// the front door: a duplicate start loses the race on the primary key and is
// reported as core.ErrWorkflowAlreadyOpen. What changed is only where the race
// is run — inside the fenced transaction that also writes the start event, so
// the winner's row and its history become durable together and the loser
// writes nothing at all. See the note on core.ExecutionStore.

const selectExecutionSQL = `
SELECT shard_id, workflow_id, run_id::text, workflow_type, status, last_seq, created_at
  FROM executions
 WHERE shard_id = $1 AND workflow_id = $2`

// Get reads the mutable status pointer for one business key.
func (s *Store) Get(ctx context.Context, shard core.ShardID, workflowID string) (core.Execution, error) {
	var (
		e       core.Execution
		runText string
	)
	err := s.pool.QueryRow(ctx, selectExecutionSQL, int32(shard), workflowID).
		Scan(&e.ShardID, &e.WorkflowID, &runText, &e.WorkflowType, &e.Status, &e.LastSeq, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Execution{}, core.ErrWorkflowNotFound
	}
	if err != nil {
		return core.Execution{}, fmt.Errorf("eventlog: get execution: %w", err)
	}
	e.RunID = core.RunID(runText)
	return e, nil
}

// List returns executions in a shard, optionally filtered by status. An empty
// status means "any", which is what the admin view asks for when it wants the
// whole shard rather than just the running work.
func (s *Store) List(ctx context.Context, shard core.ShardID, status core.ExecutionStatus, limit int) ([]core.Execution, error) {
	sql, args := listExecutionsQuery(shard, status, limit)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("eventlog: list executions: %w", err)
	}
	defer rows.Close()

	var out []core.Execution
	for rows.Next() {
		var (
			e       core.Execution
			runText string
		)
		if err := rows.Scan(&e.ShardID, &e.WorkflowID, &runText, &e.WorkflowType, &e.Status, &e.LastSeq, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("eventlog: scan execution: %w", err)
		}
		e.RunID = core.RunID(runText)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: list executions: %w", err)
	}
	return out, nil
}

// ---- reconciliation -------------------------------------------------------

// StuckExecution is a RUNNING workflow that nothing will ever drive again: it
// has no pollable task and no pending timer, so no component of the engine has
// a reason to look at it. Events is what distinguishes the two kinds.
type StuckExecution struct {
	ShardID    core.ShardID
	WorkflowID string
	RunID      core.RunID
	LastSeq    core.Seq
	// Events is how many events the run has. ZERO means the start itself was
	// abandoned — there is no history, so there is nothing to resume and the
	// row is simply garbage. NON-ZERO means a real run lost its task row, and
	// replay can finish it, because replay is a pure function of history.
	Events int
	// UpdatedAt is when anything last touched the row, which is what the
	// staleness threshold is measured against.
	UpdatedAt time.Time
}

// stuckRunningSQL finds RUNNING executions that nothing is going to drive.
//
// This is the query no evidence file in the project ever ran, and it is the one
// that found 19 abandoned starts and one workflow stuck for three days on the
// live cluster. The conditions, in order:
//
//   - status RUNNING and not stale by less than the threshold, so a workflow
//     that is merely between a commit and its next poll is never touched;
//   - no tasks row, because a task is exactly "something will drive this";
//   - no timers row, because a timer is a scheduled reason to come back;
//   - NOT an unresolved escalation hold. A held activity has the same database
//     signature as an abandoned run — RUNNING, no task, no timer — because
//     applyOutcome deliberately writes the scheduling event WITHOUT a task
//     while it waits for a human. Re-driving one is harmless but pointless, and
//     counting it as stuck would make this query noisy exactly where an
//     operator needs it to be precise. A hold is identified as an ESCALATE
//     verdict with no later __adjudicate signal.
const stuckRunningSQL = `
SELECT e.shard_id, e.workflow_id, e.run_id::text, e.last_seq, e.updated_at,
       (SELECT count(*) FROM events v
         WHERE v.shard_id = e.shard_id AND v.workflow_id = e.workflow_id
           AND v.run_id = e.run_id)
  FROM executions e
 WHERE e.shard_id = $1
   AND e.status = 'RUNNING'
   AND e.updated_at < now() - make_interval(secs => $2)
   AND NOT EXISTS (SELECT 1 FROM tasks t
                    WHERE t.shard_id = e.shard_id AND t.workflow_id = e.workflow_id)
   AND NOT EXISTS (SELECT 1 FROM timers m
                    WHERE m.shard_id = e.shard_id AND m.workflow_id = e.workflow_id)
   AND NOT EXISTS (
         SELECT 1 FROM events esc
          WHERE esc.shard_id = e.shard_id AND esc.workflow_id = e.workflow_id
            AND esc.run_id = e.run_id
            AND esc.event_type = 'SemanticDedupEvaluated'
            AND esc.payload->>'verdict' = 'ESCALATE'
            AND NOT EXISTS (
                  SELECT 1 FROM events adj
                   WHERE adj.shard_id = esc.shard_id AND adj.workflow_id = esc.workflow_id
                     AND adj.run_id = esc.run_id
                     AND adj.event_type = 'SignalReceived'
                     AND adj.payload->>'name' = '__adjudicate'
                     AND adj.seq > esc.seq))
 ORDER BY e.updated_at
 LIMIT $3`

// StuckRunning returns RUNNING executions in a shard that have no task and no
// timer and have been untouched for at least olderThan.
//
// It is read-only. Deciding what to do about each row belongs to the caller,
// because the two kinds need opposite treatment and only one of them is safe to
// do automatically.
func (s *Store) StuckRunning(ctx context.Context, shard core.ShardID, olderThan time.Duration, limit int) ([]StuckExecution, error) {
	rows, err := s.pool.Query(ctx, stuckRunningSQL,
		int32(shard), olderThan.Seconds(), clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("eventlog: scan for stuck executions: %w", err)
	}
	defer rows.Close()

	var out []StuckExecution
	for rows.Next() {
		var (
			e       StuckExecution
			runText string
		)
		if err := rows.Scan(&e.ShardID, &e.WorkflowID, &runText, &e.LastSeq, &e.UpdatedAt, &e.Events); err != nil {
			return nil, fmt.Errorf("eventlog: scan stuck execution: %w", err)
		}
		e.RunID = core.RunID(runText)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: scan for stuck executions: %w", err)
	}
	return out, nil
}

// deleteAbandonedStartSQL removes an executions row whose run never started.
//
// Every condition is re-tested inside the DELETE rather than trusted from the
// earlier SELECT, which is what makes this safe to run against a live shard: if
// anything at all has happened to the row since it was listed — an event
// appended, a task enqueued, the status moved, the row touched — zero rows are
// affected and the reconciler simply does nothing. There is no window in which
// it can delete a workflow that is actually running.
//
// Deleting rather than failing the run is the right repair for this one state.
// The row has NO history, so nothing happened in the world and there is nothing
// to compensate; it is not a workflow, it is the residue of a start that was
// rolled back everywhere except here. Leaving it would keep the business key
// occupied forever, which is the mechanism by which the client's retry kept
// being told "already open, RUNNING". Removing it lets the caller start the
// work it asked for.
const deleteAbandonedStartSQL = `
DELETE FROM executions e
 WHERE e.shard_id = $1
   AND e.workflow_id = $2
   AND e.status = 'RUNNING'
   AND e.last_seq = 0
   AND e.updated_at < now() - make_interval(secs => $3)
   AND NOT EXISTS (SELECT 1 FROM events v
                    WHERE v.shard_id = e.shard_id AND v.workflow_id = e.workflow_id)
   AND NOT EXISTS (SELECT 1 FROM tasks t
                    WHERE t.shard_id = e.shard_id AND t.workflow_id = e.workflow_id)
   AND NOT EXISTS (SELECT 1 FROM timers m
                    WHERE m.shard_id = e.shard_id AND m.workflow_id = e.workflow_id)`

// DeleteAbandonedStart removes one executions row that has no history at all.
// It reports whether a row was actually removed, so a caller that was racing
// something else learns it lost rather than assuming it won.
func (s *Store) DeleteAbandonedStart(ctx context.Context, shard core.ShardID, workflowID string, olderThan time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, deleteAbandonedStartSQL,
		int32(shard), workflowID, olderThan.Seconds())
	if err != nil {
		return false, fmt.Errorf("eventlog: delete abandoned start %q: %w", workflowID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ---- small pure helpers ---------------------------------------------------

// defaultListLimit and maxListLimit bound an admin query so a caller that
// forgets to page cannot pull an entire shard into memory.
const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

// clampLimit turns a caller-supplied limit into one the database may safely
// execute. Zero and negative mean "unspecified", not "none".
func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultListLimit
	case n > maxListLimit:
		return maxListLimit
	default:
		return n
	}
}

// listExecutionsQuery builds the List statement. It is split out from List so
// the argument order and the optional status filter can be unit tested without
// a database, which is the only part of List that is easy to get wrong.
func listExecutionsQuery(shard core.ShardID, status core.ExecutionStatus, limit int) (string, []any) {
	const cols = `SELECT shard_id, workflow_id, run_id::text, workflow_type, status, last_seq, created_at FROM executions`

	args := []any{int32(shard)}
	sql := cols + ` WHERE shard_id = $1`
	if status != "" {
		args = append(args, string(status))
		sql += ` AND status = $2`
	}
	args = append(args, clampLimit(limit))
	// Newest first. Ordering by workflow_id instead would be alphabetical, which
	// buries a run the operator just started under thousands of older rows whose
	// ids happen to sort earlier, so the list looks frozen on historical data.
	// workflow_id breaks ties, keeping the order total and paging stable.
	sql += fmt.Sprintf(` ORDER BY created_at DESC, workflow_id LIMIT $%d`, len(args))
	return sql, args
}

// isUniqueViolation reports whether err is a Postgres 23505. We match on the
// SQLSTATE rather than the message so it survives a server locale change.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// nullTime renders a zero time.Time as SQL NULL so a COALESCE in the statement
// can substitute now(). Callers that do not care about ordering leave the
// field zero and let the database stamp it.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// nullSeq renders seq 0 as SQL NULL, which is how the schema spells "this task
// was not created by a scheduling event".
func nullSeq(s core.Seq) any {
	if s == 0 {
		return nil
	}
	return int64(s)
}

// nullString renders "" as SQL NULL. Used for lock_token, which is a UUID
// column and therefore cannot accept an empty string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// jsonOrNull guarantees a non-NULL jsonb value. The payload column is NOT
// NULL, and an event with no payload is common, so we store the JSON literal
// null rather than failing the whole commit.
func jsonOrNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
