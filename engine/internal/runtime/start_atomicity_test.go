package runtime

// Regression tests for the start path's atomicity (finding F-01).
//
// THE INVARIANT EVERY TEST IN THIS FILE ASSERTS, in one sentence: if an
// executions row exists for a business key, then its run's start event and its
// first workflow task exist too.
//
// Before the fix a start was two transactions — INSERT INTO executions on the
// pool, and then the Decision carrying the start event and the first task — and
// anything that went wrong in between left the row behind as RUNNING with
// last_seq 0, no events, no task and no timer. Nothing in the engine ever
// touched such a row again, and because the executions primary key excludes
// run_id the client's retry was answered "already open, RUNNING" indefinitely.
// The audit found 19 of them on the demonstration cluster, and reproduced 151
// from 553 starts using nothing but short client timeouts.
//
// These tests therefore do not check an error code. They check the database.
// Every one of them ends by asserting the invariant above directly in SQL, so a
// future change that reopens the gap fails here however it is spelled.
//
// They need a real PostgreSQL, which internal/testdb supplies and which it
// refuses to let be the demonstration cluster.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/api"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
	"github.com/dharaneswar339/durexec/internal/faultinj"
	"github.com/dharaneswar339/durexec/internal/testdb"
)

// ---- harness --------------------------------------------------------------

// startHarness is one isolated shard with a real store and a real lease row.
type startHarness struct {
	pool   *pgxpool.Pool
	store  *eventlog.Store
	engine *Engine
	shard  core.ShardID

	// token is what the engine believes it holds. Tests move the lease row
	// underneath it to make the engine stale.
	token core.FencingToken
	owned bool

	fencing *countingFencingObserver
}

// countingFencingObserver records every write the fencing check refused.
// fencing_rejections_total has never fired on the live cluster, so the tests
// that expect it have to prove it fired rather than assume it would.
type countingFencingObserver struct {
	mu     sync.Mutex
	shards []core.ShardID
}

func (o *countingFencingObserver) FencingRejection(s core.ShardID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.shards = append(o.shards, s)
}

func (o *countingFencingObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.shards)
}

// newStartHarness gives each test its own shard id so tests can run in
// parallel and so no test can see another's rows.
func newStartHarness(t *testing.T) *startHarness {
	t.Helper()
	// A shard id well outside the engine's configured range, so nothing a real
	// node owns can ever poll these rows, and unique per test so tests cannot
	// see one another's.
	return newStartHarnessOn(t, testdb.Pool(t), core.ShardID(500000+int32(uuid.New().ID()%100000)))
}

func newStartHarnessOn(t *testing.T, pool *pgxpool.Pool, shard core.ShardID) *startHarness {
	t.Helper()

	h := &startHarness{
		pool:    pool,
		store:   eventlog.New(pool),
		shard:   shard,
		token:   7,
		owned:   true,
		fencing: &countingFencingObserver{},
	}
	h.clean(t)
	t.Cleanup(func() { h.clean(t) })

	h.setLease(t, h.token)
	h.engine = &Engine{
		Events:     h.store,
		Executions: h.store,
		Committer:  eventlog.NewFencingCounter(h.store, h.fencing),
		Leases:     &fakeLeases{lease: core.Lease{ShardID: shard, Owner: "someone-else"}},
		ShardCount: 16,
		NodeID:     "node-test",
		Clock:      fixedClock{testNow},
		TokenFor: func(core.ShardID) (core.FencingToken, bool) {
			return h.token, h.owned
		},
	}
	return h
}

// helperTimeout bounds every query the harness issues on its own behalf.
//
// None of them is slow, so a deadline never fires in a healthy run. It exists
// so that a test which somehow does end up behind a lock FAILS with a message
// naming the statement, instead of blocking on context.Background() until the
// whole package is killed by a test timeout — which gives no information at all
// about where it stopped.
const helperTimeout = 30 * time.Second

func (h *startHarness) helperCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), helperTimeout)
}

