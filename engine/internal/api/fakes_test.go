package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Every dependency below is a hand-written fake in a test file. There is no
// database and no network anywhere in this package's tests: the API's job is
// routing, shape and error mapping, all of which are decidable from fakes.

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// fixedShard sends every workflow to one shard, so a test can control routing
// without reproducing the hash.
type fixedShard core.ShardID

func (s fixedShard) ShardFor(string) core.ShardID { return core.ShardID(s) }

type fakeLeases struct {
	lease core.Lease
	list  []core.Lease
	err   error
}

func (f *fakeLeases) Acquire(context.Context, core.ShardID, string, int) (core.FencingToken, error) {
	return 0, nil
}
func (f *fakeLeases) Renew(context.Context, core.ShardID, string, core.FencingToken, int) error {
	return nil
}
func (f *fakeLeases) Release(context.Context, core.ShardID, string, core.FencingToken) error {
	return nil
}
func (f *fakeLeases) Get(_ context.Context, shard core.ShardID) (core.Lease, error) {
	if f.err != nil {
		return core.Lease{}, f.err
	}
	l := f.lease
	l.ShardID = shard
	return l, nil
}
func (f *fakeLeases) List(context.Context) ([]core.Lease, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.list, nil
}

type fakeExecutions struct {
	get     core.Execution
	getErr  error
	list    []core.Execution
	listErr error
	created []core.Execution
}

func (f *fakeExecutions) Create(_ context.Context, shard core.ShardID, id string, run core.RunID, wfType string) error {
	f.created = append(f.created, core.Execution{ShardID: shard, WorkflowID: id, RunID: run, WorkflowType: wfType})
	return nil
}
func (f *fakeExecutions) Get(context.Context, core.ShardID, string) (core.Execution, error) {
	if f.getErr != nil {
		return core.Execution{}, f.getErr
	}
	return f.get, nil
}
func (f *fakeExecutions) List(_ context.Context, _ core.ShardID, status core.ExecutionStatus, limit int) ([]core.Execution, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []core.Execution
	for _, e := range f.list {
		if status != "" && e.Status != status {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, e)
	}
	return out, nil
}

type fakeEvents struct {
	events []core.Event
	err    error
}

func (f *fakeEvents) Load(context.Context, core.ShardID, string, core.RunID) ([]core.Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}
func (f *fakeEvents) LastSeq(context.Context, core.ShardID, string, core.RunID) (core.Seq, error) {
	if f.err != nil {
		return 0, f.err
	}
	return core.Seq(len(f.events)), nil
}

type fakeStarter struct {
	result StartResult
	err    error
	seen   StartRequest
}

func (f *fakeStarter) StartWorkflow(_ context.Context, req StartRequest) (StartResult, error) {
	f.seen = req
	return f.result, f.err
}

type fakeSignaller struct {
	err  error
	seen SignalRequest
}

func (f *fakeSignaller) Signal(_ context.Context, req SignalRequest) error {
	f.seen = req
	return f.err
}

type fakeCanceller struct {
	err  error
	seen CancelRequest
}

func (f *fakeCanceller) Cancel(_ context.Context, req CancelRequest) error {
	f.seen = req
	return f.err
}

type fakeFaults struct {
	err  error
	seen FailStepRequest
}

func (f *fakeFaults) FailStep(_ context.Context, req FailStepRequest) error {
	f.seen = req
	return f.err
}

// dedupEvent builds a journaled semantic verdict, the shape the verdicts
// endpoint reads back out of history.
func dedupEvent(seq core.Seq, k int, verdict core.DedupVerdict, sim float64) core.Event {
	payload, _ := json.Marshal(core.DedupEvaluated{
		K:          k,
		Verdict:    verdict,
		MatchedK:   2,
		Similarity: sim,
		TauHigh:    0.90,
		TauLow:     0.85,
		Model:      "all-MiniLM-L6-v2",
		IntentText: `search_flights({"q":"BLR to DEL next Friday"})`,
	})
	return core.Event{
		ShardID:    3,
		WorkflowID: "trip-42",
		RunID:      "run-1",
		Seq:        seq,
		Type:       core.EvDedupEvaluated,
		Payload:    payload,
		CreatedAt:  routingNow,
	}
}
