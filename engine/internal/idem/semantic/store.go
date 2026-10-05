package semantic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Store is the PostgreSQL implementation of core.IntentStore, backed by the
// activity_intents table and the pgvector extension.
type Store struct {
	pool *pgxpool.Pool
}

var _ core.IntentStore = (*Store)(nil)

// NewStore wraps an already-configured pool. Pool lifecycle stays with the
// caller, which shares one pool across every store in the process.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// The vector is bound as a text literal and cast with $n::vector rather than
// through a driver type. pgvector's wire type is not one pgx knows natively,
// and registering a custom codec would mean every pool in the process had to
// be constructed through this package. A literal cast keeps the dependency at
// exactly "pgx can send a string", which is what lets the other agents build
// their own pools without importing this one.
const insertIntentSQL = `
INSERT INTO activity_intents
       (shard_id, workflow_id, run_id, scheduled_seq, activity_type, intent_text, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7::vector)
ON CONFLICT (shard_id, workflow_id, run_id, scheduled_seq) DO NOTHING`

// Put records one intent.
//
// The conflict is ignored rather than treated as an error because this row is
// written from the same commit path that may be retried after a crash. The
// primary key is (shard, workflow, run, scheduled_seq), so a re-run writes
// byte-identical data; making the retry fail would turn a harmless replay into
// a stuck workflow.
func (s *Store) Put(ctx context.Context, in core.Intent) error {
	_, err := s.pool.Exec(ctx, insertIntentSQL,
		int32(in.ShardID),
		in.WorkflowID,
		string(in.RunID),
		int64(in.ScheduledSeq),
		in.ActivityType,
		in.IntentText,
		VectorLiteral(in.Embedding),
	)
	if err != nil {
		return fmt.Errorf("semantic: put intent: %w", err)
	}
	return nil
}

// Rows are scoped to one run and one activity type, and ordered by
// scheduled_seq so the candidate list is in program order. Deterministic
// ordering matters: when two candidates tie on similarity, the winner must be
// the same one on every machine that reads these rows.
//
// There is no status column to filter on, and none is needed. An intent row is
// written only by the commit that records an activity's successful completion,
// so every row that exists is by construction a completed invocation. That
// convention is the reason the schema stays four narrow columns wide instead
// of carrying a lifecycle of its own.
const selectCandidatesSQL = `
SELECT shard_id, workflow_id, run_id::text, scheduled_seq, activity_type, intent_text, embedding::text
  FROM activity_intents
 WHERE shard_id = $1 AND workflow_id = $2 AND run_id = $3 AND activity_type = $4
 ORDER BY scheduled_seq`

// Candidates returns prior completed invocations of the same activity type in
// the same run. The set is bounded by the length of one workflow's history, so
// the caller can compare against all of them.
func (s *Store) Candidates(ctx context.Context, shard core.ShardID, workflowID string, run core.RunID, activityType string) ([]core.Intent, error) {
	rows, err := s.pool.Query(ctx, selectCandidatesSQL,
		int32(shard), workflowID, string(run), activityType)
	if err != nil {
		return nil, fmt.Errorf("semantic: load candidates: %w", err)
	}
	defer rows.Close()

	var out []core.Intent
	for rows.Next() {
		var (
			in      core.Intent
			runText string
			vecText string
		)
		if err := rows.Scan(&in.ShardID, &in.WorkflowID, &runText, &in.ScheduledSeq,
			&in.ActivityType, &in.IntentText, &vecText); err != nil {
			return nil, fmt.Errorf("semantic: scan candidate: %w", err)
		}
		vec, err := ParseVector(vecText)
		if err != nil {
			return nil, fmt.Errorf("semantic: candidate at seq %d: %w", in.ScheduledSeq, err)
		}
		in.RunID = core.RunID(runText)
		in.Embedding = vec
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic: load candidates: %w", err)
	}
	return out, nil
}

// VectorLiteral renders a vector in pgvector's text form, "[1,2,3]".
//
// Components are printed with the shortest text that reads back as the same
// float32, so a value that goes into the database comes out of it bit-identical
// and a similarity score computed before a restart matches one computed after.
// It is a pure function so the encoding can be tested without a database.
func VectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*12 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// ParseVector reads pgvector's text form back into a slice. Pure, for the same
// reason VectorLiteral is.
func ParseVector(s string) ([]float32, error) {
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '[' || t[len(t)-1] != ']' {
		return nil, fmt.Errorf("semantic: malformed vector literal %q", s)
	}
	body := strings.TrimSpace(t[1 : len(t)-1])
	if body == "" {
		return []float32{}, nil
	}
	parts := strings.Split(body, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("semantic: vector component %d: %w", i, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}
