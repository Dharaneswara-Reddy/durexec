// Package timers implements durable sleep.
//
// When a workflow says "wait three days", nothing in the engine waits. There is
// no goroutine parked for three days, because a goroutine dies with its process
// and a workflow must survive a restart, a deploy, or the machine being
// replaced. Instead the wait is written down: one row in the timers table with
// the time it should fire. Every node forgets about it entirely.
//
// A scanner then does the only other half of the job. Once a second it asks the
// database which rows are now due, turns each one into a workflow task, and
// deletes the row. The workflow wakes up because a task appeared for it — the
// same way it wakes up for anything else.
package timers

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// DB is the slice of pgx this package uses.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the PostgreSQL implementation of core.TimerStore.
type Store struct{ db DB }

var _ core.TimerStore = (*Store)(nil)

// New builds a Store over a pool.
func New(pool *pgxpool.Pool) *Store { return &Store{db: pool} }

// NewWithDB builds a Store over any query runner.
func NewWithDB(db DB) *Store { return &Store{db: db} }

// defaultDueLimit bounds one scan. A shard that accumulated a million timers
// while a node was down must not try to wake them all in one pass; taking a
// bounded batch every second drains the backlog without a single enormous
// transaction and without starving live work.
const defaultDueLimit = 100

// clampLimit turns a caller-supplied limit into one that is safe to execute.
func clampLimit(n int) int {
	if n <= 0 {
		return defaultDueLimit
	}
	return n
}

// dueSQL selects the timers whose moment has arrived.
//
// Ordering by fire_at means the most overdue timer is always handled first, so a
// backlog drains in the order the workflows expected rather than in whatever
// order the rows happen to sit on disk. The timers_due index on
// (shard_id, fire_at) serves both the filter and the sort.
const dueSQL = `
SELECT shard_id, workflow_id, run_id::text, timer_seq, fire_at
  FROM timers
 WHERE shard_id = $1 AND fire_at <= now()
 ORDER BY fire_at
 LIMIT $2`

// Due returns the timers in a shard that are ready to fire, oldest first.
func (s *Store) Due(ctx context.Context, shard core.ShardID, limit int) ([]core.Timer, error) {
	rows, err := s.db.Query(ctx, dueSQL, int32(shard), clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("timers: due for shard %d: %w", shard, err)
	}
	defer rows.Close()

	var out []core.Timer
	for rows.Next() {
		var (
			t       core.Timer
			id      int32
			runText string
		)
		if err := rows.Scan(&id, &t.WorkflowID, &runText, &t.TimerSeq, &t.FireAt); err != nil {
			return nil, fmt.Errorf("timers: scan timer: %w", err)
		}
		t.ShardID = core.ShardID(id)
		t.RunID = core.RunID(runText)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timers: due for shard %d: %w", shard, err)
	}
	return out, nil
}

const deleteSQL = `
DELETE FROM timers
 WHERE shard_id = $1 AND workflow_id = $2 AND run_id = $3::uuid AND timer_seq = $4`

// Delete removes a fired timer. Deleting a timer that is already gone is not an
// error: two scanners racing on the same shard is possible for a moment during a
// handover, and the loser has simply found the work already done.
func (s *Store) Delete(ctx context.Context, t core.Timer) error {
	_, err := s.db.Exec(ctx, deleteSQL,
		int32(t.ShardID), t.WorkflowID, string(t.RunID), int64(t.TimerSeq))
	if err != nil {
		return fmt.Errorf("timers: delete timer %d for %q: %w", t.TimerSeq, t.WorkflowID, err)
	}
	return nil
}
