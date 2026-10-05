// Package taskq implements core.TaskQueue on PostgreSQL.
//
// The queue is a lease queue, not a delete-on-read queue. Polling does not
// remove the row; it stamps a lock that expires. If the worker holding the lock
// dies, nothing has to notice — the lock simply lapses and the row becomes
// pollable again. That is the entire crash-recovery story for in-flight work.
package taskq

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ErrLockLost means the row was not updated because the caller's lock token no
// longer matches, or the lock had already expired and been taken by someone
// else. The caller must stop working on that task immediately: another worker
// may already be running it.
var ErrLockLost = errors.New("taskq: lock lost or already expired")

// DB is the slice of *pgxpool.Pool this package uses. Narrowing it keeps the
// package honest about what it touches and lets a caller pass a pool, a
// connection or an already-open transaction.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Config tunes the queue.
type Config struct {
	// LockDuration is how long a poll holds a task before it becomes visible
	// again. It must comfortably exceed the p99 task runtime, or long tasks get
	// picked up twice; a worker that needs longer calls Extend.
	LockDuration time.Duration
	// Backoff is used by Poller to space out retries of a failing task.
	Backoff Backoff
}

// DefaultConfig is a 30s lock with the default jittered backoff.
func DefaultConfig() Config {
	return Config{LockDuration: 30 * time.Second, Backoff: DefaultBackoff()}
}

func (c Config) withDefaults() Config {
	if c.LockDuration <= 0 {
		c.LockDuration = DefaultConfig().LockDuration
	}
	c.Backoff = c.Backoff.withDefaults()
	return c
}

// Queue is the PostgreSQL implementation of core.TaskQueue.
type Queue struct {
	db  DB
	cfg Config
}

var _ core.TaskQueue = (*Queue)(nil)

// New builds a Queue over a pgx pool.
func New(pool *pgxpool.Pool, cfg Config) *Queue { return NewWithDB(pool, cfg) }

// NewWithDB builds a Queue over anything that can run queries, which is how an
// integration test points the queue at a single connection.
func NewWithDB(db DB, cfg Config) *Queue {
	return &Queue{db: db, cfg: cfg.withDefaults()}
}

// Config returns the effective configuration, defaults already applied.
func (q *Queue) Config() Config { return q.cfg }

// interval renders a Go duration as a PostgreSQL interval literal. Everything
// is expressed in milliseconds so there is no locale or unit ambiguity in the
// text the server parses.
func interval(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return strconv.FormatInt(d.Milliseconds(), 10) + " milliseconds"
}

// pollSQL locks exactly one pollable row.
//
// SKIP LOCKED is not an optimisation, it is the whole design. Without it, every
// worker that reaches this statement queues behind the same row-level lock and
// the fleet degrades to one worker at a time. With it, a worker that finds a row
// already being claimed walks straight past it to the next one.
//
// The selection is a CTE joined into the UPDATE, and that shape is load-bearing.
//
// The obvious spelling — UPDATE ... WHERE (shard_id, task_id) IN (SELECT ...
// FOR UPDATE SKIP LOCKED LIMIT 1) — is a well-known PostgreSQL trap and it was
// in this file until it was measured. Under READ COMMITTED, when the outer
// UPDATE meets a row another transaction has just updated, it re-runs the
// uncorrelated subplan during EvalPlanQual. The re-run now sees the winners'
// locks, SKIP LOCKED steps over them, the recheck fails, and the UPDATE modifies
// nothing — so the poller is told "no task available" while tasks are sitting
// there unclaimed.
//
// Measured before the fix, four warm connections polling four available tasks:
// one poller got work and three got nothing, in 12 of 12 rounds. It hid during
// development because cold connections stagger pollers by roughly 17ms, which is
// long enough for each to finish before the next begins.
//
// With the CTE, the lock and the skip happen once while choosing. RETURNING
// hands back the row as it now stands, including the server-generated token.
const pollSQL = `
WITH pick AS (
    SELECT shard_id, task_id FROM tasks
    WHERE shard_id=$1 AND task_type=$2 AND visible_at <= now()
      AND (locked_until IS NULL OR locked_until < now())
    ORDER BY task_id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE tasks t
   SET locked_until = now() + $3::interval,
       lock_token   = gen_random_uuid(),
       attempt      = t.attempt + 1
  FROM pick p
 WHERE t.shard_id = p.shard_id AND t.task_id = p.task_id
RETURNING t.*`

