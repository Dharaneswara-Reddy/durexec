package timers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---- fakes ----------------------------------------------------------------

// fakeClock is a Clock that only moves when a test says so.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeStore is an in-memory core.TimerStore. It records the order of calls so a
// test can assert that enqueue happens before delete.
type fakeStore struct {
	mu      sync.Mutex
	due     map[core.ShardID][]core.Timer
	deleted []core.Timer
	dueErr  error
	delErr  error
	calls   []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{due: map[core.ShardID][]core.Timer{}}
}

func (f *fakeStore) Due(_ context.Context, shard core.ShardID, limit int) ([]core.Timer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "due")
	if f.dueErr != nil {
		return nil, f.dueErr
	}
	all := f.due[shard]
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	out := make([]core.Timer, len(all))
	copy(out, all)
	return out, nil
}

func (f *fakeStore) Delete(_ context.Context, t core.Timer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "delete")
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, t)
	remaining := f.due[t.ShardID][:0]
	for _, existing := range f.due[t.ShardID] {
		if existing.TimerSeq != t.TimerSeq || existing.WorkflowID != t.WorkflowID {
			remaining = append(remaining, existing)
		}
	}
	f.due[t.ShardID] = remaining
	return nil
}

// fakeQueue is an in-memory core.TaskQueue. Only Enqueue is exercised by the
// scanner; the rest satisfy the interface.
type fakeQueue struct {
	mu       sync.Mutex
	enqueued []core.Task
	err      error
	calls    *[]string
}

func (q *fakeQueue) Enqueue(_ context.Context, tasks []core.Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.calls != nil {
		*q.calls = append(*q.calls, "enqueue")
	}
	if q.err != nil {
		return q.err
	}
	q.enqueued = append(q.enqueued, tasks...)
	return nil
}

func (q *fakeQueue) Poll(context.Context, core.ShardID, core.TaskType) (core.Task, error) {
	return core.Task{}, core.ErrNoTask
}
func (q *fakeQueue) Extend(context.Context, core.ShardID, int64, string) error { return nil }
func (q *fakeQueue) Release(context.Context, core.ShardID, int64, string, int64) error {
	return nil
}

var (
	_ core.TimerStore = (*fakeStore)(nil)
	_ core.TaskQueue  = (*fakeQueue)(nil)
)

func timer(shard core.ShardID, wf string, seq core.Seq, fireAt time.Time) core.Timer {
	return core.Timer{ShardID: shard, WorkflowID: wf, RunID: "11111111-1111-1111-1111-111111111111", TimerSeq: seq, FireAt: fireAt}
}

// ---- pure mapping ---------------------------------------------------------

func TestTasksForTimers(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   []core.Timer
		want int
	}{
		{"nothing due produces no tasks", nil, 0},
		{"one timer, one task", []core.Timer{timer(1, "a", 5, now)}, 1},
		{"three timers, three tasks", []core.Timer{
			timer(1, "a", 5, now), timer(1, "b", 6, now), timer(1, "c", 7, now),
		}, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TasksForTimers(tc.in, now)
			if len(got) != tc.want {
				t.Fatalf("got %d tasks, want %d", len(got), tc.want)
			}
			for i, task := range got {
				src := tc.in[i]
				if task.Type != core.TaskWorkflow {
					t.Errorf("task %d type = %q, want WORKFLOW: a timer always wakes the workflow, never an activity", i, task.Type)
				}
				if task.ShardID != src.ShardID || task.WorkflowID != src.WorkflowID || task.RunID != src.RunID {
					t.Errorf("task %d does not identify its timer's run: %+v", i, task)
				}
				if task.ScheduledEventSeq != src.TimerSeq {
					t.Errorf("task %d seq = %d, want the timer's seq %d so replay can match it",
						i, task.ScheduledEventSeq, src.TimerSeq)
				}
				if !task.VisibleAt.Equal(now) {
					t.Errorf("task %d visible at %v, want %v", i, task.VisibleAt, now)
				}
			}
		})
	}
}

