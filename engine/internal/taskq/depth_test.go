package taskq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---- fakes ----------------------------------------------------------------

// depthRow is one row of the GROUP BY result.
type depthRow struct {
	shard    int32
	taskType string
	n        int64
}

// fakeRows serves a fixed result set. pgx.Rows is embedded rather than
// implemented: the embedded interface is nil, so any method the sampler does
// not use panics instead of quietly returning a zero value.
type fakeRows struct {
	pgx.Rows
	rows []depthRow
	i    int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if len(dest) != 3 {
		return fmt.Errorf("fakeRows: %d destinations, want 3", len(dest))
	}
	*(dest[0].(*int32)) = row.shard
	*(dest[1].(*string)) = row.taskType
	*(dest[2].(*int64)) = row.n
	return nil
}

func (r *fakeRows) Err() error { return r.err }
func (r *fakeRows) Close()     {}

// fakeDepthDB answers the one query the sampler makes.
type fakeDepthDB struct {
	rows     []depthRow
	queryErr error
	rowsErr  error
	queries  int
	lastArgs []any
}

func (f *fakeDepthDB) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	f.queries++
	f.lastArgs = args
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return &fakeRows{rows: f.rows, err: f.rowsErr}, nil
}

func (f *fakeDepthDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("the depth sampler does not write")
}

func (f *fakeDepthDB) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("the depth sampler does not batch")
}

var _ DB = (*fakeDepthDB)(nil)

// recordingDepths captures what was published, with no Prometheus registry.
type recordingDepths struct {
	mu      sync.Mutex
	set     map[depthKey]float64
	cleared []core.ShardID
}

func newRecordingDepths() *recordingDepths {
	return &recordingDepths{set: map[depthKey]float64{}}
}

func (r *recordingDepths) SetQueueDepth(shard core.ShardID, tt core.TaskType, depth float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set[depthKey{shard, tt}] = depth
}

func (r *recordingDepths) ClearQueueDepth(shard core.ShardID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared = append(r.cleared, shard)
	for k := range r.set {
		if k.shard == shard {
			delete(r.set, k)
		}
	}
}

func (r *recordingDepths) depth(shard core.ShardID, tt core.TaskType) (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.set[depthKey{shard, tt}]
	return v, ok
}

func (r *recordingDepths) published() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.set)
}

var _ DepthObserver = (*recordingDepths)(nil)

func newTestSampler(db *fakeDepthDB, obs *recordingDepths, shards ...core.ShardID) *DepthSampler {
	owned := shards
	return NewDepthSampler(NewWithDB(db, DefaultConfig()), DepthConfig{
		Shards:   func() []core.ShardID { return owned },
		Observer: obs,
	})
}

// ---- tests ----------------------------------------------------------------

// A drained queue must report zero, not its last depth. A GROUP BY returns no
// row for a type with no tasks, and a gauge holds its last value forever, so
// publishing only what the query found would freeze the panel at the moment the
// backlog was at its worst.
func TestSamplePublishesZeroForATypeWithNoTasks(t *testing.T) {
	db := &fakeDepthDB{rows: []depthRow{{shard: 0, taskType: "WORKFLOW", n: 3}}}
	obs := newRecordingDepths()
	s := newTestSampler(db, obs, 0)

	if err := s.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got, ok := obs.depth(0, core.TaskWorkflow); !ok || got != 3 {
		t.Fatalf("workflow depth = %v (present %v), want 3", got, ok)
	}
	if got, ok := obs.depth(0, core.TaskActivity); !ok || got != 0 {
		t.Fatalf("activity depth = %v (present %v), want an explicit 0", got, ok)
	}
}

