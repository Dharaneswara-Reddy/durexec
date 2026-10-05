package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// RawEvent is one row of the events table, read straight out of PostgreSQL.
type RawEvent struct {
	ShardID    core.ShardID
	WorkflowID string
	RunID      string
	Seq        core.Seq
	Type       core.EventType
	Payload    []byte
}

// CompletedEvent is an ActivityTaskCompleted with its program-order index
// already extracted.
type CompletedEvent struct {
	ShardID    core.ShardID
	WorkflowID string
	RunID      string
	Seq        core.Seq
	K          int
}

// EventReader is the database surface this audit needs.
//
// It is an interface so the duplicate detection can be tested against a
// handwritten event list with a planted duplicate. That matters more here than
// anywhere else in the tool: the check itself is the evidence for NFR-3, and a
// check that has never been shown to fire on a real duplicate is not evidence
// of anything.
type EventReader interface {
	// PrimaryKeyDuplicates counts (shard_id, workflow_id, run_id, seq) tuples
	// appearing more than once.
	PrimaryKeyDuplicates(ctx context.Context, prefix string) (int, error)
	// ActivityCompletedEvents returns every ActivityTaskCompleted event.
	ActivityCompletedEvents(ctx context.Context, prefix string) ([]RawEvent, error)
	// StatusCounts returns how many executions sit in each status.
	StatusCounts(ctx context.Context, prefix string) (map[string]int, error)
	Close()
}

// ---------------------------------------------------------------------------
// Pure analysis.
// ---------------------------------------------------------------------------

// ParseCompleted pulls k out of each payload, returning the events it could
// not read separately rather than dropping them.
//
// An unreadable payload is reported rather than skipped because this audit's
// conclusion is "we found nothing", and "we found nothing" is only meaningful
// if the number of rows that were never looked at is zero.
func ParseCompleted(raw []RawEvent) (events []CompletedEvent, unreadable []RawEvent) {
	for _, r := range raw {
		var p core.ActivityCompleted
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			unreadable = append(unreadable, r)
			continue
		}
		events = append(events, CompletedEvent{
			ShardID:    r.ShardID,
			WorkflowID: r.WorkflowID,
			RunID:      r.RunID,
			Seq:        r.Seq,
			K:          p.K,
		})
	}
	return events, unreadable
}

// DuplicateK is one workflow step that completed more than once.
type DuplicateK struct {
	ShardID    core.ShardID
	WorkflowID string
	RunID      string
	K          int
	Count      int
	Seqs       []core.Seq
}

// FindDuplicateK reports every (shard, workflow, run, k) that has more than
// one ActivityTaskCompleted event.
//
// This is the check that carries NFR-3. k is the position of a step in the
// workflow's own program order, so two completions at the same k mean the
// engine journaled the same step finishing twice — which is what a duplicated
// side effect looks like from the log. The result is sorted so two runs of the
// audit over the same data produce identical output.
func FindDuplicateK(evs []CompletedEvent) []DuplicateK {
	type key struct {
		shard core.ShardID
		wf    string
		run   string
		k     int
	}
	seqs := map[key][]core.Seq{}
	for _, e := range evs {
		kk := key{e.ShardID, e.WorkflowID, e.RunID, e.K}
		seqs[kk] = append(seqs[kk], e.Seq)
	}

	out := make([]DuplicateK, 0)
	for kk, ss := range seqs {
		if len(ss) < 2 {
			continue
		}
		sorted := append([]core.Seq(nil), ss...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		out = append(out, DuplicateK{
			ShardID: kk.shard, WorkflowID: kk.wf, RunID: kk.run,
			K: kk.k, Count: len(sorted), Seqs: sorted,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.ShardID != b.ShardID:
			return a.ShardID < b.ShardID
		case a.WorkflowID != b.WorkflowID:
			return a.WorkflowID < b.WorkflowID
		case a.RunID != b.RunID:
			return a.RunID < b.RunID
		default:
			return a.K < b.K
		}
	})
	return out
}

// countRuns returns how many distinct (shard, workflow, run) triples the
// events cover, which is the denominator NFR-3 is stated over.
func countRuns(evs []CompletedEvent) int {
	type key struct {
		shard core.ShardID
		wf    string
		run   string
	}
	seen := map[key]struct{}{}
	for _, e := range evs {
		seen[key{e.ShardID, e.WorkflowID, e.RunID}] = struct{}{}
	}
	return len(seen)
}

// ---------------------------------------------------------------------------
// PostgreSQL.
// ---------------------------------------------------------------------------

// pgReader is the real EventReader.
type pgReader struct{ pool *pgxpool.Pool }

// NewPGReader connects to the engine's database.
//
// The audit reads the events table directly rather than going through the HTTP
// history endpoint on purpose. The API serves history through the same code
// paths that wrote it, so a bug that duplicated an event could in principle be
// hidden by a read path that de-duplicated on the way out. Going to the tables
// means the check is against what is actually stored.
func NewPGReader(ctx context.Context, dsn string) (EventReader, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging %s: %w", redactDSN(dsn), err)
	}
	return &pgReader{pool: pool}, nil
}

func (r *pgReader) Close() { r.pool.Close() }

const pkDuplicateSQL = `
SELECT count(*) FROM (
    SELECT 1 FROM events
    WHERE workflow_id LIKE $1 || '%'
    GROUP BY shard_id, workflow_id, run_id, seq
    HAVING count(*) > 1
) AS dups`

func (r *pgReader) PrimaryKeyDuplicates(ctx context.Context, prefix string) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, pkDuplicateSQL, prefix).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

const completedSQL = `
SELECT shard_id, workflow_id, run_id::text, seq, payload
FROM events
WHERE event_type = 'ActivityTaskCompleted'
  AND workflow_id LIKE $1 || '%'
ORDER BY shard_id, workflow_id, run_id, seq`

func (r *pgReader) ActivityCompletedEvents(ctx context.Context, prefix string) ([]RawEvent, error) {
	rows, err := r.pool.Query(ctx, completedSQL, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RawEvent
	for rows.Next() {
		var e RawEvent
		if err := rows.Scan(&e.ShardID, &e.WorkflowID, &e.RunID, &e.Seq, &e.Payload); err != nil {
			return nil, err
		}
		e.Type = core.EvActivityCompleted
		out = append(out, e)
	}
	return out, rows.Err()
}

const statusSQL = `
SELECT status, count(*) FROM executions
WHERE workflow_id LIKE $1 || '%'
GROUP BY status`

func (r *pgReader) StatusCounts(ctx context.Context, prefix string) (map[string]int, error) {
	rows, err := r.pool.Query(ctx, statusSQL, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// redactDSN removes the password before a DSN reaches an error message that
// may well be pasted into a report.
func redactDSN(dsn string) string {
	at := -1
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return dsn
	}
	scheme := 0
	for i := 0; i+2 < len(dsn); i++ {
		if dsn[i] == ':' && dsn[i+1] == '/' && dsn[i+2] == '/' {
			scheme = i + 3
			break
		}
	}
	return dsn[:scheme] + "***" + dsn[at:]
}
