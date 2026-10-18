package eventlog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---- the fencing check ----------------------------------------------------

// verifyTokenSQL re-reads the shard's lease and holds a row lock on it.
//
// FOR UPDATE is the load-bearing part. It takes an exclusive lock on the lease
// row that is held until this transaction ends, so any other transaction that
// tries to read the same row FOR UPDATE — including a rival node's commit, and
// including the lease manager's own Acquire — has to wait for us to finish.
const verifyTokenSQL = `SELECT fencing_token FROM shard_leases WHERE shard_id = $1 FOR UPDATE`

// verifyToken compares the stored fencing token with the one the caller was
// working under. A missing lease row is treated as lost ownership rather than a
// separate error: if there is no lease, we certainly do not hold it.
func verifyToken(ctx context.Context, tx pgx.Tx, shard core.ShardID, tok core.FencingToken) error {
	var stored int64
	err := tx.QueryRow(ctx, verifyTokenSQL, int32(shard)).Scan(&stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("eventlog: shard %d has no lease row: %w", shard, core.ErrLostOwnership)
	case err != nil:
		return fmt.Errorf("eventlog: verify fencing token: %w", err)
	case core.FencingToken(stored) != tok:
		return &FencedError{Shard: shard, Stored: core.FencingToken(stored), Held: tok}
	}
	return nil
}

// FencedError is the one outcome worth counting: the stored fencing token had
// moved on, so this node's write was refused.
//
// It is a type rather than another wrapped sentinel because three different
// things in this package report core.ErrLostOwnership — a token mismatch, a
// shard with no lease row at all, and the duplicate-seq race in insertEvents —
// and only the first is the fencing check firing. A missing lease row means
// EnsureRows never ran, and a duplicate seq means we lost a race we should not
// have been in; counting either as a fencing rejection would put operational
// faults into the one number that is supposed to be evidence that split brain
// was prevented.
//
// It wraps core.ErrLostOwnership, so every existing errors.Is check and the
// error text both behave exactly as they did before.
type FencedError struct {
	Shard core.ShardID
	// Stored is the token the lease row actually held; Held is the one the
	// writer believed it had. Stored is always the higher of the two.
	Stored core.FencingToken
	Held   core.FencingToken
}

func (e *FencedError) Error() string {
	return fmt.Sprintf("eventlog: shard %d fencing token is %d, we hold %d: %v",
		e.Shard, e.Stored, e.Held, core.ErrLostOwnership)
}

func (e *FencedError) Unwrap() error { return core.ErrLostOwnership }

// FencingObserver receives one call per write the fencing check refused. It is
// declared here, as the narrowest interface this package needs, rather than
// importing the metrics package, so the eventlog stays testable with no
// Prometheus registry. internal/obs.Metrics satisfies it.
type FencingObserver interface {
	FencingRejection(shard core.ShardID)
}

// FencingCounter wraps a Committer and counts the commits the fencing check
// refused. Without it, a run in which no split brain ever happened is
// indistinguishable from one in which fencing was silently broken all along.
//
// It is a wrapper rather than a field on Store because counting belongs to the
// observability layer and not to the schema: the store keeps working with no
// metrics at all, and this wrapper can be unit-tested against a fake committer
// with no database.
type FencingCounter struct {
	Committer core.Committer
	Observer  FencingObserver
}

var _ core.Committer = (*FencingCounter)(nil)

// NewFencingCounter wraps c. A nil observer counts nothing rather than
// panicking, so the wrapper can be installed unconditionally.
func NewFencingCounter(c core.Committer, o FencingObserver) *FencingCounter {
	return &FencingCounter{Committer: c, Observer: o}
}

// CommitDecision commits and returns the error untouched. Observing must never
// change what the caller sees: the fenced-out worker has to stop either way.
func (f *FencingCounter) CommitDecision(ctx context.Context, d core.Decision) error {
	err := f.Committer.CommitDecision(ctx, d)
	var fenced *FencedError
	if f.Observer != nil && errors.As(err, &fenced) {
		// The shard comes from the error, not from the decision, so a rejection
		// can never be attributed to a shard it did not happen on.
		f.Observer.FencingRejection(fenced.Shard)
	}
	return err
}

// ---- the statements the commit runs ---------------------------------------