func (h *startHarness) clean(t *testing.T) {
	t.Helper()
	ctx, cancel := h.helperCtx(t)
	defer cancel()
	for _, tbl := range []string{"events", "executions", "tasks", "timers", "activity_intents", "shard_leases"} {
		if _, err := h.pool.Exec(ctx,
			"DELETE FROM "+tbl+" WHERE shard_id = $1", int32(h.shard)); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
}

func (h *startHarness) setLease(t *testing.T, tok core.FencingToken) {
	t.Helper()
	ctx, cancel := h.helperCtx(t)
	defer cancel()
	_, err := h.pool.Exec(ctx,
		`INSERT INTO shard_leases (shard_id, owner_node, fencing_token, expires_at)
		 VALUES ($1, 'node-test', $2, now() + interval '1 hour')
		 ON CONFLICT (shard_id) DO UPDATE SET fencing_token = EXCLUDED.fencing_token`,
		int32(h.shard), int64(tok))
	if err != nil {
		t.Fatalf("set lease: %v", err)
	}
}

func (h *startHarness) req(wfID string) api.StartRequest {
	return api.StartRequest{ShardID: h.shard, WorkflowID: wfID, WorkflowType: "DeliveryOrder"}
}

// shape reads back everything that matters about one business key.
type startShape struct {
	rows    int
	lastSeq int64
	status  string
	runID   string
	events  int
	tasks   int
	timers  int
}

func (h *startHarness) shape(t *testing.T, wfID string) startShape {
	t.Helper()
	ctx, cancel := h.helperCtx(t)
	defer cancel()
	var s startShape
	err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM executions WHERE shard_id=$1 AND workflow_id=$2),
		       COALESCE((SELECT last_seq FROM executions WHERE shard_id=$1 AND workflow_id=$2), -1),
		       COALESCE((SELECT status   FROM executions WHERE shard_id=$1 AND workflow_id=$2), ''),
		       COALESCE((SELECT run_id::text FROM executions WHERE shard_id=$1 AND workflow_id=$2), ''),
		       (SELECT count(*) FROM events WHERE shard_id=$1 AND workflow_id=$2),
		       (SELECT count(*) FROM tasks  WHERE shard_id=$1 AND workflow_id=$2),
		       (SELECT count(*) FROM timers WHERE shard_id=$1 AND workflow_id=$2)`,
		int32(h.shard), wfID).
		Scan(&s.rows, &s.lastSeq, &s.status, &s.runID, &s.events, &s.tasks, &s.timers)
	if err != nil {
		t.Fatalf("read shape of %q: %v", wfID, err)
	}
	return s
}

// assertNothing is the post-condition for every failed start: the business key
// is untouched, so the caller may retry it and will get a real run.
func (h *startHarness) assertNothing(t *testing.T, wfID string) {
	t.Helper()
	got := h.shape(t, wfID)
	if got.rows != 0 || got.events != 0 || got.tasks != 0 || got.timers != 0 {
		t.Fatalf("a failed start left state behind for %q: %+v; want nothing at all", wfID, got)
	}
}

// assertFullyStarted is the post-condition for every successful start.
func (h *startHarness) assertFullyStarted(t *testing.T, wfID string) startShape {
	t.Helper()
	got := h.shape(t, wfID)
	if got.rows != 1 || got.lastSeq != 1 || got.status != string(core.StatusRunning) ||
		got.events != 1 || got.tasks != 1 {
		t.Fatalf("a successful start is incomplete for %q: %+v; want 1 row at last_seq 1 with 1 event and 1 task",
			wfID, got)
	}
	return got
}

// assertNoOrphansInShard is the invariant, asserted in one query over every
// business key the test touched. This is the query no evidence file in the
// project ever ran, and it is the one that found the 19 live orphans.
func (h *startHarness) assertNoOrphansInShard(t *testing.T) {
	t.Helper()
	ctx, cancel := h.helperCtx(t)
	defer cancel()
	rows, err := h.pool.Query(ctx, `
		SELECT e.workflow_id, e.status, e.last_seq,
		       (SELECT count(*) FROM events v WHERE v.shard_id=e.shard_id AND v.workflow_id=e.workflow_id),
		       (SELECT count(*) FROM tasks  t WHERE t.shard_id=e.shard_id AND t.workflow_id=e.workflow_id)
		  FROM executions e
		 WHERE e.shard_id = $1
		   AND NOT EXISTS (SELECT 1 FROM events v
		                    WHERE v.shard_id = e.shard_id AND v.workflow_id = e.workflow_id)`,
		int32(h.shard))
	if err != nil {
		t.Fatalf("orphan probe: %v", err)
	}
	defer rows.Close()
	var orphans []string
	for rows.Next() {
		var (
			wf            string
			status        string
			lastSeq       int64
			events, tasks int
		)
		if err := rows.Scan(&wf, &status, &lastSeq, &events, &tasks); err != nil {
			t.Fatalf("orphan probe scan: %v", err)
		}
		orphans = append(orphans, fmt.Sprintf("%s(status=%s last_seq=%d events=%d tasks=%d)",
			wf, status, lastSeq, events, tasks))
	}
	if len(orphans) > 0 {
		t.Fatalf("%d orphaned execution(s) — accepted work with no history: %v", len(orphans), orphans)
	}
}

// countOrphansInShard is assertNoOrphansInShard's query without the assertion,
// so the detector itself can be tested.
func (h *startHarness) countOrphansInShard(t *testing.T) int {
	t.Helper()
	ctx, cancel := h.helperCtx(t)
	defer cancel()
	var n int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM executions e
		 WHERE e.shard_id = $1
		   AND NOT EXISTS (SELECT 1 FROM events v
		                    WHERE v.shard_id = e.shard_id AND v.workflow_id = e.workflow_id)`,
		int32(h.shard)).Scan(&n); err != nil {
		t.Fatalf("orphan probe: %v", err)
	}
	return n
}

