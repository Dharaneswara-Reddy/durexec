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
)

// Store is the single Postgres-backed implementation of the durable ports.
// One Store serves one shard database; the shard_id column still qualifies
// every statement so a shared database also works.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an already-configured pool. Pool lifecycle stays with the caller,
// because the process may share one pool across several components.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

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

const insertExecutionSQL = `
INSERT INTO executions (shard_id, workflow_id, run_id, workflow_type, status, last_seq)
VALUES ($1, $2, $3, $4, $5, 0)`

// Create opens a new execution. The executions primary key is (shard_id,
// workflow_id) with no run_id, so at most one run can be open per business
// key. That is what makes StartWorkflow idempotent at the front door: a
// duplicate start loses the race on the primary key, and we report that as
// ErrWorkflowAlreadyOpen, which callers treat as success.
func (s *Store) Create(ctx context.Context, shard core.ShardID, workflowID string, run core.RunID, wfType string) error {
	_, err := s.pool.Exec(ctx, insertExecutionSQL,
		int32(shard), workflowID, string(run), wfType, string(core.StatusRunning))
	if isUniqueViolation(err) {
		return core.ErrWorkflowAlreadyOpen
	}
	if err != nil {
		return fmt.Errorf("eventlog: create execution: %w", err)
	}
	return nil
}

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
