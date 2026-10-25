package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventRow is one row of the events table, read exactly as stored.
//
// RunID is a string rather than a uuid type because this tool never compares
// run ids to anything but each other, and the text form is what goes into a
// report an examiner reads.
// The json tags match core.Event's wire form, so the same struct decodes a row
// read straight from PostgreSQL and a row returned by GET /v1/workflows/{id}/history.
// Comparing those two is how FR-15 is checked, and it only works if one type
// can hold both.
type EventRow struct {
	ShardID    int32           `json:"shard_id"`
	WorkflowID string          `json:"workflow_id"`
	RunID      string          `json:"run_id"`
	Seq        int64           `json:"seq"`
	Type       string          `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ExecRow is one row of the executions table: the mutable status pointer.
type ExecRow struct {
	ShardID      int32
	WorkflowID   string
	RunID        string
	WorkflowType string
	Status       string
	LastSeq      int64
}

// LeaseRow is one row of shard_leases: who owns a shard and with which token.
type LeaseRow struct {
	ShardID   int32
	Owner     string
	Token     int64
	ExpiresAt time.Time
}

// Orphan is a workflow the database says is RUNNING although nothing is due to
// happen for it: no queued task and no pending timer.
//
// This is the only shape "lost work" can take in this design. Every unit of
// future progress is a row — a task or a timer — so a running execution with
// neither is a workflow that will never advance again, which is precisely what
// NFR-1's "zero loss" forbids.
type Orphan struct {
	ShardID    int32
	WorkflowID string
	Status     string
	LastSeq    int64
	// AgeSeconds is how long ago the execution row was created. Zero when the
	// store cannot say, which a check must treat as "too young to judge".
	AgeSeconds float64
}

// Store is the database half of the tool. It is an interface so the checks can
// be exercised against a fake with no PostgreSQL anywhere, which is how every
// test in this package runs.
type Store interface {
	// Events returns history for every run, ordered by (shard, workflow, run,
	// seq). truncated reports that the row cap was reached, which invalidates
	// the gap analysis and must be surfaced rather than analysed around.
	Events(ctx context.Context, limit int) (rows []EventRow, truncated bool, err error)
	Executions(ctx context.Context) ([]ExecRow, error)
	Leases(ctx context.Context) ([]LeaseRow, error)
	Orphans(ctx context.Context) ([]Orphan, error)
	// SeqKeyDuplicates runs the (shard_id, workflow_id, run_id, seq)
	// GROUP BY over the whole table. It is weak evidence by construction —
	// that tuple is the primary key — and is reported as such.
	SeqKeyDuplicates(ctx context.Context) ([]string, error)
	// WorkflowEvents returns the current run's history for one workflow,
	// straight from the table, for comparison with the query API.
	WorkflowEvents(ctx context.Context, workflowID string) ([]EventRow, error)
	// ReplicationStreams counts standbys streaming from this server.
	ReplicationStreams(ctx context.Context) (int, error)
	Close()
}

// pgStore is the live implementation.
type pgStore struct{ pool *pgxpool.Pool }

// OpenStore connects to the engine's database. It is read-only by convention:
// every statement below is a SELECT, because an acceptance tool that could
// write to the log it is auditing would be worthless as evidence.
func OpenStore(ctx context.Context, dsn string) (Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// A handful of connections is plenty; this tool issues a few large reads.
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &pgStore{pool: pool}, nil
}

func (s *pgStore) Close() { s.pool.Close() }

func (s *pgStore) Events(ctx context.Context, limit int) ([]EventRow, bool, error) {
	// limit+1 rows are requested so that hitting the cap is detectable. A
	// truncated history would show a phantom gap at the cut, and reporting a
	// gap that is an artefact of this tool's own paging would be worse than
	// reporting nothing.
	const q = `
SELECT shard_id, workflow_id, run_id::text, seq, event_type, payload, created_at
FROM events
ORDER BY shard_id, workflow_id, run_id, seq
LIMIT $1`
	rows, err := s.pool.Query(ctx, q, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	out := make([]EventRow, 0, 1024)
	for rows.Next() {
		var e EventRow
		var payload []byte
		if err := rows.Scan(&e.ShardID, &e.WorkflowID, &e.RunID, &e.Seq, &e.Type, &payload, &e.CreatedAt); err != nil {
			return nil, false, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func (s *pgStore) Executions(ctx context.Context) ([]ExecRow, error) {
	const q = `
SELECT shard_id, workflow_id, run_id::text, workflow_type, status, last_seq
FROM executions
ORDER BY shard_id, workflow_id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExecRow
	for rows.Next() {
		var e ExecRow
		if err := rows.Scan(&e.ShardID, &e.WorkflowID, &e.RunID, &e.WorkflowType, &e.Status, &e.LastSeq); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) Leases(ctx context.Context) ([]LeaseRow, error) {
	const q = `
SELECT shard_id, COALESCE(owner_node, ''), fencing_token, expires_at
FROM shard_leases
ORDER BY shard_id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LeaseRow
	for rows.Next() {
		var l LeaseRow
		if err := rows.Scan(&l.ShardID, &l.Owner, &l.Token, &l.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *pgStore) Orphans(ctx context.Context) ([]Orphan, error) {
	const q = `
SELECT e.shard_id, e.workflow_id, e.status, e.last_seq,
       extract(epoch FROM (now() - e.created_at))::float8
FROM executions e
WHERE e.status = 'RUNNING'
  AND NOT EXISTS (
        SELECT 1 FROM tasks t
        WHERE t.shard_id = e.shard_id AND t.workflow_id = e.workflow_id)
  AND NOT EXISTS (
        SELECT 1 FROM timers tm
        WHERE tm.shard_id = e.shard_id AND tm.workflow_id = e.workflow_id)
ORDER BY e.shard_id, e.workflow_id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Orphan
	for rows.Next() {
		var o Orphan
		if err := rows.Scan(&o.ShardID, &o.WorkflowID, &o.Status, &o.LastSeq, &o.AgeSeconds); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *pgStore) SeqKeyDuplicates(ctx context.Context) ([]string, error) {
	// The same query as deploy/assert_no_duplicates.sql, so `make verify` and
	// this tool can never disagree about what "duplicate" means here.
	const q = `
SELECT shard_id, workflow_id, run_id::text, seq, count(*)
FROM events
GROUP BY shard_id, workflow_id, run_id, seq
HAVING count(*) > 1
ORDER BY shard_id, workflow_id, run_id, seq
LIMIT 50`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			shard       int32
			wf, run     string
			seq, copies int64
		)
		if err := rows.Scan(&shard, &wf, &run, &seq, &copies); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("shard %d / %s / run %s / seq %d appears %d times",
			shard, wf, shortRun(run), seq, copies))
	}
	return out, rows.Err()
}