// Poll takes a time-boxed lock on one task, or returns core.ErrNoTask.
func (q *Queue) Poll(ctx context.Context, shard core.ShardID, t core.TaskType) (core.Task, error) {
	rows, err := q.db.Query(ctx, pollSQL, int32(shard), string(t), interval(q.cfg.LockDuration))
	if err != nil {
		return core.Task{}, fmt.Errorf("taskq: poll shard %d: %w", shard, err)
	}
	task, err := collectOneTask(rows)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Task{}, core.ErrNoTask
	}
	if err != nil {
		return core.Task{}, fmt.Errorf("taskq: poll shard %d: %w", shard, err)
	}
	return task, nil
}

// collectOneTask scans the RETURNING * row. The column order is the column
// order of the frozen migration: shard_id, task_id, task_type, workflow_id,
// run_id, scheduled_event_seq, visible_at, locked_until, lock_token, attempt.
func collectOneTask(rows pgx.Rows) (core.Task, error) {
	return pgx.CollectOneRow(rows, func(row pgx.CollectableRow) (core.Task, error) {
		var (
			t         core.Task
			shard     int32
			taskType  string
			runID     pgtype.UUID
			schedSeq  pgtype.Int8
			locked    pgtype.Timestamptz
			lockToken pgtype.UUID
		)
		if err := row.Scan(&shard, &t.TaskID, &taskType, &t.WorkflowID,
			&runID, &schedSeq, &t.VisibleAt, &locked, &lockToken, &t.Attempt); err != nil {
			return core.Task{}, err
		}
		t.ShardID = core.ShardID(shard)
		t.Type = core.TaskType(taskType)
		t.RunID = core.RunID(uuidText(runID))
		t.ScheduledEventSeq = core.Seq(schedSeq.Int64)
		if locked.Valid {
			at := locked.Time
			t.LockedUntil = &at
		}
		t.LockToken = uuidText(lockToken)
		return t, nil
	})
}

// uuidText renders a scanned uuid as its canonical string, empty when NULL.
// core models run and lock ids as strings; the schema stores them as UUID.
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

const extendSQL = `
UPDATE tasks SET locked_until = now() + $4::interval
WHERE shard_id=$1 AND task_id=$2 AND lock_token=$3::uuid AND locked_until > now()`