// ---- 0. the detector itself ----------------------------------------------

// Every test below ends by asserting there are no orphans, so that assertion
// had better be capable of failing. This is not pedantry: the audit's central
// complaint was not that a test was wrong but that NO evidence file in the
// project ever ran this query, and 19 orphans accumulated in the gap between
// the measurements. A probe nobody has seen fail is a probe nobody should
// trust.
func TestTheOrphanProbeActuallyDetectsAnOrphan(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()

	if n := h.countOrphansInShard(t); n != 0 {
		t.Fatalf("precondition: %d orphans before anything happened", n)
	}
	// A healthy start must not register as one.
	if _, err := h.engine.StartWorkflow(ctx, h.req("wf-healthy-probe")); err != nil {
		t.Fatalf("start: %v", err)
	}
	if n := h.countOrphansInShard(t); n != 0 {
		t.Fatalf("a healthy start registered as %d orphan(s): the probe has false positives", n)
	}

	// Exactly the shape the old two-transaction start left behind: the
	// executions row committed on its own, with the start event and first task
	// never written. Raw SQL, because the fixed engine cannot produce it.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO executions (shard_id, workflow_id, run_id, workflow_type, status, last_seq)
		 VALUES ($1, 'wf-probe-orphan', $2::uuid, 'DeliveryOrder', 'RUNNING', 0)`,
		int32(h.shard), uuid.NewString()); err != nil {
		t.Fatalf("seed an orphan: %v", err)
	}
	if n := h.countOrphansInShard(t); n != 1 {
		t.Fatalf("the probe reported %d orphans for a seeded orphan, want 1: it cannot detect the defect", n)
	}
}

// ---- 1. crash before the start transaction --------------------------------

// Nothing has been written yet, so there is nothing to lose. This is also the
// pre-flight ownership case: the engine checks the token BEFORE writing, which
// is the opposite of the old order.
func TestStartCrashBeforeTheTransactionLeavesNothing(t *testing.T) {
	h := newStartHarness(t)
	h.owned = false

	_, err := h.engine.StartWorkflow(context.Background(), h.req("wf-before-tx"))
	var notOwner *core.NotOwnerError
	if !errors.As(err, &notOwner) {
		t.Fatalf("start without the lease: got %v, want *core.NotOwnerError", err)
	}
	h.assertNothing(t, "wf-before-tx")
	h.assertNoOrphansInShard(t)
}

// ---- 2. crash after the insert, before the commit -------------------------

// THE regression test for F-01. faultinj.BeforeDecisionCommit fires inside
// CommitDecision at the one instant where the executions INSERT, the start
// event and the first task have all been issued and NONE of them is durable.
// Under the old two-transaction start the row was already committed by this
// point and survived; now the whole thing rolls back.
func TestStartCrashAfterTheInsertBeforeTheCommitLeavesNothing(t *testing.T) {
	h := newStartHarness(t)

	inj := faultinj.New(true)
	inj.CrashAt(faultinj.BeforeDecisionCommit)
	h.store.UseFaultInjector(inj)
	t.Cleanup(func() { h.store.UseFaultInjector(nil) })

	_, err := h.engine.StartWorkflow(context.Background(), h.req("wf-mid-tx"))
	var crash *eventlog.CrashInjectedError
	if !errors.As(err, &crash) {
		t.Fatalf("start with a crash armed at the commit: got %v, want *eventlog.CrashInjectedError", err)
	}
	if inj.Fired() != 1 {
		t.Fatalf("injector fired %d times, want 1 — the seam is not wired to the real boundary", inj.Fired())
	}

	// The whole point of the fix.
	h.assertNothing(t, "wf-mid-tx")
	h.assertNoOrphansInShard(t)

	// And the caller's retry gets a real run rather than "already open".
	res, err := h.engine.StartWorkflow(context.Background(), h.req("wf-mid-tx"))
	if err != nil {
		t.Fatalf("retry after the injected crash: %v", err)
	}
	if res.AlreadyOpen {
		t.Fatal("retry reported AlreadyOpen for a start that never landed")
	}
	h.assertFullyStarted(t, "wf-mid-tx")
}

// ---- 3. crash immediately after the commit --------------------------------

// The commit landed and the process died before answering. The engine is
// consistent and the client cannot tell, so its retry has to be safe: it must
// find the SAME run, not start a second one.
func TestStartCrashImmediatelyAfterCommitIsRecoveredByTheRetry(t *testing.T) {
	h := newStartHarness(t)

	res, err := h.engine.StartWorkflow(context.Background(), h.req("wf-after-commit"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// ...and now the process dies here, before the HTTP response is written.
	landed := h.assertFullyStarted(t, "wf-after-commit")
	if landed.runID != string(res.RunID) {
		t.Fatalf("stored run %s, returned %s", landed.runID, res.RunID)
	}

	retry, err := h.engine.StartWorkflow(context.Background(), h.req("wf-after-commit"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !retry.AlreadyOpen || retry.RunID != res.RunID {
		t.Fatalf("retry = %+v, want AlreadyOpen on run %s", retry, res.RunID)
	}
	// Still exactly one of everything: the retry must not append a second start.
	h.assertFullyStarted(t, "wf-after-commit")
	h.assertNoOrphansInShard(t)
}

// ---- 4. client timeout while PostgreSQL keeps going -----------------------

// This is how the audit produced 151 orphans from 553 starts: give the client a
// few milliseconds and let the database carry on. Each start lands somewhere
// different in the transaction, which is exactly what makes it a good probe —
// the invariant has to hold at every one of those points, not at a chosen one.
func TestStartUnderShortClientTimeoutsNeverOrphans(t *testing.T) {
	h := newStartHarness(t)

	const starts = 120
	var succeeded, failed int
	for i := 0; i < starts; i++ {
		wf := fmt.Sprintf("wf-timeout-%03d", i)
		// Sweep the window rather than guess it: 0 means "already expired",
		// which exercises the earliest possible abort.
		budget := time.Duration(i%30) * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		_, err := h.engine.StartWorkflow(ctx, h.req(wf))
		cancel()

		if err == nil {
			succeeded++
			h.assertFullyStarted(t, wf)
			continue
		}
		failed++
		// A start that did not report success may still have landed: the
		// context can expire after the server committed. Either outcome is
		// correct; a row without history is not.
		got := h.shape(t, wf)
		if got.rows == 1 && (got.events != 1 || got.tasks != 1 || got.lastSeq != 1) {
			t.Fatalf("%s: orphan after a client timeout: %+v", wf, got)
		}
	}
	h.assertNoOrphansInShard(t)
	t.Logf("%d starts under 0-29ms client timeouts: %d reported success, %d reported an error, 0 orphans",
		starts, succeeded, failed)
}

// ---- 5. database stall, then recovery -------------------------------------

// A real stall of the write path: another session holds an ACCESS EXCLUSIVE
// lock on executions, so the start's INSERT blocks on the lock rather than on
// anything the engine controls. The client gives up, the lock is released, and
// the invariant must still hold. Under the old start path the audit reproduced
// 17 orphans from 300 starts during a 20-second stall, with every client told
// success.
func TestStartDuringADatabaseStallNeverOrphans(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()

	// HOW THE STALL IS PRODUCED, and why not with a table lock.
	//
	// The obvious way is LOCK TABLE executions IN ACCESS EXCLUSIVE MODE, and
	// that is what this test did first. It is the wrong tool in a suite whose
	// packages share one database: an ACCESS EXCLUSIVE lock blocks every other
	// package's reads of executions AND blocks testdb.Migrate's
	// CREATE INDEX ... ON executions, which runs while holding the migration
	// advisory lock that a third package may then be waiting on with no
	// deadline. That is a cross-package stall with no bound on it, in a test
	// suite, which is indefensible however unlikely — and this package was
	// seen to hang once through a ten-minute timeout.
	//
	// So the stall is scoped to the exact rows under test instead: an open
	// transaction inserts each business key first, and the engine's own INSERT
	// then blocks on the primary key until that transaction resolves. It is a
	// genuine stall of the start path's write — the same wait, on the same
	// statement — and it is invisible to every other row, table and package.
	blocker, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire blocker: %v", err)
	}
	defer blocker.Release()
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}

	const starts = 12
	for i := 0; i < starts; i++ {
		if _, err := tx.Exec(ctx,
			`INSERT INTO executions (shard_id, workflow_id, run_id, workflow_type, status, last_seq)
			 VALUES ($1, $2, gen_random_uuid(), 'Blocker', 'RUNNING', 0)`,
			int32(h.shard), fmt.Sprintf("wf-stall-%02d", i)); err != nil {
			t.Fatalf("stage the blocking insert: %v", err)
		}
	}

	// Every start now blocks on the primary key and gives up when its own
	// deadline passes, which is exactly a client timing out against a database
	// that has stopped answering.
	for i := 0; i < starts; i++ {
		wf := fmt.Sprintf("wf-stall-%02d", i)
		sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		_, serr := h.engine.StartWorkflow(sctx, h.req(wf))
		cancel()
		if serr == nil {
			t.Fatalf("%s: start succeeded while its key was held by an open transaction", wf)
		}
	}

	// Recovery. The blocking transaction rolls back, so its rows never
	// existed and the keys are free.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release the stall: %v", err)
	}

	// Nothing was left behind by the stalled starts, and the same keys now
	// start cleanly rather than reporting "already open".
	for i := 0; i < starts; i++ {
		wf := fmt.Sprintf("wf-stall-%02d", i)
		h.assertNothing(t, wf)
		res, serr := h.engine.StartWorkflow(ctx, h.req(wf))
		if serr != nil {
			t.Fatalf("%s: start after recovery: %v", wf, serr)
		}
		if res.AlreadyOpen {
			t.Fatalf("%s: reported AlreadyOpen after a stall in which nothing landed", wf)
		}
		h.assertFullyStarted(t, wf)
	}
	h.assertNoOrphansInShard(t)
	t.Logf("%d starts stalled on a contended primary key, 0 orphans, all %d started cleanly after recovery",
		starts, starts)
}

// ---- 6. connection loss during the commit ---------------------------------

// The server drops the connection out from under an open transaction. Postgres
// aborts it, so nothing is durable — but the client sees an error whose outcome
// it cannot classify, which is the case the old start path could not survive.
func TestStartLosingTheConnectionMidCommitNeverOrphans(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()

	// A pool of this test's own, tagged so only its backends are terminated.
	// All reading is still done through h.pool, which is never disturbed, so a
	// killed connection cannot be mistaken for a failed assertion.
	cfg, err := pgxpool.ParseConfig(testdb.DSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	appName := "durexec-killme-" + uuid.NewString()[:8]
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	cfg.MaxConns = 4
	victim, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("victim pool: %v", err)
	}
	defer victim.Close()

	victimStore := eventlog.New(victim)
	engine := &Engine{
		Events:     victimStore,
		Executions: victimStore,
		Committer:  eventlog.NewFencingCounter(victimStore, h.fencing),
		Leases:     &fakeLeases{lease: core.Lease{ShardID: h.shard, Owner: "someone-else"}},
		ShardCount: 16, NodeID: "node-test", Clock: fixedClock{testNow},
		TokenFor: func(core.ShardID) (core.FencingToken, bool) { return h.token, true },
	}

	stop := make(chan struct{})
	var killer sync.WaitGroup
	killer.Add(1)
	go func() {
		defer killer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Bounded, and on a context of its own: this goroutine must not
			// be the thing that wedges the package if the pool is busy.
			kctx, kcancel := context.WithTimeout(ctx, 5*time.Second)
			_, _ = h.pool.Exec(kctx,
				`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`,
				appName)
			kcancel()
			// Paced so that some starts complete and some are cut off
			// mid-transaction. Killing continuously makes every start fail
			// before it reaches the commit, which proves nothing: the
			// interesting case is the one that was interrupted after issuing
			// its writes.
			time.Sleep(15 * time.Millisecond)
		}
	}()

	const starts = 60
	var ok, broke int
	for i := 0; i < starts; i++ {
		wf := fmt.Sprintf("wf-conn-%03d", i)
		if _, err := engine.StartWorkflow(ctx, h.req(wf)); err != nil {
			broke++
		} else {
			ok++
		}
	}
	close(stop)
	killer.Wait()

	// One probe over every key, through the healthy pool.
	for i := 0; i < starts; i++ {
		wf := fmt.Sprintf("wf-conn-%03d", i)
		got := h.shape(t, wf)
		if got.rows == 1 && (got.events != 1 || got.tasks != 1 || got.lastSeq != 1) {
			t.Fatalf("%s: orphan after a connection loss: %+v", wf, got)
		}
	}
	h.assertNoOrphansInShard(t)
	// Both numbers have to be non-zero for this run to be evidence. All-failed
	// means the killer closed connections before any start reached its commit;
	// all-succeeded means it never won a race. Either way the interesting
	// boundary was not crossed, and saying so is better than a green tick.
	if broke == 0 || ok == 0 {
		t.Skipf("not evidence: %d starts errored and %d succeeded, so no start was cut off after issuing its writes", broke, ok)
	}
	t.Logf("%d starts against a pool whose backends were being terminated: %d errored, %d succeeded, 0 orphans",
		starts, broke, ok)
}

// ---- 7. concurrent starts with the same workflow id -----------------------

// The executions primary key is the engine's front-door idempotency, and it now
// runs inside the decision transaction. Exactly one start may win, and every
// loser must write nothing: no second row, no second start event, no second
// task. A second task would run the workflow twice.
func TestConcurrentStartsOfOneWorkflowIDProduceOneRun(t *testing.T) {
	h := newStartHarness(t)
	const racers = 16

	type outcome struct {
		res api.StartResult
		err error
	}
	results := make([]outcome, racers)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			r, err := h.engine.StartWorkflow(context.Background(), h.req("wf-race"))
			results[i] = outcome{res: r, err: err}
		}(i)
	}
	close(gate)
	wg.Wait()

	var winners, alreadyOpen, errs int
	runIDs := map[core.RunID]bool{}
	for i, o := range results {
		switch {
		case o.err != nil:
			errs++
			t.Logf("racer %d errored: %v", i, o.err)
		case o.res.AlreadyOpen:
			alreadyOpen++
			runIDs[o.res.RunID] = true
		default:
			winners++
			runIDs[o.res.RunID] = true
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers reported a fresh start, want exactly 1 (already_open=%d errors=%d)",
			winners, alreadyOpen, errs)
	}
	if len(runIDs) != 1 {
		t.Fatalf("racers saw %d distinct run ids, want 1: %v", len(runIDs), runIDs)
	}
	h.assertFullyStarted(t, "wf-race")
	h.assertNoOrphansInShard(t)
}

// ---- 8. a stale shard owner attempting to create a workflow ---------------

// Fix 4's obligation. The start path had NO fencing check before this change,
// because the executions insert ran on the pool. Now it is refused by the same
// verifyToken ... FOR UPDATE that guards every other write, and the rejection
// is counted.
func TestStaleOwnerCannotStartAWorkflow(t *testing.T) {
	h := newStartHarness(t)

	// The shard moves on while this node still believes it holds token 7.
	h.setLease(t, 9)

	_, err := h.engine.StartWorkflow(context.Background(), h.req("wf-stale-owner"))
	if !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("stale owner start: got %v, want core.ErrLostOwnership", err)
	}
	h.assertNothing(t, "wf-stale-owner")
	if h.fencing.count() != 1 {
		t.Fatalf("fencing rejections counted = %d, want 1; fencing_rejections_total must fire for a refused start",
			h.fencing.count())
	}

	// Once this node is told the new token, the same start works, so the
	// refusal was about the token and nothing else.
	h.token = 9
	if _, err := h.engine.StartWorkflow(context.Background(), h.req("wf-stale-owner")); err != nil {
		t.Fatalf("start with the current token: %v", err)
	}
	h.assertFullyStarted(t, "wf-stale-owner")
	if h.fencing.count() != 1 {
		t.Fatalf("fencing rejections counted = %d after a legitimate start, want it unchanged at 1",
			h.fencing.count())
	}
	h.assertNoOrphansInShard(t)
}

// A shard with no lease row at all must be refused too, and must NOT be counted
// as a fencing rejection: a missing lease row is an operational fault, and
// conflating the two would put it into the one number that is meant to be
// evidence that split brain was prevented.
func TestStartOnAShardWithNoLeaseRowIsRefusedAndNotCountedAsFencing(t *testing.T) {
	h := newStartHarness(t)
	if _, err := h.pool.Exec(context.Background(),
		"DELETE FROM shard_leases WHERE shard_id = $1", int32(h.shard)); err != nil {
		t.Fatalf("drop lease row: %v", err)
	}

	_, err := h.engine.StartWorkflow(context.Background(), h.req("wf-no-lease"))
	if !errors.Is(err, core.ErrLostOwnership) {
		t.Fatalf("start with no lease row: got %v, want core.ErrLostOwnership", err)
	}
	h.assertNothing(t, "wf-no-lease")
	if h.fencing.count() != 0 {
		t.Fatalf("fencing rejections counted = %d, want 0 for a missing lease row", h.fencing.count())
	}
}

// ---- 9. retry after an ambiguous commit -----------------------------------

// ambiguousCommitter models the one outcome a client cannot resolve on its own:
// the transaction committed and the acknowledgement was lost. It calls through
// — so the write really does land — and then reports a connection error.
type ambiguousCommitter struct {
	inner core.Committer

	mu        sync.Mutex
	ambiguous int // how many more commits to report as ambiguous
	landed    bool
}

var errAmbiguous = errors.New("write tcp 127.0.0.1:5432: connection reset by peer")

func (c *ambiguousCommitter) CommitDecision(ctx context.Context, d core.Decision) error {
	c.mu.Lock()
	report := c.ambiguous > 0
	if report {
		c.ambiguous--
	}
	c.mu.Unlock()

	err := c.inner.CommitDecision(ctx, d)
	if err == nil {
		c.mu.Lock()
		c.landed = true
		c.mu.Unlock()
	}
	if report {
		// The caller cannot tell this from a commit that never happened.
		return fmt.Errorf("eventlog: commit decision: %w", errAmbiguous)
	}
	return err
}

// Case (a): the commit DID land. StartWorkflow must report the error rather
// than success — it genuinely does not know — and the client's retry must find
// the run that landed, not start a second one.
func TestRetryAfterAnAmbiguousCommitThatLandedFindsTheSameRun(t *testing.T) {
	h := newStartHarness(t)
	amb := &ambiguousCommitter{inner: h.store, ambiguous: 1}
	h.engine.Committer = amb

	_, err := h.engine.StartWorkflow(context.Background(), h.req("wf-ambiguous"))
	if !errors.Is(err, errAmbiguous) {
		t.Fatalf("ambiguous commit: got %v, want it reported as an error and not as success", err)
	}
	if !amb.landed {
		t.Fatal("the test did not arrange for the commit to land")
	}
	landed := h.assertFullyStarted(t, "wf-ambiguous")

	retry, err := h.engine.StartWorkflow(context.Background(), h.req("wf-ambiguous"))
	if err != nil {
		t.Fatalf("retry after an ambiguous commit: %v", err)
	}
	if !retry.AlreadyOpen {
		t.Fatal("retry did not report AlreadyOpen for a commit that had landed")
	}
	if string(retry.RunID) != landed.runID {
		t.Fatalf("retry reported run %s, but %s is the one that landed", retry.RunID, landed.runID)
	}
	// Exactly one start event and one task, so the workflow runs once.
	h.assertFullyStarted(t, "wf-ambiguous")
	h.assertNoOrphansInShard(t)
}

// Case (b): the commit did NOT land. The retry must produce a real run, and
// must not be answered "already open" on the strength of a row that was never
// written — which is what the old path did.
func TestRetryAfterAnAmbiguousCommitThatDidNotLandStartsTheRun(t *testing.T) {
	h := newStartHarness(t)
	failing := &fakeCommitter{}
	failing.failTimes(1, fmt.Errorf("eventlog: commit decision: %w", errAmbiguous))
	// Fail once through a fake, then fall through to the real store.
	h.engine.Committer = &chainCommitter{first: failing, then: h.store}

	if _, err := h.engine.StartWorkflow(context.Background(), h.req("wf-amb-lost")); !errors.Is(err, errAmbiguous) {
		t.Fatalf("first attempt: got %v, want the ambiguous error", err)
	}
	h.assertNothing(t, "wf-amb-lost")

	res, err := h.engine.StartWorkflow(context.Background(), h.req("wf-amb-lost"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.AlreadyOpen {
		t.Fatal("retry reported AlreadyOpen although nothing had been written")
	}
	h.assertFullyStarted(t, "wf-amb-lost")
	h.assertNoOrphansInShard(t)
}

// chainCommitter sends the first n commits to one committer and the rest to
// another, so a test can fail a commit without the write landing and then let
// the retry reach the real database.
type chainCommitter struct {
	mu    sync.Mutex
	used  bool
	first core.Committer
	then  core.Committer
}

func (c *chainCommitter) CommitDecision(ctx context.Context, d core.Decision) error {
	c.mu.Lock()
	first := !c.used
	c.used = true
	c.mu.Unlock()
	if first {
		return c.first.CommitDecision(ctx, d)
	}
	return c.then.CommitDecision(ctx, d)
}

// ---- the legacy orphans ---------------------------------------------------

// The 19 rows already on the demonstration cluster cannot be un-written, and a
// retry of one of those business keys must not be answered "already open,
// RUNNING" — that answer is what made them invisible for two days.
func TestARetryIsNeverToldSuccessForAPreExistingOrphan(t *testing.T) {
	h := newStartHarness(t)
	ctx := context.Background()

	// Exactly the shape the audit found: RUNNING, last_seq 0, no events, no
	// task, no timer. Written with raw SQL because the engine can no longer
	// produce it.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO executions (shard_id, workflow_id, run_id, workflow_type, status, last_seq)
		 VALUES ($1, 'wf-legacy-orphan', $2::uuid, 'DeliveryOrder', 'RUNNING', 0)`,
		int32(h.shard), uuid.NewString()); err != nil {
		t.Fatalf("seed a legacy orphan: %v", err)
	}

	res, err := h.engine.StartWorkflow(ctx, h.req("wf-legacy-orphan"))
	if !errors.Is(err, core.ErrWorkflowOrphaned) {
		t.Fatalf("start over a legacy orphan: got (%+v, %v), want core.ErrWorkflowOrphaned", res, err)
	}
	if res.AlreadyOpen {
		t.Fatal("an orphan was reported to the client as already open")
	}
}