func (s *pgStore) WorkflowEvents(ctx context.Context, workflowID string) ([]EventRow, error) {
	const q = `
SELECT e.shard_id, e.workflow_id, e.run_id::text, e.seq, e.event_type, e.payload, e.created_at
FROM events e
JOIN executions x
  ON x.shard_id = e.shard_id AND x.workflow_id = e.workflow_id AND x.run_id = e.run_id
WHERE x.workflow_id = $1
ORDER BY e.seq`
	rows, err := s.pool.Query(ctx, q, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var payload []byte
		if err := rows.Scan(&e.ShardID, &e.WorkflowID, &e.RunID, &e.Seq, &e.Type, &payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) ReplicationStreams(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_replication WHERE state = 'streaming'`).Scan(&n)
	return n, err
}

// StandbyStatus is what the replication follower says about itself.
type StandbyStatus struct {
	InRecovery bool
	// LagSeconds is the age of the last replayed transaction, or -1 when the
	// standby has not replayed one since it started.
	LagSeconds float64
}

// ProbeStandby connects to the follower and asks whether it is replaying. It
// is a separate connection, not part of Store, because the follower is a
// different server and is optional: FR-9 is MANUAL whatever this returns.
func ProbeStandby(ctx context.Context, dsn string) (StandbyStatus, error) {
	var st StandbyStatus
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return st, err
	}
	defer conn.Close(ctx)
	err = conn.QueryRow(ctx, `
SELECT pg_is_in_recovery(),
       COALESCE(EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp())::float8, -1)`).
		Scan(&st.InRecovery, &st.LagSeconds)
	return st, err
}
