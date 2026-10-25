package lease

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/testdb"
)

// Creates its own schema; fails (not skips) when DUREXEC_REQUIRE_DB is set and
// no database is available. See internal/testdb.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testdb.Pool(t)
}

func freshShard(t *testing.T, pool *pgxpool.Pool) core.ShardID {
	t.Helper()
	shard := core.ShardID(500000 + rand.Int31n(1000000))
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM shard_leases WHERE shard_id=$1", int32(shard))
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO shard_leases (shard_id, owner_node, fencing_token, expires_at) VALUES ($1, NULL, 0, now())",
		int32(shard)); err != nil {
		t.Fatalf("seed lease row: %v", err)
	}
	return shard
}

func TestAcquireIsExclusiveAndBumpsTheToken(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	s := New(pool)
	ctx := context.Background()

	tok, err := s.Acquire(ctx, shard, "node-a", 60)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if tok != 1 {
		t.Fatalf("first token = %d, want 1", tok)
	}

	// A second node must be refused while the lease is live.
	if _, err := s.Acquire(ctx, shard, "node-b", 60); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("rival acquire: got %v, want ErrLeaseHeld", err)
	}

	// The holder re-acquiring gets a new token, which invalidates anything its
	// previous incarnation might still be trying to write.
	tok2, err := s.Acquire(ctx, shard, "node-a", 60)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if tok2 != tok+1 {
		t.Fatalf("re-acquire token = %d, want %d", tok2, tok+1)
	}
}

func TestAnExpiredLeaseIsTakenOver(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	s := New(pool)
	ctx := context.Background()

	// A one-second lease that we then let lapse.
	tokA, err := s.Acquire(ctx, shard, "node-a", 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)

	tokB, err := s.Acquire(ctx, shard, "node-b", 60)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if tokB <= tokA {
		t.Fatalf("takeover token %d did not advance past %d", tokB, tokA)
	}

	// node-a is now fenced out, and must find that out the moment it renews.
	if err := s.Renew(ctx, shard, "node-a", tokA, 60); !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("stale renew: got %v, want ErrLostOwnership", err)
	}
	// The rightful owner renews fine.
	if err := s.Renew(ctx, shard, "node-b", tokB, 60); err != nil {
		t.Fatalf("owner renew: %v", err)
	}
}

func TestReleaseFreesTheShardButKeepsTheToken(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	s := New(pool)
	ctx := context.Background()

	tok, err := s.Acquire(ctx, shard, "node-a", 60)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.Release(ctx, shard, "node-a", tok); err != nil {
		t.Fatalf("release: %v", err)
	}

	l, err := s.Get(ctx, shard)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if l.Owner != "" {
		t.Fatalf("owner after release = %q, want empty", l.Owner)
	}
	// The token must survive: it is only ever allowed to increase.
	if l.Token != tok {
		t.Fatalf("token after release = %d, want %d", l.Token, tok)
	}

	next, err := s.Acquire(ctx, shard, "node-b", 60)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if next != tok+1 {
		t.Fatalf("token after handover = %d, want %d", next, tok+1)
	}
}

func TestVerifyTokenInsideATransaction(t *testing.T) {
	pool := testPool(t)
	shard := freshShard(t, pool)
	s := New(pool)
	ctx := context.Background()

	tok, err := s.Acquire(ctx, shard, "node-a", 60)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	inTx(t, pool, func(tx pgx.Tx) {
		if err := VerifyToken(ctx, tx, shard, tok); err != nil {
			t.Fatalf("VerifyToken with the current token: %v", err)
		}
		if err := VerifyToken(ctx, tx, shard, tok-1); !errors.Is(err, core.ErrLostOwnership) {
			t.Fatalf("VerifyToken with a stale token: got %v, want ErrLostOwnership", err)
		}
	})

	// A shard with no row at all is also lost ownership, not a separate error.
	inTx(t, pool, func(tx pgx.Tx) {
		if err := VerifyToken(ctx, tx, core.ShardID(-1), 1); !errors.Is(err, core.ErrLostOwnership) {
			t.Fatalf("VerifyToken on a missing row: got %v, want ErrLostOwnership", err)
		}
	})
}

func TestEnsureRowsIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := New(pool)

	// This test has to start from no rows at all, which is the one thing it
	// must not LEAVE behind.
	//
	// DUREXEC_TEST_DSN is normally pointed at the demonstration cluster's own
	// database — the Makefile's `test` target says to export it to include
	// these tests — and a shard with no lease row cannot be recovered by a
	// running node. Acquire is an UPDATE, so it matches nothing; only
	// EnsureRows creates rows, and that runs at startup. Deleting these rows
	// and walking away therefore killed shards 0-3 on the live cluster until
	// somebody restarted a node, while every /healthz stayed green. So the
	// cleanup puts the rows back, unowned, exactly as a fresh migration leaves
	// them.
	clean := func() { _, _ = pool.Exec(ctx, "DELETE FROM shard_leases WHERE shard_id < 4") }
	restore := func() {
		clean()
		if err := New(pool).EnsureRows(ctx, 4); err != nil {
			t.Errorf("restoring shard_leases rows 0-3: %v", err)
		}
	}
	clean()
	t.Cleanup(restore)

	if err := s.EnsureRows(ctx, 4); err != nil {
		t.Fatalf("EnsureRows: %v", err)
	}
	tok, err := s.Acquire(ctx, 0, "node-a", 60)
	if err != nil {
		t.Fatalf("acquire shard 0: %v", err)
	}

	// Running it again on every startup must not disturb a token already in use.
	if err := s.EnsureRows(ctx, 4); err != nil {
		t.Fatalf("second EnsureRows: %v", err)
	}
	l, err := s.Get(ctx, 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if l.Token != tok || l.Owner != "node-a" {
		t.Fatalf("EnsureRows disturbed a live lease: %+v", l)
	}

	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[core.ShardID]bool{}
	for _, l := range all {
		seen[l.ShardID] = true
	}
	for i := core.ShardID(0); i < 4; i++ {
		if !seen[i] {
			t.Fatalf("EnsureRows(4) did not create a row for shard %d", i)
		}
	}
}

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fn(tx)
}
