package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/obs"
)

const selfNode = "node-a"

// ownedDeps is a node that owns shard 3, with every dependency wired to a
// fake. Individual tests override the one field they are exercising.
func ownedDeps() Deps {
	return Deps{
		NodeID:      selfNode,
		Shards:      fixedShard(3),
		OwnedShards: []core.ShardID{3},
		Leases: &fakeLeases{lease: core.Lease{
			Owner: selfNode, Token: 9, ExpiresAt: routingNow.Add(time.Minute),
		}},
		Executions: &fakeExecutions{get: core.Execution{
			ShardID: 3, WorkflowID: "trip-42", RunID: "run-1",
			WorkflowType: "BookingWorkflow", Status: core.StatusRunning, LastSeq: 12,
		}},
		Events:    &fakeEvents{},
		Starter:   &fakeStarter{result: StartResult{RunID: "run-1", Status: core.StatusRunning}},
		Signaller: &fakeSignaller{},
		Canceller: &fakeCanceller{},
		Faults:    &fakeFaults{},
		Clock:     fixedClock{routingNow},
	}
}

func do(t *testing.T, d Deps, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	New(d).ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response body is not the expected JSON: %v\nbody: %s", err, w.Body.String())
	}
	return out
}

// ---- routes ---------------------------------------------------------------

func TestStartWorkflowCreates(t *testing.T) {
	d := ownedDeps()
	starter := d.Starter.(*fakeStarter)

	w := do(t, d, http.MethodPost, "/v1/workflows",
		`{"workflow_id":"trip-42","workflow_type":"BookingWorkflow","input":{"q":"BLR to DEL"}}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201\nbody: %s", w.Code, w.Body)
	}
	got := decode[WorkflowResponse](t, w)
	if got.WorkflowID != "trip-42" || got.RunID != "run-1" || got.ShardID != 3 {
		t.Fatalf("body = %+v", got)
	}
	if got.Status != core.StatusRunning {
		t.Fatalf("Status = %s, want RUNNING", got.Status)
	}
	if starter.seen.ShardID != 3 || starter.seen.WorkflowType != "BookingWorkflow" {
		t.Fatalf("starter received %+v", starter.seen)
	}
}

// TestStartWorkflowPassesIdempotencyKey: every mutating route accepts the
// header and hands it to the engine.
func TestStartWorkflowPassesIdempotencyKey(t *testing.T) {
	d := ownedDeps()
	starter := d.Starter.(*fakeStarter)

	r := httptest.NewRequest(http.MethodPost, "/v1/workflows",
		strings.NewReader(`{"workflow_id":"trip-42","workflow_type":"BookingWorkflow"}`))
	r.Header.Set(HeaderIdempotencyKey, "client-key-1")
	w := httptest.NewRecorder()
	New(d).ServeHTTP(w, r)

	if starter.seen.IdempotencyKey != "client-key-1" {
		t.Fatalf("Idempotency-Key = %q, want client-key-1", starter.seen.IdempotencyKey)
	}
}

// TestDuplicateStartIs200NotConflict is the front door's idempotency
// guarantee: a second start of a running workflow is SUCCESS with the existing
// run, not a 409.
func TestDuplicateStartIs200NotConflict(t *testing.T) {
	d := ownedDeps()
	d.Starter = &fakeStarter{err: core.ErrWorkflowAlreadyOpen}

	w := do(t, d, http.MethodPost, "/v1/workflows",
		`{"workflow_id":"trip-42","workflow_type":"BookingWorkflow"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a duplicate start is success, not a conflict", w.Code)
	}
	got := decode[WorkflowResponse](t, w)
	if !got.AlreadyOpen {
		t.Fatal("already_open should be true")
	}
	if got.RunID != "run-1" {
		t.Fatalf("RunID = %q, want the existing run", got.RunID)
	}
}

// TestDuplicateStartReportedByEngine covers the other spelling: the engine
// itself reports the collision as success rather than as an error.
func TestDuplicateStartReportedByEngine(t *testing.T) {
	d := ownedDeps()
	d.Starter = &fakeStarter{result: StartResult{RunID: "run-1", Status: core.StatusRunning, AlreadyOpen: true}}

	w := do(t, d, http.MethodPost, "/v1/workflows",
		`{"workflow_id":"trip-42","workflow_type":"BookingWorkflow"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !decode[WorkflowResponse](t, w).AlreadyOpen {
		t.Fatal("already_open should be true")
	}
}

func TestStartWorkflowValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing workflow_id", `{"workflow_type":"BookingWorkflow"}`},
		{"missing workflow_type", `{"workflow_id":"trip-42"}`},
		{"malformed json", `{`},
		{"unknown field", `{"workflow_id":"a","workflow_type":"b","typo":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, ownedDeps(), http.MethodPost, "/v1/workflows", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\nbody: %s", w.Code, w.Body)
			}
			if decode[ErrorResponse](t, w).Error != "BadRequest" {
				t.Fatalf("body = %s", w.Body)
			}
		})
	}
}

