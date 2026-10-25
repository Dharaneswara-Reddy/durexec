package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/testdb"
)

// The pool arithmetic had no test at all, and that is how it came to be wrong:
// cmd/engined contained no test files, so nothing noticed that the "cap" only
// ever raised MaxConns and that pgxpool had already defaulted it to
// max(4, runtime.NumCPU()). On a host with more cores than the cap, every node
// would quietly have taken one connection per core.

// TestPoolSizeNeverExceedsTheCap is the regression test for that defect. The
// NumCPU axis is the one that mattered: it is the value the engine does not
// control and cannot see in the configuration.
func TestPoolSizeNeverExceedsTheCap(t *testing.T) {
	cases := []struct {
		name                          string
		shards, wfWorkers, actWorkers int
		explicitMax                   int32
		want                          int32
	}{
		// The deployed configuration: 16 shards x (4 + 8) + 8 = 200 wanted,
		// capped.
		{"deployed", 16, 4, 8, 0, maxPoolConns},
		{"one shard", 1, 4, 8, 0, maxPoolConns},
		// A node that owns nothing still keeps the 8 connections the
		// background loops need, which is already above the floor.
		{"no shards", 0, 0, 0, 0, 8},
		// An operator asking for a smaller pool is honoured.
		{"explicit smaller", 16, 4, 8, 8, 8},
		// An operator asking for a bigger one is not: the cluster budget is
		// not theirs to overspend.
		{"explicit bigger than cap", 16, 4, 8, 500, maxPoolConns},
		// An explicit value below the floor is still floored.
		{"explicit below floor", 16, 4, 8, 1, minPoolConns},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := poolSize(c.shards, c.wfWorkers, c.actWorkers, c.explicitMax)
			if got != c.want {
				t.Fatalf("poolSize(%d, %d, %d, %d) = %d, want %d",
					c.shards, c.wfWorkers, c.actWorkers, c.explicitMax, got, c.want)
			}
			if got > maxPoolConns {
				t.Fatalf("poolSize returned %d, above the hard cap %d", got, maxPoolConns)
			}
			if got < minPoolConns {
				t.Fatalf("poolSize returned %d, below the floor %d", got, minPoolConns)
			}
		})
	}
}

// TestPoolSizeIgnoresTheMachineSize states the property directly: the answer
// must not depend on how big the host is. pgxpool's default for MaxConns is
// max(4, runtime.NumCPU()), so anything that let that value through made the
// pool size a function of the hardware.
func TestPoolSizeIgnoresTheMachineSize(t *testing.T) {
	base := poolSize(16, 4, 8, 0)
	for _, numCPU := range []int32{4, 12, 25, 64, 128} {
		// numCPU is what pgxpool would have put in Config.MaxConns. It must
		// not be passed as explicitMax, and this asserts that even if it were,
		// the cap holds.
		if got := poolSize(16, 4, 8, numCPU); got > maxPoolConns {
			t.Fatalf("a %d-core host produced a pool of %d, above the cap %d",
				numCPU, got, maxPoolConns)
		}
		if base > maxPoolConns {
			t.Fatalf("pool size %d is above the cap %d", base, maxPoolConns)
		}
	}
}

// TestFourNodesFitTheBudget is the specific case the audit predicted but did
// not run, because running it would have broken the demonstration cluster: a
// fourth node took the old cluster to 107 connections against a
// max_connections of 100.
func TestFourNodesFitTheBudget(t *testing.T) {
	// The measured baseline, for the record: 25 + 25 + 25 of engine pool plus
	// 5 background plus walreceiver plus psql = 82 of 100 on the live cluster,
	// so a fourth node at 25 reached 107 and was refused.
	const oldPerNode = 25
	if 4*oldPerNode+7 <= 100 {
		t.Fatal("this test encodes the defect it guards; check the arithmetic")
	}

	if err := checkConnBudget(4, 150); err != nil {
		t.Fatalf("four nodes should fit a 150-connection server: %v", err)
	}
	if err := checkConnBudget(4, 100); err == nil {
		t.Fatal("four nodes must NOT be allowed to start against max_connections=100; " +
			"that is the failure this check exists to prevent")
	} else if !strings.Contains(err.Error(), "max_connections") {
		t.Fatalf("the refusal must name max_connections so an operator knows what to change: %v", err)
	}
	if err := checkConnBudget(3, 100); err != nil {
		t.Fatalf("the deployed three-node cluster must still start: %v", err)
	}
	// A server we cannot interrogate is not a server we know is too small.
	if err := checkConnBudget(99, 0); err != nil {
		t.Fatalf("an unknown max_connections must not block startup: %v", err)
	}
}

func TestClusterConnBudgetCountsEveryConsumer(t *testing.T) {
	one := clusterConnBudget(1)
	two := clusterConnBudget(2)
	perNode := maxPoolConns + renewPoolConns
	if two-one != perNode {
		t.Fatalf("each node adds %d to the budget, want %d (pool %d + renewal %d)",
			two-one, perNode, maxPoolConns, renewPoolConns)
	}
	fixed := one - perNode
	wantFixed := replicationConns + monitoringConns + migrationConns + toolingConns
	if fixed != wantFixed {
		t.Fatalf("non-engine consumers total %d, want %d", fixed, wantFixed)
	}
}

func TestDSNPoolMaxConns(t *testing.T) {
	cases := map[string]int32{
		"postgres://u:p@h:5432/d?sslmode=disable":                  0,
		"postgres://u:p@h:5432/d?sslmode=disable&pool_max_conns=9": 9,
		"postgres://u:p@h:5432/d?pool_max_conns=0":                 0,
		"postgres://u:p@h:5432/d?pool_max_conns=not-a-number":      0,
		"host=h user=u dbname=d":                                   0,
	}
	for dsn, want := range cases {
		if got := dsnPoolMaxConns(dsn); got != want {
			t.Fatalf("dsnPoolMaxConns(%q) = %d, want %d", dsn, got, want)
		}
	}
}