const insertEventSQL = `
INSERT INTO events (shard_id, workflow_id, run_id, seq, event_type, payload, created_at)
VALUES ($1, $2, $3::uuid, $4, $5, $6::jsonb, COALESCE($7, now()))`

const insertTaskSQL = `
INSERT INTO tasks (shard_id, task_type, workflow_id, run_id, scheduled_event_seq, visible_at)
VALUES ($1, $2, $3, $4::uuid, $5, COALESCE($6, now()))`

// Timers and intents are keyed by the sequence number of the event that created
// them, so a duplicate row can only ever be byte-identical to the one already
// there. ON CONFLICT DO NOTHING makes a retried commit harmless instead of
// turning it into a spurious ownership error. Events deliberately do NOT get
// this treatment — there, a collision is real news and must be reported.
const insertTimerSQL = `
INSERT INTO timers (shard_id, workflow_id, run_id, timer_seq, fire_at)
VALUES ($1, $2, $3::uuid, $4, $5)
ON CONFLICT (shard_id, workflow_id, run_id, timer_seq) DO NOTHING`

const insertIntentSQL = `
INSERT INTO activity_intents
       (shard_id, workflow_id, run_id, scheduled_seq, activity_type, intent_text, embedding)
VALUES ($1, $2, $3::uuid, $4, $5, $6, $7::vector)
ON CONFLICT (shard_id, workflow_id, run_id, scheduled_seq) DO NOTHING`

// NULLIF lets a Decision that only advances last_seq leave the status alone,
// rather than forcing every caller to restate a status it did not change.
const updateExecutionSQL = `
UPDATE executions
   SET status = COALESCE(NULLIF($3, ''), status),
       last_seq = $4,
       updated_at = now()
 WHERE shard_id = $1 AND workflow_id = $2`

// The workflow task row is deleted in the same transaction as the events it
// produced. That is what makes the task's completion and its effects one fact:
// if the process dies a moment later the row is gone, and if the transaction
// rolls back the task is still there to be polled again.
const deleteTaskSQL = `DELETE FROM tasks WHERE shard_id = $1 AND task_id = $2`

// ---- CommitDecision -------------------------------------------------------

// CommitDecision applies one workflow task's entire outcome atomically.
//
// The ordering below is not arbitrary. The fencing token is checked FIRST and
// inside THIS transaction, before a single row is written. Checking ownership in
// a separate earlier transaction would be useless: between that check and these
// writes there is a window in which another node can take the shard, and a
// fenced-out node would happily write into it. Doing the check here, with FOR
// UPDATE on the lease row, serialises the ownership question against the writes
// themselves — the lease row stays locked for the rest of the transaction, so
// ownership cannot change between the check and the commit. Either we still own
// the shard and everything lands, or we do not and nothing does.
//
// Order:
//
//	a. verify the fencing token FOR UPDATE      — nothing else happens if this fails
//	b. append the events                        — the PK is the second line of defence
//	c. insert new tasks and timers
//	d. insert activity intents (pgvector)
//	e. update the execution status pointer
//	f. delete the workflow task row we just completed
func (s *Store) CommitDecision(ctx context.Context, d core.Decision) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("eventlog: begin commit: %w", err)
	}
	// Rollback after a successful Commit is a documented no-op in pgx, so this
	// deferred call is safe on every path and guarantees no leaked transaction.
	defer func() { _ = tx.Rollback(ctx) }()

	// (a) Ownership first. Everything after this line depends on it.
	if err := verifyToken(ctx, tx, d.ShardID, d.Token); err != nil {
		return err
	}

	// (b) The log. A unique violation here means another node appended the same
	// seq for this run concurrently. The token check said we owned the shard, so
	// this is a race we lost anyway — the safe reading is that our view of
	// ownership is stale, so we report it the same way and abandon the work.
	if err := s.insertEvents(ctx, tx, d.Events); err != nil {
		return err
	}

	// (c) Work that the decision scheduled.
	if err := s.insertTasks(ctx, tx, d.NewTasks); err != nil {
		return err
	}
	if err := s.insertTimers(ctx, tx, d.NewTimers); err != nil {
		return err
	}

	// (d) Meaning-vectors for the semantic duplicate check.
	if err := s.insertIntents(ctx, tx, d.Intents); err != nil {
		return err
	}

	// (e) The mutable pointer derived from the log we just extended.
	if _, err := tx.Exec(ctx, updateExecutionSQL,
		int32(d.ShardID), d.WorkflowID, string(d.Status), int64(d.LastSeq)); err != nil {
		return fmt.Errorf("eventlog: update execution %q: %w", d.WorkflowID, err)
	}

	// (f) Proof the workflow task is done.
	if d.CompleteTaskID != 0 {
		if _, err := tx.Exec(ctx, deleteTaskSQL, int32(d.ShardID), d.CompleteTaskID); err != nil {
			return fmt.Errorf("eventlog: delete task %d: %w", d.CompleteTaskID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("eventlog: commit decision: %w", err)
	}
	return nil
}

func (s *Store) insertEvents(ctx context.Context, tx pgx.Tx, events []core.Event) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, ev := range events {
		batch.Queue(insertEventSQL,
			int32(ev.ShardID), ev.WorkflowID, string(ev.RunID), int64(ev.Seq),
			string(ev.Type), jsonOrNull(ev.Payload), nullTime(ev.CreatedAt))
	}
	res := tx.SendBatch(ctx, batch)
	defer res.Close()
	for i, ev := range events {
		if _, err := res.Exec(); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("eventlog: seq %d for run %s already appended by another writer: %w",
					ev.Seq, ev.RunID, core.ErrLostOwnership)
			}
			return fmt.Errorf("eventlog: append event %d (seq %d): %w", i, ev.Seq, err)
		}
	}
	return res.Close()
}

