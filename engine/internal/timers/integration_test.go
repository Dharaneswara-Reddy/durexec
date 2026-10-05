package timers

import (
	"context"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Skipped unless DUREXEC_TEST_DSN names a PostgreSQL with the schema applied.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DUREXEC_TEST_DSN")
	if dsn == "" {
		t.Skip("set DUREXEC_TEST_DSN to run the PostgreSQL tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestDueOnlyReturnsRipeTimersOldestFirst(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	shard := core.ShardID(300000 + rand.Int31n(1000000))
	clean := func() { _, _ = pool.Exec(ctx, "DELETE FROM timers WHERE shard_id=$1", int32(shard)) }
	clean()
	t.Cleanup(clean)

	run := uuid.NewString()
	rows := []struct {
		seq    int64
		fireAt time.Time
	}{
		{1, time.Now().Add(-time.Hour)},   // long overdue
		{2, time.Now().Add(-time.Minute)}, // just overdue
		{3, time.Now().Add(time.Hour)},    // not yet
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO timers (shard_id, workflow_id, run_id, timer_seq, fire_at) VALUES ($1,'wf',$2::uuid,$3,$4)`,
			int32(shard), run, r.seq, r.fireAt); err != nil {
			t.Fatalf("seed timer %d: %v", r.seq, err)
		}
	}

	s := New(pool)
	due, err := s.Due(ctx, shard, 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("Due returned %d timers, want 2 (the future one must not appear)", len(due))
	}
	// Oldest first, so a backlog drains in the order the workflows expected.
	if due[0].TimerSeq != 1 || due[1].TimerSeq != 2 {
		t.Fatalf("Due out of fire_at order: %d then %d", due[0].TimerSeq, due[1].TimerSeq)
	}

	if err := s.Delete(ctx, due[0]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Deleting a timer that is already gone is a no-op, not a failure: two
	// scanners can briefly overlap during a shard handover.
	if err := s.Delete(ctx, due[0]); err != nil {
		t.Fatalf("second Delete: %v", err)
	}

	again, err := s.Due(ctx, shard, 10)
	if err != nil {
		t.Fatalf("Due after delete: %v", err)
	}
	if len(again) != 1 || again[0].TimerSeq != 2 {
		t.Fatalf("after deleting seq 1, Due = %+v", again)
	}
}

func TestDueRespectsTheBatchLimit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	shard := core.ShardID(400000 + rand.Int31n(1000000))
	clean := func() { _, _ = pool.Exec(ctx, "DELETE FROM timers WHERE shard_id=$1", int32(shard)) }
	clean()
	t.Cleanup(clean)

	run := uuid.NewString()
	for i := int64(1); i <= 5; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO timers (shard_id, workflow_id, run_id, timer_seq, fire_at) VALUES ($1,'wf',$2::uuid,$3, now() - interval '1 minute')`,
			int32(shard), run, i); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	due, err := New(pool).Due(ctx, shard, 2)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("Due returned %d timers, want the requested 2", len(due))
	}
}