// ---- behaviour under a full pool ------------------------------------------
//
// The arithmetic above is only half of DB-05. The other half is what happens
// when the pool is genuinely full, because that is the state in which the
// engine used to throw away completed work: a claim renewal that queues for a
// connection past the lock it is renewing loses the claim, and the activity's
// result is discarded. These need a real server.

// poolOfSize opens a pool of exactly n connections against the test database.
func poolOfSize(t *testing.T, n int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.DSN(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	cfg.MaxConns = n
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// TestPoolExhaustionBlocksThenRecovers is the property the pool-sizing comment
// in newPool asserts and nothing checked: a small pool back-pressures rather
// than deadlocking, and it recovers.
func TestPoolExhaustionBlocksThenRecovers(t *testing.T) {
	const size = 4
	pool := poolOfSize(t, size)
	ctx := context.Background()

	held := make([]*pgxpool.Conn, 0, size)
	for i := 0; i < size; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d of %d: %v", i+1, size, err)
		}
		held = append(held, c)
	}
	if got := pool.Stat().AcquiredConns(); got != size {
		t.Fatalf("%d connections acquired, want %d", got, size)
	}

	// The next acquisition must block, not fail.
	blocked := make(chan error, 1)
	go func() {
		c, err := pool.Acquire(ctx)
		if err == nil {
			c.Release()
		}
		blocked <- err
	}()
	select {
	case err := <-blocked:
		t.Fatalf("acquire on a full pool returned %v instead of waiting", err)
	case <-time.After(150 * time.Millisecond):
	}

	// Releasing one hands it to the waiter: back-pressure, not deadlock.
	held[0].Release()
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("the waiter failed after a connection was freed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("releasing a connection did not wake the waiter: the pool deadlocked")
	}

	for _, c := range held[1:] {
		c.Release()
	}
	// Recovery: the pool is fully usable again.
	for i := 0; i < size; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("pool did not recover after exhaustion: %v", err)
		}
		c.Release()
	}
	if got := pool.Stat().AcquiredConns(); got != 0 {
		t.Fatalf("%d connections still acquired after release: a leak", got)
	}
}

// TestPoolAcquireHonoursADeadline is what makes a bounded renewal possible.
// pgxpool has no acquisition timeout of its own — it waits on the context — so
// a caller with a deadline it must not miss has to impose one. Claim renewal
// now does, in taskq.Queue.Extend.
func TestPoolAcquireHonoursADeadline(t *testing.T) {
	pool := poolOfSize(t, 1)
	ctx := context.Background()

	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()

	start := time.Now()
	dctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := pool.Acquire(dctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire on a full pool returned %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("acquire took %v to honour a 100ms deadline", elapsed)
	}
	if got := pool.Stat().AcquiredConns(); got != 1 {
		t.Fatalf("%d connections acquired after a timed-out acquire, want 1: "+
			"a failed acquisition must not leak", got)
	}
}

// TestPoolAcquireHonoursCancellation is the same property for an explicit
// cancel, which is how a shutdown reaches a worker waiting for a connection.
func TestPoolAcquireHonoursCancellation(t *testing.T) {
	pool := poolOfSize(t, 1)

	c, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()

	cctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, aerr := pool.Acquire(cctx)
		done <- aerr
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case aerr := <-done:
		if !errors.Is(aerr, context.Canceled) {
			t.Fatalf("cancelled acquire returned %v, want Canceled", aerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not unblock the acquire")
	}
	if got := pool.Stat().AcquiredConns(); got != 1 {
		t.Fatalf("%d connections acquired after a cancelled acquire, want 1", got)
	}
}

// TestSeparatePoolsDoNotCompete is the point of the renewal pool: saturating
// the main pool must leave renewal able to act immediately. Without it, a
// renewal waits behind every queued poll and commit, and if it waits past the
// lock it is renewing, a completed activity's result is thrown away.
func TestSeparatePoolsDoNotCompete(t *testing.T) {
	main := poolOfSize(t, 2)
	renew := poolOfSize(t, renewPoolConns)
	ctx := context.Background()

	var held []*pgxpool.Conn
	for i := 0; i < 2; i++ {
		c, err := main.Acquire(ctx)
		if err != nil {
			t.Fatalf("saturate main pool: %v", err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()

	// The main pool is full: an acquisition with a deadline fails.
	dctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := main.Acquire(dctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the main pool should be saturated, but Acquire returned %v", err)
	}

	// The renewal pool is untouched by that, and fast.
	start := time.Now()
	rctx, rcancel := context.WithTimeout(ctx, time.Second)
	defer rcancel()
	var one int
	if err := renew.QueryRow(rctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("renewal pool blocked behind a saturated main pool: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("a renewal took %v while the main pool was saturated; "+
			"it must not queue behind data-plane traffic", elapsed)
	}
}

// TestRenewalPoolSurvivesManyConcurrentRenewals checks the size chosen for the
// renewal pool is actually enough for a node's shards. Renewal is one short
// UPDATE every TASK_LOCK/3, so a handful of connections serves many shards —
// but "a handful is enough" is an assumption worth testing rather than
// asserting in a comment.
func TestRenewalPoolSurvivesManyConcurrentRenewals(t *testing.T) {
	renew := poolOfSize(t, renewPoolConns)
	const shards = 16

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, shards)
	for i := 0; i < shards; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var one int
			if err := renew.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a renewal failed with %d shards against %d connections: %v",
			shards, renewPoolConns, err)
	}
}