func (s *Store) insertTasks(ctx context.Context, tx pgx.Tx, tasks []core.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, t := range tasks {
		batch.Queue(insertTaskSQL,
			int32(t.ShardID), string(t.Type), t.WorkflowID, string(t.RunID),
			nullSeq(t.ScheduledEventSeq), nullTime(t.VisibleAt))
	}
	res := tx.SendBatch(ctx, batch)
	defer res.Close()
	for i := range tasks {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("eventlog: insert task %d: %w", i, err)
		}
	}
	return res.Close()
}

func (s *Store) insertTimers(ctx context.Context, tx pgx.Tx, timers []core.Timer) error {
	if len(timers) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, t := range timers {
		batch.Queue(insertTimerSQL,
			int32(t.ShardID), t.WorkflowID, string(t.RunID), int64(t.TimerSeq), t.FireAt)
	}
	res := tx.SendBatch(ctx, batch)
	defer res.Close()
	for i := range timers {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("eventlog: insert timer %d: %w", i, err)
		}
	}
	return res.Close()
}

func (s *Store) insertIntents(ctx context.Context, tx pgx.Tx, intents []core.Intent) error {
	if len(intents) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, in := range intents {
		batch.Queue(insertIntentSQL,
			int32(in.ShardID), in.WorkflowID, string(in.RunID), int64(in.ScheduledSeq),
			in.ActivityType, in.IntentText, VectorLiteral(in.Embedding))
	}
	res := tx.SendBatch(ctx, batch)
	defer res.Close()
	for i := range intents {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("eventlog: insert intent %d: %w", i, err)
		}
	}
	return res.Close()
}

// ---- pgvector ------------------------------------------------------------

// VectorLiteral renders an embedding in pgvector's text input format:
// a bracketed, comma-separated list with no spaces, such as "[0.1,0.2,0.3]".
//
// We send the literal as an ordinary text parameter and cast it with ::vector in
// the statement. That is deliberate: it means the package needs no pgvector Go
// driver and no custom type registration on every pooled connection, which is
// one fewer thing that can be forgotten when a connection is recycled.
//
// Each component is formatted with 'f' and precision -1, which asks Go for the
// shortest decimal string that round-trips back to the same float32. Nothing is
// lost, and nothing spurious is added.
func VectorLiteral(v []float32) string {
	if len(v) == 0 {
		// pgvector rejects "[]" for a fixed-width column, but returning it keeps
		// this function total; the NOT NULL column is what reports the mistake.
		return "[]"
	}
	var b strings.Builder
	// 12 bytes per component is a good guess for typical embedding values and
	// saves the builder from regrowing on every append for a 384-dim vector.
	b.Grow(2 + len(v)*12)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