// One query per pass, whatever the shard count. The alternative — a count per
// shard, or worse per poll — is the thing this design exists to avoid.
func TestSampleCostsOneQueryPerPass(t *testing.T) {
	db := &fakeDepthDB{}
	s := newTestSampler(db, newRecordingDepths(), 0, 1, 2, 3)

	for i := 0; i < 3; i++ {
		if err := s.Sample(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if db.queries != 3 {
		t.Fatalf("%d queries for 3 passes over 4 shards, want 3", db.queries)
	}
	ids, ok := db.lastArgs[0].([]int32)
	if !ok || len(ids) != 4 {
		t.Fatalf("shards were passed as %#v, want one []int32 of 4", db.lastArgs[0])
	}
}

// A shard that moves to another node must stop being reported here. The
// dashboard sums the gauge across every node, so a shard reported by two nodes
// at once reads as twice the backlog it really has.
func TestSampleClearsAShardItNoLongerOwns(t *testing.T) {
	db := &fakeDepthDB{rows: []depthRow{
		{shard: 0, taskType: "WORKFLOW", n: 1},
		{shard: 1, taskType: "WORKFLOW", n: 5},
	}}
	obs := newRecordingDepths()
	owned := []core.ShardID{0, 1}
	s := NewDepthSampler(NewWithDB(db, DefaultConfig()), DepthConfig{
		Shards:   func() []core.ShardID { return owned },
		Observer: obs,
	})
	ctx := context.Background()

	if err := s.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := obs.depth(1, core.TaskWorkflow); got != 5 {
		t.Fatalf("setup: shard 1 depth = %v, want 5", got)
	}

	// Shard 1 has gone to another node.
	owned = []core.ShardID{0}
	db.rows = []depthRow{{shard: 0, taskType: "WORKFLOW", n: 1}}
	if err := s.Sample(ctx); err != nil {
		t.Fatal(err)
	}

	if _, ok := obs.depth(1, core.TaskWorkflow); ok {
		t.Fatal("shard 1 is still being reported after ownership moved")
	}
	if _, ok := obs.depth(0, core.TaskWorkflow); !ok {
		t.Fatal("shard 0 was cleared too; only the lost shard should go")
	}

	// And it must not be cleared again on every later pass.
	if err := s.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	if len(obs.cleared) != 1 {
		t.Fatalf("shard cleared %d times, want 1: %v", len(obs.cleared), obs.cleared)
	}
}

// Losing every shard is the failover case, and it must clear rather than freeze.
func TestSampleWithNothingOwnedClearsAndDoesNotQuery(t *testing.T) {
	db := &fakeDepthDB{rows: []depthRow{{shard: 0, taskType: "WORKFLOW", n: 2}}}
	obs := newRecordingDepths()
	owned := []core.ShardID{0}
	s := NewDepthSampler(NewWithDB(db, DefaultConfig()), DepthConfig{
		Shards:   func() []core.ShardID { return owned },
		Observer: obs,
	})
	ctx := context.Background()

	if err := s.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	owned = nil
	if err := s.Sample(ctx); err != nil {
		t.Fatal(err)
	}

	if obs.published() != 0 {
		t.Fatal("a node that owns nothing is still publishing a depth")
	}
	if db.queries != 1 {
		t.Fatalf("%d queries, want 1: there is nothing to count with no shards", db.queries)
	}
}

// A database error must not be published as an empty queue. Writing zeros would
// turn a brief outage into a dashboard saying the backlog cleared.
func TestSampleKeepsTheLastValuesWhenTheQueryFails(t *testing.T) {
	db := &fakeDepthDB{rows: []depthRow{{shard: 0, taskType: "WORKFLOW", n: 4}}}
	obs := newRecordingDepths()
	s := newTestSampler(db, obs, 0)
	ctx := context.Background()

	if err := s.Sample(ctx); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("connection refused")
	db.queryErr = boom
	if err := s.Sample(ctx); !errors.Is(err, boom) {
		t.Fatalf("Sample returned %v, want the query error", err)
	}
	if got, _ := obs.depth(0, core.TaskWorkflow); got != 4 {
		t.Fatalf("depth = %v after a failed pass, want the previous 4 left alone", got)
	}
}

// An error that only appears while iterating is just as much a failed pass.
func TestSampleReportsAnIterationError(t *testing.T) {
	boom := errors.New("connection reset")
	db := &fakeDepthDB{rows: []depthRow{{shard: 0, taskType: "WORKFLOW", n: 1}}, rowsErr: boom}
	obs := newRecordingDepths()
	s := newTestSampler(db, obs, 0)

	if err := s.Sample(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Sample returned %v, want the row error", err)
	}
	if obs.published() != 0 {
		t.Fatal("a half-read result set was published")
	}
}

// A task type the schema has grown since depthTaskTypes was written is reported
// rather than dropped: an unknown type with a backlog is the thing most worth
// seeing.
func TestSampleReportsAnUnknownTaskType(t *testing.T) {
	db := &fakeDepthDB{rows: []depthRow{{shard: 0, taskType: "SAGA_COMPENSATION", n: 6}}}
	obs := newRecordingDepths()
	s := newTestSampler(db, obs, 0)

	if err := s.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := obs.depth(0, core.TaskType("SAGA_COMPENSATION")); !ok || got != 6 {
		t.Fatalf("unknown task type depth = %v (present %v), want 6", got, ok)
	}
}

// A sampler with no observer is a node running without a registry, and it must
// cost nothing rather than panic.
func TestSampleWithNoObserverDoesNothing(t *testing.T) {
	db := &fakeDepthDB{}
	s := NewDepthSampler(NewWithDB(db, DefaultConfig()), DepthConfig{
		Shards: func() []core.ShardID { return []core.ShardID{0} },
	})
	if err := s.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.queries != 0 {
		t.Fatalf("%d queries with no observer, want 0", db.queries)
	}
}

func TestDepthConfigWithDefaults(t *testing.T) {
	if got := (DepthConfig{}).withDefaults().Interval; got != DepthInterval {
		t.Fatalf("default interval = %v, want %v", got, DepthInterval)
	}
	if got := (DepthConfig{Interval: time.Minute}).withDefaults().Interval; got != time.Minute {
		t.Fatalf("configured interval = %v, want a minute", got)
	}
	// The interval is the scrape interval for a reason; if one moves the other
	// has to move with it.
	if DepthInterval != 5*time.Second {
		t.Fatalf("DepthInterval = %v, want 5s to match deploy/prometheus.yml", DepthInterval)
	}
}

// Run must stop on cancellation like every other loop in the engine, and
// stopping is a normal shutdown rather than a failure.
func TestDepthSamplerRunStopsOnCancellation(t *testing.T) {
	db := &fakeDepthDB{}
	s := NewDepthSampler(NewWithDB(db, DefaultConfig()), DepthConfig{
		Interval: time.Millisecond,
		Shards:   func() []core.ShardID { return []core.ShardID{0} },
		Observer: newRecordingDepths(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}