// Extend renews the lock on a task that is still being worked. It refuses to
// renew a lock that has already lapsed: at that point another worker may hold
// the row, and silently re-taking it would let two workers run the same task.
func (q *Queue) Extend(ctx context.Context, shard core.ShardID, taskID int64, lockToken string) error {
	tok, err := parseToken(lockToken)
	if err != nil {
		return err
	}
	tag, err := q.db.Exec(ctx, extendSQL, int32(shard), taskID, tok, interval(q.cfg.LockDuration))
	if err != nil {
		return fmt.Errorf("taskq: extend task %d: %w", taskID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("taskq: extend task %d: %w", taskID, ErrLockLost)
	}
	return nil
}

const releaseSQL = `
UPDATE tasks SET locked_until = NULL, lock_token = NULL, visible_at = now() + $4::interval
WHERE shard_id=$1 AND task_id=$2 AND lock_token=$3::uuid`

// Release drops the lock and pushes the task's visibility into the future.
//
// visibleIn is in MILLISECONDS. A failed task must not become instantly
// pollable, or a task that fails fast spins the whole fleet at full speed;
// the delay is what turns a retry loop into a backoff.
func (q *Queue) Release(ctx context.Context, shard core.ShardID, taskID int64, lockToken string, visibleIn int64) error {
	tok, err := parseToken(lockToken)
	if err != nil {
		return err
	}
	if visibleIn < 0 {
		visibleIn = 0
	}
	delay := time.Duration(visibleIn) * time.Millisecond
	tag, err := q.db.Exec(ctx, releaseSQL, int32(shard), taskID, tok, interval(delay))
	if err != nil {
		return fmt.Errorf("taskq: release task %d: %w", taskID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("taskq: release task %d: %w", taskID, ErrLockLost)
	}
	return nil
}

// ReleaseAfter is Release with a duration, so callers do not have to remember
// the unit of the interface's int64.
func (q *Queue) ReleaseAfter(ctx context.Context, shard core.ShardID, taskID int64, lockToken string, d time.Duration) error {
	return q.Release(ctx, shard, taskID, lockToken, d.Milliseconds())
}

const completeSQL = `DELETE FROM tasks WHERE shard_id=$1 AND task_id=$2 AND lock_token=$3::uuid`

// Complete deletes a finished task. It is not part of core.TaskQueue because a
// workflow task is normally deleted inside the Committer's transaction (see
// Decision.CompleteTaskID) — that is what makes the commit atomic. This exists
// for work that has no Decision to ride along with, such as an activity task.
func (q *Queue) Complete(ctx context.Context, shard core.ShardID, taskID int64, lockToken string) error {
	tok, err := parseToken(lockToken)
	if err != nil {
		return err
	}
	tag, err := q.db.Exec(ctx, completeSQL, int32(shard), taskID, tok)
	if err != nil {
		return fmt.Errorf("taskq: complete task %d: %w", taskID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("taskq: complete task %d: %w", taskID, ErrLockLost)
	}
	return nil
}

const enqueueSQL = `
INSERT INTO tasks (shard_id, task_type, workflow_id, run_id, scheduled_event_seq, visible_at)
VALUES ($1, $2, $3, $4::uuid, $5, COALESCE($6, now()))`

// Enqueue inserts tasks outside a Decision. task_id is left to the sequence and
// the lock columns to their NULL defaults, so a new row is immediately pollable
// unless VisibleAt says otherwise.
func (q *Queue) Enqueue(ctx context.Context, tasks []core.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for i, t := range tasks {
		run, err := parseRunID(t.RunID)
		if err != nil {
			return fmt.Errorf("taskq: enqueue task %d: %w", i, err)
		}
		var seq *int64
		if t.ScheduledEventSeq != 0 {
			v := int64(t.ScheduledEventSeq)
			seq = &v
		}
		var visible *time.Time
		if !t.VisibleAt.IsZero() {
			at := t.VisibleAt
			visible = &at
		}
		batch.Queue(enqueueSQL, int32(t.ShardID), string(t.Type), t.WorkflowID, run, seq, visible)
	}
	// One round trip for the whole slice: the timer scanner enqueues in bursts.
	res := q.db.SendBatch(ctx, batch)
	defer res.Close()
	for i := range tasks {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("taskq: enqueue task %d: %w", i, err)
		}
	}
	return res.Close()
}

// parseToken validates a lock token before it reaches the server, so a caller
// passing an empty or malformed token gets a clear error rather than a cast
// failure from PostgreSQL.
func parseToken(tok string) (string, error) {
	if tok == "" {
		return "", fmt.Errorf("taskq: empty lock token: %w", ErrLockLost)
	}
	u, err := uuid.Parse(tok)
	if err != nil {
		return "", fmt.Errorf("taskq: malformed lock token %q: %w", tok, err)
	}
	return u.String(), nil
}

func parseRunID(r core.RunID) (string, error) {
	u, err := uuid.Parse(string(r))
	if err != nil {
		return "", fmt.Errorf("run id %q is not a uuid: %w", r, err)
	}
	return u.String(), nil
}
