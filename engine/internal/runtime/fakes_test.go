package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// The fakes below let every test in this package run with no database and no
// model server. That is not only for speed: the behaviour under test here is
// decision-making, and a test that also had to set up PostgreSQL would be
// testing the setup as much as the decision.

// ---- clock ----------------------------------------------------------------

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var testNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// ---- event store ----------------------------------------------------------

type fakeEvents struct {
	hist []core.Event
	err  error
}

func (f *fakeEvents) Load(context.Context, core.ShardID, string, core.RunID) ([]core.Event, error) {
	return f.hist, f.err
}

func (f *fakeEvents) LastSeq(context.Context, core.ShardID, string, core.RunID) (core.Seq, error) {
	if len(f.hist) == 0 {
		return 0, nil
	}
	return f.hist[len(f.hist)-1].Seq, nil
}

// ---- execution store ------------------------------------------------------

type fakeExecutions struct {
	ex  core.Execution
	err error
}

func (f *fakeExecutions) Create(context.Context, core.ShardID, string, core.RunID, string) error {
	return nil
}

func (f *fakeExecutions) Get(context.Context, core.ShardID, string) (core.Execution, error) {
	return f.ex, f.err
}

func (f *fakeExecutions) List(context.Context, core.ShardID, core.ExecutionStatus, int) ([]core.Execution, error) {
	return []core.Execution{f.ex}, nil
}

// ---- committer ------------------------------------------------------------

type fakeCommitter struct {
	mu        sync.Mutex
	decisions []core.Decision
	err       error

	// failures counts down the commits that return failWith before the
	// committer starts accepting them. See failTimes.
	failures int
	failWith error
}

func (f *fakeCommitter) CommitDecision(_ context.Context, d core.Decision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.failures > 0 {
		f.failures--
		return f.failWith
	}
	f.decisions = append(f.decisions, d)
	return nil
}

// failTimes makes the next n commits return err and every later one succeed.
// A sequence race is transient by nature: the loser recomputes from the log and
// the retry lands, so a fake that fails forever cannot show the difference
// between "kept going" and "gave up".
func (f *fakeCommitter) failTimes(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = n
	f.failWith = err
}

func (f *fakeCommitter) commitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.decisions)
}

func (f *fakeCommitter) last() (core.Decision, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) == 0 {
		return core.Decision{}, false
	}
	return f.decisions[len(f.decisions)-1], true
}

// ---- task queue -----------------------------------------------------------

type releaseRecord struct {
	taskID    int64
	visibleIn int64
}

type fakeQueue struct {
	mu       sync.Mutex
	tasks    []core.Task
	released []releaseRecord
	enqueued []core.Task
	pollErr  error

	// extends counts claim renewals, and extendErr is what the nth renewal
	// returns. They exist because the activity worker now has to renew the
	// claim on a task it is still working, and "did it renew, and what did it
	// do when told the claim was gone" is the whole behaviour under test.
	extends   int
	extendErr error

	// requeueOnRelease puts a released task back at the end of the queue, so a
	// test can drive the worker LOOP rather than one process call and watch it
	// re-poll work it handed back.
	requeueOnRelease bool
}

func (q *fakeQueue) Poll(_ context.Context, _ core.ShardID, t core.TaskType) (core.Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pollErr != nil {
		return core.Task{}, q.pollErr
	}
	for i, task := range q.tasks {
		if task.Type != t {
			continue
		}
		q.tasks = append(q.tasks[:i], q.tasks[i+1:]...)
		return task, nil
	}
	return core.Task{}, core.ErrNoTask
}

func (q *fakeQueue) Extend(_ context.Context, _ core.ShardID, _ int64, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.extends++
	return q.extendErr
}

func (q *fakeQueue) Release(_ context.Context, _ core.ShardID, taskID int64, tok string, visibleIn int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.released = append(q.released, releaseRecord{taskID: taskID, visibleIn: visibleIn})
	if q.requeueOnRelease {
		q.tasks = append(q.tasks, core.Task{
			ShardID: 1, TaskID: taskID, Type: core.TaskWorkflow,
			WorkflowID: "wf-1", RunID: "run-1", ScheduledEventSeq: 1,
			LockToken: tok, Attempt: 1,
		})
	}
	return nil
}

func (q *fakeQueue) Enqueue(_ context.Context, tasks []core.Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueued = append(q.enqueued, tasks...)
	return nil
}

func (q *fakeQueue) releaseCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.released)
}

func (q *fakeQueue) extendCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.extends
}

// ---- lease store ----------------------------------------------------------

type fakeLeases struct{ lease core.Lease }

func (f *fakeLeases) Acquire(context.Context, core.ShardID, string, int) (core.FencingToken, error) {
	return f.lease.Token, nil
}
func (f *fakeLeases) Renew(context.Context, core.ShardID, string, core.FencingToken, int) error {
	return nil
}
func (f *fakeLeases) Release(context.Context, core.ShardID, string, core.FencingToken) error {
	return nil
}
func (f *fakeLeases) Get(context.Context, core.ShardID) (core.Lease, error) { return f.lease, nil }
func (f *fakeLeases) List(context.Context) ([]core.Lease, error)            { return []core.Lease{f.lease}, nil }

// ---- intent store ---------------------------------------------------------

type fakeIntents struct {
	rows []core.Intent
	err  error
}

func (f *fakeIntents) Put(_ context.Context, in core.Intent) error {
	f.rows = append(f.rows, in)
	return nil
}

func (f *fakeIntents) Candidates(_ context.Context, _ core.ShardID, _ string, _ core.RunID, activityType string) ([]core.Intent, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []core.Intent
	for _, r := range f.rows {
		if r.ActivityType == activityType {
			out = append(out, r)
		}
	}
	return out, nil
}

// ---- embedder -------------------------------------------------------------

// scriptedEmbedder returns a vector chosen per text, so a test can pin the
// exact cosine similarity a check will see.
//
// Two-dimensional unit vectors are enough. A candidate at [1,0] and a query at
// [s, sqrt(1-s^2)] have a cosine of exactly s, which means a test can say "put
// this call at 0.87 similarity" and land precisely inside the escalation band
// instead of hoping a hash falls there.
type scriptedEmbedder struct {
	sims map[string]float64
	err  error
}

func unitAt(sim float64) []float32 {
	if sim > 1 {
		sim = 1
	}
	return []float32{float32(sim), float32(math.Sqrt(math.Max(0, 1-sim*sim)))}
}

func (e *scriptedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	if sim, ok := e.sims[text]; ok {
		return unitAt(sim), nil
	}
	// Anything unscripted is orthogonal to the stored candidate, which reads
	// as "nothing like it", i.e. DISTINCT.
	return []float32{0, 1}, nil
}

func (e *scriptedEmbedder) Model() string { return "scripted" }
func (e *scriptedEmbedder) Dim() int      { return 2 }

// candidateVector is the vector every stored candidate gets, so the cosine
// with a scripted query is exactly the scripted number.
func candidateVector() []float32 { return []float32{1, 0} }

// ---- helpers --------------------------------------------------------------

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func event(seq core.Seq, t core.EventType, payload any) core.Event {
	return core.Event{
		ShardID: 1, WorkflowID: "wf-1", RunID: "run-1",
		Seq: seq, Type: t, Payload: mustJSON(payload), CreatedAt: testNow,
	}
}

var errBoom = errors.New("boom")
