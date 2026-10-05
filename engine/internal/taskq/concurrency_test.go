package taskq

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConcurrentWarmPollersEachGetATask is the empirical check for the
// EvalPlanQual defect. Four connections are warmed first, then released
// simultaneously against four available tasks.
func TestConcurrentWarmPollersEachGetATask(t *testing.T) {
	dsn := os.Getenv("DUREXEC_TEST_DSN")
	if dsn == "" {
		t.Skip("no DUREXEC_TEST_DSN")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	const workers = 4
	const rounds = 12
	q := New(pool, DefaultConfig())
	won := 0
	total := 0

	for r := 0; r < rounds; r++ {
		shard := core.ShardID(900 + r)
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