func TestClampLimit(t *testing.T) {
	tests := []struct{ in, want int }{
		{-1, defaultDueLimit},
		{0, defaultDueLimit},
		{1, 1},
		{500, 500},
	}
	for _, tc := range tests {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestScannerConfigDefaults(t *testing.T) {
	got := ScannerConfig{}.withDefaults()
	if got.Interval != time.Second {
		t.Errorf("Interval = %v, want 1s", got.Interval)
	}
	if got.Batch != defaultDueLimit {
		t.Errorf("Batch = %d, want %d", got.Batch, defaultDueLimit)
	}
	if got.Clock == nil {
		t.Error("Clock is nil; the scanner would panic on its first tick")
	}
	custom := ScannerConfig{Interval: 250 * time.Millisecond, Batch: 5}.withDefaults()
	if custom.Interval != 250*time.Millisecond || custom.Batch != 5 {
		t.Errorf("explicit values were overwritten: %+v", custom)
	}
}

// ---- the tick decision, against the fakes ---------------------------------

func TestScanShardFiresEveryDueTimer(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.due[3] = []core.Timer{
		timer(3, "a", 1, now.Add(-time.Hour)),
		timer(3, "b", 2, now.Add(-time.Minute)),
	}
	queue := &fakeQueue{calls: &store.calls}
	s := NewScanner(store, queue, ScannerConfig{Clock: &fakeClock{now: now}})

	fired, err := s.ScanShard(context.Background(), 3)
	if err != nil {
		t.Fatalf("ScanShard: %v", err)
	}
	if fired != 2 {
		t.Fatalf("fired = %d, want 2", fired)
	}
	if len(queue.enqueued) != 2 {
		t.Fatalf("enqueued %d tasks, want 2", len(queue.enqueued))
	}
	if len(store.deleted) != 2 {
		t.Fatalf("deleted %d timers, want 2", len(store.deleted))
	}
	// Enqueue must precede every delete. The other order risks deleting the only
	// record of a wake-up that was never scheduled, leaving a workflow asleep
	// forever; this order risks only a harmless duplicate task.
	wantOrder := []string{"due", "enqueue", "delete", "delete"}
	if len(store.calls) != len(wantOrder) {
		t.Fatalf("call order = %v, want %v", store.calls, wantOrder)
	}
	for i := range wantOrder {
		if store.calls[i] != wantOrder[i] {
			t.Fatalf("call order = %v, want %v", store.calls, wantOrder)
		}
	}
}

func TestScanShardWithNothingDueTouchesNothing(t *testing.T) {
	store := newFakeStore()
	queue := &fakeQueue{calls: &store.calls}
	s := NewScanner(store, queue, ScannerConfig{Clock: &fakeClock{now: time.Now()}})

	fired, err := s.ScanShard(context.Background(), 9)
	if err != nil {
		t.Fatalf("ScanShard: %v", err)
	}
	if fired != 0 || len(queue.enqueued) != 0 || len(store.deleted) != 0 {
		t.Fatalf("an empty shard caused work: fired=%d enqueued=%d deleted=%d", fired, len(queue.enqueued), len(store.deleted))
	}
}

// If the enqueue fails, the timer rows must survive so the next pass retries.
func TestScanShardKeepsTimersWhenEnqueueFails(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.due[1] = []core.Timer{timer(1, "a", 1, now)}
	queue := &fakeQueue{err: errors.New("database down"), calls: &store.calls}
	s := NewScanner(store, queue, ScannerConfig{Clock: &fakeClock{now: now}})

	fired, err := s.ScanShard(context.Background(), 1)
	if err == nil {
		t.Fatal("want an error when the enqueue fails")
	}
	if fired != 0 {
		t.Fatalf("fired = %d, want 0", fired)
	}
	if len(store.deleted) != 0 {
		t.Fatal("a timer was deleted even though its wake-up was never enqueued")
	}
}

func TestScanShardReportsADueQueryFailure(t *testing.T) {
	store := newFakeStore()
	store.dueErr = errors.New("connection reset")
	s := NewScanner(store, &fakeQueue{}, ScannerConfig{Clock: &fakeClock{now: time.Now()}})

	if _, err := s.ScanShard(context.Background(), 1); !errors.Is(err, store.dueErr) {
		t.Fatalf("got %v, want the underlying due error wrapped", err)
	}
}

// A Tick asks for the owned shards fresh, so a shard lost a moment ago is not
// scanned by a stale list.
func TestTickScansEveryOwnedShardAndNoOthers(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.due[1] = []core.Timer{timer(1, "a", 1, now)}
	store.due[2] = []core.Timer{timer(2, "b", 1, now)}
	store.due[3] = []core.Timer{timer(3, "c", 1, now)} // owned by another node
	queue := &fakeQueue{}

	owned := []core.ShardID{1, 2}
	s := NewScanner(store, queue, ScannerConfig{
		Clock:  &fakeClock{now: now},
		Shards: func() []core.ShardID { return owned },
	})

	s.Tick(context.Background())
	if len(queue.enqueued) != 2 {
		t.Fatalf("enqueued %d tasks, want 2 (shards 1 and 2 only)", len(queue.enqueued))
	}
	if len(store.due[3]) != 1 {
		t.Fatal("a shard this node does not own was scanned")
	}

	// Ownership changes; the next tick must follow it without the scanner being
	// rebuilt.
	owned = []core.ShardID{3}
	s.Tick(context.Background())
	if len(queue.enqueued) != 3 {
		t.Fatalf("enqueued %d tasks after the handover, want 3", len(queue.enqueued))
	}
	if len(store.due[3]) != 0 {
		t.Fatal("the newly owned shard was not scanned")
	}
}

func TestTickWithNoShardsFuncDoesNothing(t *testing.T) {
	store := newFakeStore()
	store.due[1] = []core.Timer{timer(1, "a", 1, time.Now())}
	queue := &fakeQueue{}
	s := NewScanner(store, queue, ScannerConfig{Clock: &fakeClock{now: time.Now()}})

	s.Tick(context.Background())
	if len(queue.enqueued) != 0 {
		t.Fatal("a scanner with no ownership source scanned something")
	}
}

func TestTickReportsErrorsAndKeepsGoing(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.dueErr = errors.New("nope")
	var seen []core.ShardID
	s := NewScanner(store, &fakeQueue{}, ScannerConfig{
		Clock:   &fakeClock{now: now},
		Shards:  func() []core.ShardID { return []core.ShardID{1, 2, 3} },
		OnError: func(shard core.ShardID, _ error) { seen = append(seen, shard) },
	})

	s.Tick(context.Background())
	if len(seen) != 3 {
		t.Fatalf("reported %d failures, want 3: one failing shard must not stop the others", len(seen))
	}
}

func TestTickStopsOnACancelledContext(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.due[1] = []core.Timer{timer(1, "a", 1, now)}
	queue := &fakeQueue{}
	s := NewScanner(store, queue, ScannerConfig{
		Clock:  &fakeClock{now: now},
		Shards: func() []core.ShardID { return []core.ShardID{1, 2} },
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Tick(ctx)
	if len(queue.enqueued) != 0 {
		t.Fatal("the scanner kept working after its context was cancelled")
	}
}

func TestRunStopsWhenTheContextEnds(t *testing.T) {
	s := NewScanner(newFakeStore(), &fakeQueue{}, ScannerConfig{
		Interval: time.Millisecond,
		Clock:    &fakeClock{now: time.Now()},
		Shards:   func() []core.ShardID { return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}
}
