package taskq

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// firstTestShard is the base of the shard ids this test writes. They are far
// outside any real SHARD_COUNT so they cannot collide with live data, and they
// are named so the cleanup below and the loop cannot drift apart.
const firstTestShard = 900

// TestConcurrentWarmPollersEachGetATask is the empirical check for the
// EvalPlanQual defect. Four connections are warmed first, then released
// simultaneously against four available tasks.
func TestConcurrentWarmPollersEachGetATask(t *testing.T) {
	// Through testPool, so this goes via internal/testdb: it applies the
	// migrations itself and refuses a DSN pointing at the demonstration
	// cluster. Reading DUREXEC_TEST_DSN directly here bypassed that guard,
	// and the shard ids below were left behind in the live demo database.
	ctx := context.Background()
	pool := testPool(t)

	const workers = 4
	const rounds = 12

	// Clean up every shard this test writes, however it ends. The rounds below
	// delete their own shard on the way IN, which leaves the last round's rows
	// behind on success and every round's rows behind on a failure.
	t.Cleanup(func() {
		for r := 0; r < rounds; r++ {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM tasks WHERE shard_id = $1`, int32(firstTestShard+r))
		}
	})
	q := New(pool, DefaultConfig())
	won := 0
	total := 0

	for r := 0; r < rounds; r++ {
		shard := core.ShardID(firstTestShard + r)
		if _, err := pool.Exec(ctx, `DELETE FROM tasks WHERE shard_id=$1`, int32(shard)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < workers; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO tasks (shard_id, task_type, workflow_id, run_id, visible_at)
				 VALUES ($1,'WORKFLOW','wf','00000000-0000-0000-0000-000000000001', now())`,
				int32(shard)); err != nil {
				t.Fatal(err)
			}
		}
		// Warm every connection so none is staggered by connection setup.
		conns := make([]*pgxpool.Conn, workers)
		for i := range conns {
			c, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = c.Exec(ctx, "SELECT 1")
			conns[i] = c
		}
		for _, c := range conns {
			c.Release()
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		gotThisRound := 0
		start := make(chan struct{})
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := q.Poll(ctx, shard, core.TaskWorkflow); err == nil {
					mu.Lock()
					gotThisRound++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		won += gotThisRound
		total += workers
		t.Logf("round %2d: %d of %d pollers got a task", r+1, gotThisRound, workers)
	}
	t.Logf("TOTAL: %d of %d polls succeeded (%.0f%%)", won, total, float64(won)/float64(total)*100)
	if won < total {
		t.Errorf("starvation: only %d of %d warm pollers got work", won, total)
	}
}