func TestGetWorkflow(t *testing.T) {
	w := do(t, ownedDeps(), http.MethodGet, "/v1/workflows/trip-42", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decode[WorkflowResponse](t, w)
	if got.WorkflowID != "trip-42" || got.LastSeq != 12 || got.WorkflowType != "BookingWorkflow" {
		t.Fatalf("body = %+v", got)
	}
}

func TestGetWorkflowNotFound(t *testing.T) {
	d := ownedDeps()
	d.Executions = &fakeExecutions{getErr: core.ErrWorkflowNotFound}

	w := do(t, d, http.MethodGet, "/v1/workflows/missing", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if decode[ErrorResponse](t, w).Error != "WorkflowNotFound" {
		t.Fatalf("body = %s", w.Body)
	}
}

func TestGetHistory(t *testing.T) {
	d := ownedDeps()
	d.Events = &fakeEvents{events: []core.Event{
		{ShardID: 3, WorkflowID: "trip-42", RunID: "run-1", Seq: 1, Type: core.EvWorkflowStarted, Payload: json.RawMessage(`{}`)},
		dedupEvent(2, 1, core.VerdictDuplicate, 0.97),
	}}

	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42/history", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decode[HistoryResponse](t, w)
	if got.Count != 2 || len(got.Events) != 2 {
		t.Fatalf("count = %d, events = %d", got.Count, len(got.Events))
	}
	if got.RunID != "run-1" {
		t.Fatalf("RunID = %q, want the run from the executions pointer", got.RunID)
	}
	if got.Events[0].Type != core.EvWorkflowStarted {
		t.Fatalf("events are not in seq order: %+v", got.Events[0])
	}
}

func TestGetHistoryEmptyIsAnArrayNotNull(t *testing.T) {
	w := do(t, ownedDeps(), http.MethodGet, "/v1/workflows/trip-42/history", "")
	if !strings.Contains(w.Body.String(), `"events":[]`) {
		t.Fatalf("an empty history must serialise as [], not null\nbody: %s", w.Body)
	}
}

func TestSignal(t *testing.T) {
	d := ownedDeps()
	sig := d.Signaller.(*fakeSignaller)

	r := httptest.NewRequest(http.MethodPost, "/v1/workflows/trip-42/signals",
		strings.NewReader(`{"name":"approval","payload":{"ok":true}}`))
	r.Header.Set(HeaderIdempotencyKey, "sig-key")
	w := httptest.NewRecorder()
	New(d).ServeHTTP(w, r)

	// 202, not 200: the signal is recorded, but the workflow may be asleep on
	// a timer and has not necessarily reacted yet.
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nbody: %s", w.Code, w.Body)
	}
	if sig.seen.Name != "approval" || sig.seen.WorkflowID != "trip-42" || sig.seen.ShardID != 3 {
		t.Fatalf("signaller received %+v", sig.seen)
	}
	if sig.seen.IdempotencyKey != "sig-key" {
		t.Fatalf("Idempotency-Key = %q", sig.seen.IdempotencyKey)
	}
	if !decode[AcceptedResponse](t, w).Accepted {
		t.Fatalf("body = %s", w.Body)
	}
}

func TestSignalRequiresName(t *testing.T) {
	w := do(t, ownedDeps(), http.MethodPost, "/v1/workflows/trip-42/signals", `{"payload":{}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestCancel(t *testing.T) {
	d := ownedDeps()
	c := d.Canceller.(*fakeCanceller)

	w := do(t, d, http.MethodPost, "/v1/workflows/trip-42/cancel", `{"reason":"customer changed mind"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nbody: %s", w.Code, w.Body)
	}
	if c.seen.Reason != "customer changed mind" || c.seen.WorkflowID != "trip-42" {
		t.Fatalf("canceller received %+v", c.seen)
	}
}

// TestCancelAcceptsEmptyBody: a reason is optional, so no body is not an error.
func TestCancelAcceptsEmptyBody(t *testing.T) {
	w := do(t, ownedDeps(), http.MethodPost, "/v1/workflows/trip-42/cancel", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nbody: %s", w.Code, w.Body)
	}
}

func TestListWorkflows(t *testing.T) {
	d := ownedDeps()
	d.Executions = &fakeExecutions{list: []core.Execution{
		{ShardID: 3, WorkflowID: "a", RunID: "r1", Status: core.StatusRunning},
		{ShardID: 3, WorkflowID: "b", RunID: "r2", Status: core.StatusCompleted},
		{ShardID: 3, WorkflowID: "c", RunID: "r3", Status: core.StatusRunning},
	}}

	t.Run("all", func(t *testing.T) {
		w := do(t, d, http.MethodGet, "/v1/workflows", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := decode[ListWorkflowsResponse](t, w); got.Count != 3 {
			t.Fatalf("count = %d, want 3", got.Count)
		}
	})

	t.Run("filtered by status", func(t *testing.T) {
		w := do(t, d, http.MethodGet, "/v1/workflows?status=RUNNING", "")
		got := decode[ListWorkflowsResponse](t, w)
		if got.Count != 2 {
			t.Fatalf("count = %d, want 2", got.Count)
		}
	})

	t.Run("limit", func(t *testing.T) {
		w := do(t, d, http.MethodGet, "/v1/workflows?limit=1", "")
		if got := decode[ListWorkflowsResponse](t, w); got.Count != 1 {
			t.Fatalf("count = %d, want 1", got.Count)
		}
	})

	t.Run("bad shard param", func(t *testing.T) {
		w := do(t, d, http.MethodGet, "/v1/workflows?shard=abc", "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})
}

// TestDedupVerdicts is the contribution's observability surface: the verdict
// is read straight out of history, so what this endpoint shows is exactly what
// replay will see.
func TestDedupVerdicts(t *testing.T) {
	d := ownedDeps()
	d.Events = &fakeEvents{events: []core.Event{
		{ShardID: 3, WorkflowID: "trip-42", RunID: "run-1", Seq: 1, Type: core.EvWorkflowStarted, Payload: json.RawMessage(`{}`)},
		dedupEvent(2, 1, core.VerdictDuplicate, 0.97),
		{ShardID: 3, WorkflowID: "trip-42", RunID: "run-1", Seq: 3, Type: core.EvActivityScheduled, Payload: json.RawMessage(`{}`)},
		dedupEvent(4, 3, core.VerdictEscalate, 0.87),
	}}

	w := do(t, d, http.MethodGet, "/v1/dedup/verdicts?workflow_id=trip-42", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", w.Code, w.Body)
	}
	got := decode[VerdictsResponse](t, w)
	if got.Count != 2 {
		t.Fatalf("count = %d, want 2 (only the dedup events)", got.Count)
	}

	first := got.Verdicts[0]
	if first.Verdict != core.VerdictDuplicate || first.Similarity != 0.97 {
		t.Fatalf("verdict = %+v", first)
	}
	// The whole audit claim: every field needed to re-derive the decision is
	// present in the response because it is present in the log.
	if first.Model == "" || first.IntentText == "" || first.TauHigh == 0 || first.TauLow == 0 {
		t.Fatalf("verdict is not self-explaining: %+v", first)
	}
	if first.Seq != 2 || first.WorkflowID != "trip-42" {
		t.Fatalf("verdict is not located in history: %+v", first)
	}
	if got.Verdicts[1].Verdict != core.VerdictEscalate {
		t.Fatalf("second verdict = %+v", got.Verdicts[1])
	}
}

func TestDedupVerdictsAcrossOwnedShards(t *testing.T) {
	d := ownedDeps()
	d.Executions = &fakeExecutions{list: []core.Execution{
		{ShardID: 3, WorkflowID: "trip-42", RunID: "run-1", Status: core.StatusRunning},
	}}
	d.Events = &fakeEvents{events: []core.Event{dedupEvent(2, 1, core.VerdictDuplicate, 0.97)}}

	w := do(t, d, http.MethodGet, "/v1/dedup/verdicts", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", w.Code, w.Body)
	}
	if got := decode[VerdictsResponse](t, w); got.Count != 1 {
		t.Fatalf("count = %d, want 1", got.Count)
	}
}

func TestFailStep(t *testing.T) {
	d := ownedDeps()
	f := d.Faults.(*fakeFaults)

	w := do(t, d, http.MethodPost, "/v1/admin/fail-step",
		`{"workflow_id":"trip-42","activity_type":"charge_payment","k":3,"attempts":2,"non_retryable":true,"error":"injected"}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nbody: %s", w.Code, w.Body)
	}
	if f.seen.ActivityType != "charge_payment" || f.seen.K != 3 || !f.seen.NonRetryable {
		t.Fatalf("fault injector received %+v", f.seen)
	}
	// The fault has to be armed on the shard that will run the step.
	if f.seen.ShardID != 3 {
		t.Fatalf("ShardID = %d, want 3", f.seen.ShardID)
	}
}

func TestFailStepRequiresWorkflowID(t *testing.T) {
	w := do(t, ownedDeps(), http.MethodPost, "/v1/admin/fail-step", `{"activity_type":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestShards(t *testing.T) {
	d := ownedDeps()
	d.Leases = &fakeLeases{list: []core.Lease{
		{ShardID: 0, Owner: selfNode, Token: 4, ExpiresAt: routingNow.Add(time.Minute)},
		{ShardID: 1, Owner: "node-b", Token: 7, ExpiresAt: routingNow.Add(time.Minute)},
		{ShardID: 2, Owner: "node-c", Token: 2, ExpiresAt: routingNow.Add(-time.Minute)},
	}}

	w := do(t, d, http.MethodGet, "/v1/shards", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decode[ShardsResponse](t, w)
	if got.Node != selfNode || got.Count != 3 {
		t.Fatalf("body = %+v", got)
	}
	if !got.Shards[0].Self || got.Shards[0].Expired {
		t.Fatalf("shard 0 = %+v, want ours and live", got.Shards[0])
	}
	if got.Shards[1].Self {
		t.Fatalf("shard 1 = %+v, want not ours", got.Shards[1])
	}
	// An expired lease must read as expired, however recently it was ours.
	if !got.Shards[2].Expired || got.Shards[2].Self {
		t.Fatalf("shard 2 = %+v, want expired", got.Shards[2])
	}
	// The fencing token is exposed because watching it increase is how the
	// safety argument is checked from outside the process.
	if got.Shards[1].Token != 7 {
		t.Fatalf("fencing token = %d, want 7", got.Shards[1].Token)
	}
}

func TestHealthz(t *testing.T) {
	w := do(t, ownedDeps(), http.MethodGet, "/healthz", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decode[HealthResponse](t, w)
	if got.Status != "ok" || got.Node != selfNode || got.Shards != 1 {
		t.Fatalf("body = %+v", got)
	}
}

// TestHealthzReportsWhatIsOwnedNotWhatExists is a regression guard.
//
// /healthz used to report len(OwnedShards), and engined sets OwnedShards to
// EVERY shard in the cluster on purpose, so all three nodes answered
// owned_shards=16 whatever they actually held. A cluster with four dead shards
// and half its start requests failing reported three healthy nodes each owning
// everything. owned_shards now comes from the lease manager.
func TestHealthzReportsWhatIsOwnedNotWhatExists(t *testing.T) {
	tests := []struct {
		name              string
		ownedNow          []core.ShardID
		wantOwned         int
		wantClusterShards int
	}{
		{
			name:     "a node holding two of sixteen says two",
			ownedNow: []core.ShardID{3, 11},
			// Sixteen shards exist; this node has leases on two of them.
			wantOwned: 2, wantClusterShards: 16,
		},
		{
			// The case that was invisible before. A node fenced out of
			// everything, or one that has only just started, owns nothing —
			// and must say so rather than claiming the whole cluster.
			name:     "a node holding nothing says nothing",
			ownedNow: []core.ShardID{},
			// Still "ok": owning no shard is not a reason to be restarted.
			wantOwned: 0, wantClusterShards: 16,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := ownedDeps()
			deps.OwnedShards = make([]core.ShardID, 16)
			for i := range deps.OwnedShards {
				deps.OwnedShards[i] = core.ShardID(i)
			}
			owned := tc.ownedNow
			deps.OwnedNow = func() []core.ShardID { return owned }

			got := decode[HealthResponse](t, do(t, deps, http.MethodGet, "/healthz", ""))
			if got.Status != "ok" {
				t.Errorf("status = %q, want ok: owning no shard is not unhealthy", got.Status)
			}
			if got.Shards != tc.wantOwned {
				t.Errorf("owned_shards = %d, want %d", got.Shards, tc.wantOwned)
			}
			if got.ClusterShards != tc.wantClusterShards {
				t.Errorf("cluster_shards = %d, want %d", got.ClusterShards, tc.wantClusterShards)
			}
		})
	}
}

// TestHealthzIgnoresStorage: health drives restarts, and a node that cannot
// momentarily reach Postgres must not be killed for it — the lease expiring
// and another node taking over is the designed response.
func TestHealthzIgnoresStorage(t *testing.T) {
	d := ownedDeps()
	d.Executions = &fakeExecutions{getErr: errors.New("connection refused")}
	d.Leases = &fakeLeases{err: errors.New("connection refused")}

	if w := do(t, d, http.MethodGet, "/healthz", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := obs.New(reg)
	m.FencingRejection(3)
	m.ObserveDedup("search_flights", core.VerdictDuplicate, 0.02)

	d := ownedDeps()
	d.Gatherer = reg

	w := do(t, d, http.MethodGet, "/metrics", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`fencing_rejections_total{shard="3"} 1`,
		`dedup_verdict_total{activity_type="search_flights",verdict="DUPLICATE"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics does not contain %q\nbody:\n%s", want, body)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	// Method-and-pattern routes make the mux answer 405 itself, so no handler
	// has to remember to check r.Method.
	w := do(t, ownedDeps(), http.MethodDelete, "/v1/workflows/trip-42", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	if w := do(t, ownedDeps(), http.MethodGet, "/v1/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestMissingDependencyIs501: a half-wired node should say which piece is
// missing rather than panic and take the process down.
func TestMissingDependencyIs501(t *testing.T) {
	tests := []struct {
		name   string
		strip  func(*Deps)
		method string
		target string
		body   string
	}{
		{"no starter", func(d *Deps) { d.Starter = nil }, http.MethodPost, "/v1/workflows", `{"workflow_id":"a","workflow_type":"b"}`},
		{"no signaller", func(d *Deps) { d.Signaller = nil }, http.MethodPost, "/v1/workflows/a/signals", `{"name":"x"}`},
		{"no canceller", func(d *Deps) { d.Canceller = nil }, http.MethodPost, "/v1/workflows/a/cancel", ""},
		{"no fault injector", func(d *Deps) { d.Faults = nil }, http.MethodPost, "/v1/admin/fail-step", `{"workflow_id":"a"}`},
		{"no execution store", func(d *Deps) { d.Executions = nil }, http.MethodGet, "/v1/workflows/a", ""},
		{"no event store", func(d *Deps) { d.Events = nil }, http.MethodGet, "/v1/workflows/a/history", ""},
		{"no lease store for shards", func(d *Deps) { d.Leases = nil }, http.MethodGet, "/v1/shards", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ownedDeps()
			tc.strip(&d)
			w := do(t, d, tc.method, tc.target, tc.body)
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501\nbody: %s", w.Code, w.Body)
			}
		})
	}
}

// ---- routing headers ------------------------------------------------------

// TestRoutingHeadersOnEveryResponse: X-Shard-Id and X-Owner-Node are how a
// failover demo is watched, so they must never be missing.
func TestRoutingHeadersOnEveryResponse(t *testing.T) {
	targets := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/healthz", ""},
		{http.MethodGet, "/v1/shards", ""},
		{http.MethodGet, "/v1/workflows", ""},
		{http.MethodGet, "/v1/workflows/trip-42", ""},
		{http.MethodGet, "/v1/workflows/trip-42/history", ""},
		{http.MethodPost, "/v1/workflows", `{"workflow_id":"trip-42","workflow_type":"B"}`},
		{http.MethodPost, "/v1/workflows/trip-42/signals", `{"name":"x"}`},
		{http.MethodPost, "/v1/workflows/trip-42/cancel", ""},
	}
	for _, tc := range targets {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := do(t, ownedDeps(), tc.method, tc.path, tc.body)
			if w.Header().Get(HeaderShardID) == "" {
				t.Errorf("%s is missing", HeaderShardID)
			}
			if w.Header().Get(HeaderOwnerNode) == "" {
				t.Errorf("%s is missing", HeaderOwnerNode)
			}
		})
	}
}

func TestRoutingHeadersNameTheShard(t *testing.T) {
	w := do(t, ownedDeps(), http.MethodGet, "/v1/workflows/trip-42", "")
	if got := w.Header().Get(HeaderShardID); got != "3" {
		t.Fatalf("%s = %q, want 3", HeaderShardID, got)
	}
	if got := w.Header().Get(HeaderOwnerNode); got != selfNode {
		t.Fatalf("%s = %q, want %s", HeaderOwnerNode, got, selfNode)
	}
}

// TestRoutingHeadersOnRefusal: on a 409 the caller most wants to know who it
// should have asked, so the headers name the real owner, not us.
func TestRoutingHeadersOnRefusal(t *testing.T) {
	d := ownedDeps()
	d.Leases = &fakeLeases{lease: core.Lease{Owner: "node-c", ExpiresAt: routingNow.Add(time.Minute)}}

	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := w.Header().Get(HeaderOwnerNode); got != "node-c" {
		t.Fatalf("%s = %q, want node-c", HeaderOwnerNode, got)
	}
	if got := w.Header().Get(HeaderShardID); got != "3" {
		t.Fatalf("%s = %q, want 3", HeaderShardID, got)
	}
}

func TestRequestIDIsEchoed(t *testing.T) {
	d := ownedDeps()

	t.Run("client id is honoured", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.Header.Set(obs.RequestIDHeader, "trace-abc")
		w := httptest.NewRecorder()
		New(d).ServeHTTP(w, r)
		if got := w.Header().Get(obs.RequestIDHeader); got != "trace-abc" {
			t.Fatalf("request id = %q, want trace-abc", got)
		}
	})

	t.Run("one is minted otherwise", func(t *testing.T) {
		w := do(t, d, http.MethodGet, "/healthz", "")
		if got := w.Header().Get(obs.RequestIDHeader); len(got) != 32 {
			t.Fatalf("minted request id = %q, want 32 hex characters", got)
		}
	})
}

// ---- the error mapping table ----------------------------------------------

// TestErrorMapping walks the contract row by row. This table IS the API's
// promise to its clients: their entire retry strategy is derived from it, so
// every row gets its own case.
func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantError  string
		check      func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			name:       "NotOwner is 409 naming the current owner",
			err:        &core.NotOwnerError{ShardID: 3, CurrentOwner: "node-c"},
			wantStatus: http.StatusConflict,
			wantError:  "NotOwner",
			check: func(t *testing.T, w *httptest.ResponseRecorder) {
				got := decode[ErrorResponse](t, w)
				if got.CurrentOwner != "node-c" {
					t.Fatalf("current_owner = %q, want node-c", got.CurrentOwner)
				}
				if got.ShardID == nil || *got.ShardID != 3 {
					t.Fatalf("shard_id = %v, want 3", got.ShardID)
				}
				// A routing miss is retryable, just somewhere else.
				if !got.Retry {
					t.Fatal("retry should be true")
				}
			},
		},
		{
			name:       "WorkflowNotFound is 404",
			err:        core.ErrWorkflowNotFound,
			wantStatus: http.StatusNotFound,
			wantError:  "WorkflowNotFound",
		},
		{
			name: "NonDeterminism is 500 exposing the divergence",
			err: &core.NonDeterminismError{
				Seq: 14, K: 3,
				Recorded:  "ActivityTaskScheduled(charge_payment)",
				Requested: "ActivityTaskScheduled(refund_payment)",
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "NonDeterminism",
			check: func(t *testing.T, w *httptest.ResponseRecorder) {
				got := decode[ErrorResponse](t, w)
				// All four coordinates are what make a replay bug findable.
				if got.Seq == nil || *got.Seq != 14 {
					t.Fatalf("seq = %v, want 14", got.Seq)
				}
				if got.K == nil || *got.K != 3 {
					t.Fatalf("k = %v, want 3", got.K)
				}
				if !strings.Contains(got.Recorded, "charge_payment") {
					t.Fatalf("recorded = %q", got.Recorded)
				}
				if !strings.Contains(got.Requested, "refund_payment") {
					t.Fatalf("requested = %q", got.Requested)
				}
			},
		},
		{
			name: "CompensationFailed is 200 surfacing COMPENSATING_FAILED",
			err: &core.CompensationFailedError{
				WorkflowID: "trip-42", Activity: "refund_payment", Attempts: 5,
				Cause: errors.New("gateway rejected the refund"),
			},
			// 200, not 5xx: a 5xx invites a retry, and retrying a failed
			// compensation just fails again while hiding the state that needs
			// a human.
			wantStatus: http.StatusOK,
			wantError:  "CompensationFailed",
			check: func(t *testing.T, w *httptest.ResponseRecorder) {
				got := decode[ErrorResponse](t, w)
				if got.Status != core.StatusCompensatingFailed {
					t.Fatalf("status = %q, want COMPENSATING_FAILED", got.Status)
				}
				if got.Activity != "refund_payment" {
					t.Fatalf("activity = %q", got.Activity)
				}
				if got.Attempts == nil || *got.Attempts != 5 {
					t.Fatalf("attempts = %v, want 5", got.Attempts)
				}
				if got.Retry {
					t.Fatal("retry must be false: this needs a person, not another attempt")
				}
			},
		},
		{
			name:       "LostOwnership is 503 with Retry-After",
			err:        core.ErrLostOwnership,
			wantStatus: http.StatusServiceUnavailable,
			wantError:  "LostOwnership",
			check: func(t *testing.T, w *httptest.ResponseRecorder) {
				if got := w.Header().Get("Retry-After"); got == "" {
					t.Fatal("Retry-After is missing")
				}
				if !decode[ErrorResponse](t, w).Retry {
					t.Fatal("retry should be true")
				}
			},
		},
		{
			name:       "an unrecognised error is a generic 500",
			err:        errors.New("pgx: dial tcp 10.0.0.7:5432: connection refused"),
			wantStatus: http.StatusInternalServerError,
			wantError:  "Internal",
			check: func(t *testing.T, w *httptest.ResponseRecorder) {
				// Nothing internal may leak: a driver error can carry a host,
				// a port or a query fragment.
				if strings.Contains(w.Body.String(), "10.0.0.7") || strings.Contains(w.Body.String(), "pgx") {
					t.Fatalf("internal detail leaked into the response: %s", w.Body)
				}
			},
		},
		{
			name:       "a wrapped sentinel is still recognised",
			err:        errWrap{core.ErrWorkflowNotFound},
			wantStatus: http.StatusNotFound,
			wantError:  "WorkflowNotFound",
		},
		{
			name:       "a wrapped typed error is still recognised",
			err:        errWrap{&core.NotOwnerError{ShardID: 5, CurrentOwner: "node-z"}},
			wantStatus: http.StatusConflict,
			wantError:  "NotOwner",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Driven through a real route so the mapping is tested where
			// clients actually meet it.
			d := ownedDeps()
			d.Starter = &fakeStarter{err: tc.err}
			w := do(t, d, http.MethodPost, "/v1/workflows",
				`{"workflow_id":"trip-42","workflow_type":"BookingWorkflow"}`)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d\nbody: %s", w.Code, tc.wantStatus, w.Body)
			}
			if got := decode[ErrorResponse](t, w).Error; got != tc.wantError {
				t.Fatalf("error = %q, want %q\nbody: %s", got, tc.wantError, w.Body)
			}
			if tc.check != nil {
				tc.check(t, w)
			}
		})
	}
}

// TestErrorMappingAlreadyOpenFallback covers the one row that normally never
// reaches writeError, by making the execution lookup fail too. It must still
// be 200: a duplicate start is success however we learn about it.
func TestErrorMappingAlreadyOpenFallback(t *testing.T) {
	d := ownedDeps()
	d.Starter = &fakeStarter{err: core.ErrWorkflowAlreadyOpen}
	d.Executions = &fakeExecutions{getErr: errors.New("transient read failure")}

	w := do(t, d, http.MethodPost, "/v1/workflows",
		`{"workflow_id":"trip-42","workflow_type":"BookingWorkflow"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", w.Code, w.Body)
	}
	body := decode[map[string]any](t, w)
	if body["already_open"] != true {
		t.Fatalf("body = %s", w.Body)
	}
}

// TestUnownedShardIsNotOwner is the routing refusal end to end.
func TestUnownedShardIsNotOwner(t *testing.T) {
	routes := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/workflows", `{"workflow_id":"trip-42","workflow_type":"B"}`},
		{http.MethodGet, "/v1/workflows/trip-42", ""},
		{http.MethodGet, "/v1/workflows/trip-42/history", ""},
		{http.MethodPost, "/v1/workflows/trip-42/signals", `{"name":"x"}`},
		{http.MethodPost, "/v1/workflows/trip-42/cancel", ""},
		{http.MethodPost, "/v1/admin/fail-step", `{"workflow_id":"trip-42"}`},
		{http.MethodGet, "/v1/dedup/verdicts?workflow_id=trip-42", ""},
	}
	for _, tc := range routes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			d := ownedDeps()
			d.Leases = &fakeLeases{lease: core.Lease{Owner: "node-c", ExpiresAt: routingNow.Add(time.Minute)}}

			w := do(t, d, tc.method, tc.path, tc.body)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409\nbody: %s", w.Code, w.Body)
			}
			got := decode[ErrorResponse](t, w)
			if got.Error != "NotOwner" || got.CurrentOwner != "node-c" {
				t.Fatalf("body = %s", w.Body)
			}
		})
	}
}

// TestExpiredOwnLeaseIsNotOwner is the split-brain guard: our name is still in
// the lease row, but it has lapsed, so we must refuse rather than serve.
func TestExpiredOwnLeaseIsNotOwner(t *testing.T) {
	d := ownedDeps()
	d.Leases = &fakeLeases{lease: core.Lease{Owner: selfNode, ExpiresAt: routingNow.Add(-time.Second)}}

	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := decode[ErrorResponse](t, w); got.CurrentOwner != "" {
		t.Fatalf("current_owner = %q, want it unknown: pointing at a lapsed owner just sends the client somewhere that will refuse again", got.CurrentOwner)
	}
}

// errWrap proves the mapping uses errors.Is/As rather than comparing values,
// so an engine that adds context to an error does not break every client.
type errWrap struct{ inner error }

func (e errWrap) Error() string { return "wrapped: " + e.inner.Error() }
func (e errWrap) Unwrap() error { return e.inner }
