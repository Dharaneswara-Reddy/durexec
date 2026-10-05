package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every test here runs on hand-built evidence, fakes and httptest servers.
// None needs a cluster, a database or the network.

var t0 = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

func mk(wf string, seq int64, typ, payload string) EventRow {
	return EventRow{
		ShardID: 1, WorkflowID: wf, RunID: "run-" + wf, Seq: seq, Type: typ,
		Payload: json.RawMessage(payload), CreatedAt: t0.Add(time.Duration(seq) * time.Second),
	}
}

func cleanRun(wf string) []EventRow {
	return []EventRow{
		mk(wf, 1, evWorkflowStarted, `{}`),
		mk(wf, 2, evActivityScheduled, `{"k":1,"activity_type":"ChargePayment","dedup_key":"key-1"}`),
		mk(wf, 3, evActivityCompleted, `{"k":1}`),
		mk(wf, 4, evWorkflowCompleted, `{"result":{}}`),
	}
}

// compensatedRun schedules three forward steps, fails the fourth, then runs
// the given compensations in the given order.
func compensatedRun(wf string, undo ...string) []EventRow {
	out := []EventRow{
		mk(wf, 1, evWorkflowStarted, `{}`),
		mk(wf, 2, evActivityScheduled, `{"k":1,"activity_type":"ChargePayment","dedup_key":"a"}`),
		mk(wf, 3, evActivityCompleted, `{"k":1}`),
		mk(wf, 4, evActivityScheduled, `{"k":2,"activity_type":"ReserveInventory","dedup_key":"b"}`),
		mk(wf, 5, evActivityCompleted, `{"k":2}`),
		mk(wf, 6, evActivityScheduled, `{"k":3,"activity_type":"NotifyRestaurant","dedup_key":"c"}`),
		mk(wf, 7, evActivityCompleted, `{"k":3}`),
		mk(wf, 8, evActivityScheduled, `{"k":4,"activity_type":"DispatchDriver","dedup_key":"d"}`),
		mk(wf, 9, evActivityFailed, `{"k":4,"error":"boom","attempt":1,"non_retryable":true}`),
	}
	seq, k := int64(10), 5
	for _, u := range undo {
		p, _ := json.Marshal(map[string]any{"k": k, "activity_type": u, "dedup_key": u})
		out = append(out, mk(wf, seq, evActivityScheduled, string(p)))
		out = append(out, mk(wf, seq+1, evActivityCompleted, `{"k":`+itoa(k)+`}`))
		seq, k = seq+2, k+1
	}
	return out
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func withEvents(events ...[]EventRow) *Evidence {
	ev := newEvidence(Options{EventLimit: 100000})
	ev.StoreRead = true
	for _, e := range events {
		ev.Events = append(ev.Events, e...)
	}
	return ev
}

const verdictOK = `{"k":2,"verdict":"DISTINCT","matched_k":-1,"similarity":0.31,"tau_high":0.9,"tau_low":0.75,"model":"all-MiniLM-L6-v2","intent_text":"book LHR-EDI"}`
const verdictNoModel = `{"k":2,"verdict":"DISTINCT","matched_k":-1,"similarity":0.31,"tau_high":0.9,"tau_low":0.75,"intent_text":"book LHR-EDI"}`

func agentHistory(withSignal bool) []EventRow {
	h := []EventRow{
		mk("ag", 1, evWorkflowStarted, `{}`),
		mk("ag", 2, evActivityScheduled, `{"k":1,"activity_type":"Plan","dedup_key":"p"}`),
	}
	if withSignal {
		h = append(h, mk("ag", 3, evSignalReceived, `{"name":"HumanApproval","payload":{"approved":true}}`))
	}
	return append(h,
		mk("ag", 4, evActivityCompleted, `{"k":1}`),
		mk("ag", 5, evActivityScheduled, `{"k":2,"activity_type":"Finalize","dedup_key":"f"}`),
		mk("ag", 6, evActivityCompleted, `{"k":2}`),
		mk("ag", 7, evWorkflowCompleted, `{"result":{"approved":true}}`),
	)
}

func withAgent(history []EventRow) *Evidence {
	ev := newEvidence(Options{})
	ev.Agent = &Driven{ID: "ag", Type: wfAgent, Started: true, Terminal: true, Status: statusCompleted}
	ev.AgentHistory = history
	return ev
}

// TestOutcomeClassification drives each of the four outcomes through the real
// verdict rules, from evidence built by hand.
func TestOutcomeClassification(t *testing.T) {
	cases := []struct {
		name string
		ev   func() *Evidence
		want map[string]Outcome
	}{
		{
			name: "clean log: measured parts pass, kill-dependent rows stay MANUAL",
			ev:   func() *Evidence { return withEvents(cleanRun("wf-a")) },
			want: map[string]Outcome{"C-1": Pass, "C-2": Pass, "FR-1": Manual, "FR-2": Manual, "NFR-3": Manual, "FR-8": Manual},
		},
		{
			name: "seq gap: FR-1 FAIL",
			ev: func() *Evidence {
				r := cleanRun("wf-a")
				return withEvents(append(r[:2], r[3:]...))
			},
			want: map[string]Outcome{"FR-1": Fail},
		},
		{
			name: "database down: log rows BLOCKED, never PASS",
			ev: func() *Evidence {
				ev := newEvidence(Options{})
				ev.StoreErr, ev.SeqDupsErr = "connection refused", "connection refused"
				return ev
			},
			want: map[string]Outcome{"C-1": Blocked, "C-2": Blocked, "FR-1": Blocked, "FR-10": Blocked, "FR-14": Blocked, "NFR-3": Blocked},
		},
		{
			name: "LIFO compensation: FR-10 PASS",
			ev: func() *Evidence {
				return withEvents(compensatedRun("wf-c", "CancelRestaurantOrder", "ReleaseInventory", "RefundPayment"))
			},
			want: map[string]Outcome{"C-7": Pass, "FR-10": Pass},
		},
		{
			name: "compensation out of order: FR-10 FAIL",
			ev: func() *Evidence {
				return withEvents(compensatedRun("wf-c", "CancelRestaurantOrder", "RefundPayment", "ReleaseInventory"))
			},
			want: map[string]Outcome{"C-7": Fail, "FR-10": Fail},
		},
		{
			name: "no compensation anywhere: FR-10 BLOCKED",
			ev:   func() *Evidence { return withEvents(cleanRun("wf-a")) },
			want: map[string]Outcome{"C-7": Blocked, "FR-10": Blocked},
		},
		{
			name: "complete verdict: FR-14 PASS",
			ev: func() *Evidence {
				return withEvents(append(cleanRun("wf-v"), mk("wf-v", 5, evDedupEvaluated, verdictOK)))
			},
			want: map[string]Outcome{"C-8": Pass, "FR-14": Pass},
		},
		{
			name: "verdict without model: FR-14 FAIL",
			ev: func() *Evidence {
				return withEvents(append(cleanRun("wf-v"), mk("wf-v", 5, evDedupEvaluated, verdictNoModel)))
			},
			want: map[string]Outcome{"C-8": Fail, "FR-14": Fail},
		},
		{
			name: "three failures then one success: FR-3 PASS",
			ev: func() *Evidence {
				return withEvents([]EventRow{
					mk("wf-r", 1, evWorkflowStarted, `{}`),
					mk("wf-r", 2, evActivityScheduled, `{"k":1,"activity_type":"ChargePayment","dedup_key":"x"}`),
					mk("wf-r", 3, evActivityFailed, `{"k":1,"error":"e","attempt":1}`),
					mk("wf-r", 4, evActivityFailed, `{"k":1,"error":"e","attempt":2}`),
					mk("wf-r", 5, evActivityFailed, `{"k":1,"error":"e","attempt":3}`),
					mk("wf-r", 6, evActivityCompleted, `{"k":1}`),
				})
			},
			want: map[string]Outcome{"FR-3": Pass},
		},
		{
			name: "signal buffered before the wait: FR-5 PASS",
			ev:   func() *Evidence { return withAgent(agentHistory(true)) },
			want: map[string]Outcome{"FR-5": Pass},
		},
		{
			name: "signal accepted but never journaled: FR-5 FAIL",
			ev:   func() *Evidence { return withAgent(agentHistory(false)) },
			want: map[string]Outcome{"FR-5": Fail},
		},
		{
			name: "nothing reachable: requirement rows are only MANUAL or BLOCKED",
			ev:   func() *Evidence { return newEvidence(Options{}) },
			want: map[string]Outcome{"FR-5": Blocked, "FR-6": Blocked, "FR-7": Manual, "NFR-2": Manual, "C-9": Blocked, "C-10": Blocked},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := NewRecorder(AllRows())
			Evaluate(rec, tc.ev())
			for id, want := range tc.want {
				if got := rec.Get(id); got.Outcome != want {
					t.Errorf("%s = %s, want %s (observed %q, action %q)", id, got.Outcome, want, got.Observed, got.Action)
				}
			}
		})
	}
}

func TestEveryRowIsEvaluatedEvenWithNoEvidence(t *testing.T) {
	rec := NewRecorder(AllRows())
	Evaluate(rec, newEvidence(Options{}))
	for _, r := range AllRows() {
		res := rec.Get(r.ID)
		if !res.Evaluated {
			t.Errorf("%s was never decided by any check", r.ID)
		}
		if res.Outcome == Pass || res.Outcome == Fail {
			t.Errorf("%s = %s with no evidence at all; only MANUAL or BLOCKED is honest", r.ID, res.Outcome)
		}
	}
}

func TestRecorderNeverDowngradesAFail(t *testing.T) {
	rec := NewRecorder(Spec)
	if got := rec.Get("FR-1"); got.Outcome != Blocked || got.Evaluated {
		t.Fatalf("fresh row = %+v, want un-evaluated BLOCKED", got)
	}
	rec.Fail("FR-1", "gap at seq %d", 3)
	rec.Blocked("FR-1", "db", "later check could not run")
	rec.Pass("FR-1", "later check passed")
	if got := rec.Get("FR-1"); got.Outcome != Fail || got.Observed != "gap at seq 3" {
		t.Fatalf("FR-1 = %+v, want the original FAIL kept", got)
	}
}

// TestDuplicateKDetector plants the duplicate the primary key cannot see: the
// same step completed twice, at two different seqs.
func TestDuplicateKDetector(t *testing.T) {
	run := []EventRow{
		mk("wf-d", 1, evWorkflowStarted, `{}`),
		mk("wf-d", 2, evActivityScheduled, `{"k":1,"activity_type":"ChargePayment","dedup_key":"a"}`),
		mk("wf-d", 3, evActivityCompleted, `{"k":1}`),
		mk("wf-d", 4, evActivityScheduled, `{"k":2,"activity_type":"ReserveInventory","dedup_key":"b"}`),
		mk("wf-d", 5, evActivityCompleted, `{"k":2}`),
		mk("wf-d", 6, evActivityCompleted, `{"k":2}`), // the planted duplicate
		mk("wf-d", 7, evWorkflowCompleted, `{"result":{}}`),
	}

	dups := FindDuplicateCompletions(run)
	if len(dups) != 1 || dups[0].K != 2 || dups[0].Count != 2 {
		t.Fatalf("FindDuplicateCompletions = %+v, want one duplicate at k=2", dups)
	}
	if got := dups[0].Seqs; len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("duplicate seqs = %v, want [5 6]", got)
	}
	if li := CheckLogIntegrity(run); len(li.Duplicates) != 0 {
		t.Fatalf("seq-level scan found %v; it should be blind to this, which is the point", li.Duplicates)
	}
	if dups := FindDuplicateCompletions(cleanRun("wf-ok")); len(dups) != 0 {
		t.Fatalf("clean run reported duplicates: %+v", dups)
	}

	rec := NewRecorder(AllRows())
	Evaluate(rec, withEvents(run))
	for id, want := range map[string]Outcome{"C-1": Pass, "C-2": Fail, "FR-2": Fail, "FR-8": Fail, "NFR-3": Fail} {
		if got := rec.Get(id).Outcome; got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	if !strings.Contains(rec.Get("C-1").Observed, "WEAK EVIDENCE") {
		t.Errorf("C-1 must be labelled weak evidence, got %q", rec.Get("C-1").Observed)
	}
	if !strings.Contains(rec.Get("C-2").Observed, "REAL duplicate-side-effect check") {
		t.Errorf("C-2 must be labelled the real check, got %q", rec.Get("C-2").Observed)
	}
}

func TestFencingTokenMonotonicity(t *testing.T) {
	cases := []struct {
		name   string
		series map[int32][]int64
		want   []TokenViolation
	}{
		{"rising", map[int32][]int64{0: {1, 2, 3}}, nil},
		{"flat is allowed", map[int32][]int64{0: {4, 4, 4}}, nil},
		{"one decrease", map[int32][]int64{0: {1, 2}, 7: {5, 6, 4, 9}},
			[]TokenViolation{{ShardID: 7, Index: 2, From: 6, To: 4}}},
		{"strictly decreasing", map[int32][]int64{3: {9, 8, 7}},
			[]TokenViolation{{ShardID: 3, Index: 1, From: 9, To: 8}, {ShardID: 3, Index: 2, From: 8, To: 7}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckTokenMonotonic(tc.series)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}

	// Through the verdict rules: samples in time order, one shard going down.
	ev := newEvidence(Options{})
	ev.TokenSamples = []TokenSample{
		{Source: "db/start", At: t0, Tokens: map[int32]int64{0: 3, 1: 5}},
		{Source: "node/mid", At: t0.Add(time.Second), Tokens: map[int32]int64{0: 3, 1: 6}},
		{Source: "db/end", At: t0.Add(2 * time.Second), Tokens: map[int32]int64{0: 2, 1: 6}},
	}
	rec := NewRecorder(AllRows())
	Evaluate(rec, ev)
	if got := rec.Get("C-5").Outcome; got != Fail {
		t.Errorf("C-5 = %s, want FAIL for a decreasing token", got)
	}
	if got := rec.Get("FR-8").Outcome; got != Fail {
		t.Errorf("FR-8 = %s, want FAIL for a decreasing token", got)
	}
}

// ---- the 409 chase ---------------------------------------------------------

func refusal(owner string) *Response {
	b, _ := json.Marshal(map[string]any{"error": "NotOwner", "current_owner": owner, "retry": true})
	return &Response{Status: http.StatusConflict, Body: b}
}

func TestFollowNotOwner(t *testing.T) {
	book := map[string]string{"node-a": "A", "node-b": "B", "node-c": "C"}
	resolve := func(id string) (string, bool) { u, ok := book[id]; return u, ok }

	cases := []struct {
		name     string
		answers  map[string]*Response
		maxHops  int
		wantCode int
		wantHops []string
	}{
		{"owner answers first time", map[string]*Response{"A": {Status: 200}}, 4, 200, []string{"A"}},
		{"one hop to the named owner", map[string]*Response{"A": refusal("node-b"), "B": {Status: 200}}, 4, 200, []string{"A", "B"}},
		{"two hops when ownership moved", map[string]*Response{"A": refusal("node-b"), "B": refusal("node-c"), "C": {Status: 201}}, 4, 201, []string{"A", "B", "C"}},
		{"header fallback", map[string]*Response{
			"A": {Status: 409, Body: []byte(`{}`), Header: http.Header{"X-Owner-Node": []string{"node-b"}}},
			"B": {Status: 200}}, 4, 200, []string{"A", "B"}},
		{"nodes disagree: stop, return the 409", map[string]*Response{"A": refusal("node-b"), "B": refusal("node-a")}, 4, 409, []string{"A", "B"}},
		{"unknown owner: return the 409", map[string]*Response{"A": refusal("node-z")}, 4, 409, []string{"A"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			send := func(_ context.Context, base string) (*Response, error) {
				r, ok := tc.answers[base]
				if !ok {
					return nil, errors.New("unexpected node " + base)
				}
				cp := *r
				return &cp, nil
			}
			resp, err := FollowNotOwner(context.Background(), "A", resolve, tc.maxHops, send)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Status != tc.wantCode || strings.Join(resp.Hops, ",") != strings.Join(tc.wantHops, ",") {
				t.Fatalf("got %d via %v, want %d via %v", resp.Status, resp.Hops, tc.wantCode, tc.wantHops)
			}
		})
	}
}

// TestClusterFollows409OverHTTP runs the same chase through real HTTP.
func TestClusterFollows409OverHTTP(t *testing.T) {
	health := func(w http.ResponseWriter, id string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "node": id, "owned_shards": 8})
	}
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			health(w, "node-b")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_id": "wf-1", "shard_id": 5, "status": "RUNNING"})
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			health(w, "node-a")
			return
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "NotOwner", "current_owner": "node-b", "shard_id": 5})
	}))
	defer a.Close()

	c := NewCluster([]string{a.URL, b.URL}, nil)
	c.Discover(context.Background())
	resp, err := c.DoFrom(context.Background(), a.URL, http.MethodGet, "/v1/workflows/wf-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || resp.BaseURL != b.URL || len(resp.Hops) != 2 {
		t.Fatalf("got %d from %s via %v, want 200 from node-b after one hop", resp.Status, resp.BaseURL, resp.Hops)
	}

	// The routing probe judges the same 409 without chasing it.
	ok, named, _ := judgeRoutingAnswer(refusal("node-b"), "wf-1", 5, "node-b")
	if !ok || named != "node-b" {
		t.Fatalf("a 409 naming the true owner must count as correct routing")
	}
	if ok, _, _ := judgeRoutingAnswer(refusal("node-c"), "wf-1", 5, "node-b"); ok {
		t.Fatalf("a 409 naming the wrong owner must count as a routing fault")
	}
}

// ---- output -----------------------------------------------------------------

func TestWriteCSV(t *testing.T) {
	rows := []Requirement{
		{ID: "FR-1", Kind: "FR", Text: "plain", Acceptance: "a, b"},
		{ID: "NFR-9", Kind: "NFR", Text: `says "quoted"`, Acceptance: "line one\nline two"},
	}
	rec := NewRecorder(rows)
	rec.Pass("FR-1", "measured 3, 4 and 5")
	rec.Manual("NFR-9", "run docker stats", `limit "300m", total 2300 MB`)

	var buf bytes.Buffer
	if err := WriteCSV(&buf, rows, rec); err != nil {
		t.Fatal(err)
	}
	got, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\n%s", err, buf.String())
	}
	want := [][]string{
		CSVHeader,
		{"FR-1", "FR", "PASS", "plain", "a, b", "measured 3, 4 and 5", "", "true"},
		{"NFR-9", "NFR", "MANUAL", `says "quoted"`, "line one\nline two", `limit "300m", total 2300 MB`, "run docker stats", "true"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if strings.Join(got[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("record %d:\n got  %q\n want %q", i, got[i], want[i])
		}
	}
	if strings.Join(CSVHeader, ",") != "id,kind,outcome,requirement,acceptance,observed,action,evaluated" {
		t.Errorf("CSV header drifted: %v", CSVHeader)
	}
}

func TestExitCode(t *testing.T) {
	rows := []Requirement{{ID: "A"}, {ID: "B"}, {ID: "C"}}
	rec := NewRecorder(rows)
	rec.Manual("A", "kill a node", "")
	rec.Blocked("B", "database", "refused")
	// C is left un-evaluated, which is also BLOCKED.
	if code := rec.ExitCode(); code != 0 {
		t.Fatalf("only MANUAL and BLOCKED rows: exit %d, want 0", code)
	}
	rec.Pass("C", "ok")
	if code := rec.ExitCode(); code != 0 {
		t.Fatalf("PASS, MANUAL, BLOCKED: exit %d, want 0", code)
	}
	rec.Fail("C", "broken")
	if code := rec.ExitCode(); code != 1 {
		t.Fatalf("with a FAIL: exit %d, want 1", code)
	}
}

// TestRunDegradesToBlocked runs the whole command against a node that is
// down and a database that refuses: it must finish, write the CSV, report
// nothing as PASS or FAIL, and exit 0.
func TestRunDegradesToBlocked(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down for the test", http.StatusServiceUnavailable)
	}))
	defer down.Close()

	dir := t.TempDir()
	csvPath := filepath.Join(dir, "out.csv")
	openFails := func(context.Context, string) (Store, error) {
		return nil, errors.New("fake: database unreachable")
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-cluster", down.URL,
		"-dsn", "postgres://fake/db",
		"-standby-dsn", "",
		"-csv", csvPath,
		"-compose", filepath.Join(dir, "missing-compose.yml"),
		"-gomod", filepath.Join(dir, "missing-go.mod"),
		"-timeout", "30s",
		"-request-timeout", "2s",
	}, &stdout, &stderr, openFails)

	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s\n%s", code, stdout.String(), stderr.String())
	}
	for _, heading := range []string{"FUNCTIONAL REQUIREMENTS", "NON-FUNCTIONAL REQUIREMENTS", "SUPPORTING CHECKS", "SUMMARY"} {
		if !strings.Contains(stdout.String(), heading) {
			t.Errorf("output lacks %q", heading)
		}
	}
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if want := len(Spec) + len(Checks) + 1; len(records) != want {
		t.Fatalf("CSV has %d records, want %d", len(records), want)
	}
	for _, r := range records[1:] {
		if r[2] == string(Pass) || r[2] == string(Fail) {
			t.Errorf("%s = %s with nothing reachable", r[0], r[2])
		}
	}
}

// ---- configuration scanning ------------------------------------------------

// TestDependencyRowNeedsALiveNode: clean files alone are a description of a
// deployment, not of the running one, so they may not produce a PASS.
func TestDependencyRowNeedsALiveNode(t *testing.T) {
	ev := newEvidence(Options{})
	ev.Compose = parseCompose("services:\n  postgres:\n    image: postgres:16\n  node-a:\n    image: engine\n")
	ev.GoMod = parseGoMod("module x\nrequire github.com/jackc/pgx/v5 v5.0.0\n")

	rec := NewRecorder(AllRows())
	Evaluate(rec, ev)
	if rec.Get("C-10").Outcome != Blocked || rec.Get("NFR-10").Outcome != Blocked {
		t.Fatalf("no live node: C-10=%s NFR-10=%s, want both BLOCKED", rec.Get("C-10").Outcome, rec.Get("NFR-10").Outcome)
	}

	ev.Metrics["http://node-a"] = ParseMetrics("# TYPE up gauge\nup 1\n")
	rec = NewRecorder(AllRows())
	Evaluate(rec, ev)
	if rec.Get("C-10").Outcome != Pass || rec.Get("NFR-10").Outcome != Pass {
		t.Fatalf("live node read: C-10=%s NFR-10=%s, want both PASS (%s)", rec.Get("C-10").Outcome, rec.Get("NFR-10").Outcome, rec.Get("NFR-10").Observed)
	}

	ev.Metrics["http://node-a"] = ParseMetrics("etcd_server_has_leader 1\n")
	rec = NewRecorder(AllRows())
	Evaluate(rec, ev)
	if rec.Get("C-10").Outcome != Fail || rec.Get("NFR-10").Outcome != Fail {
		t.Fatalf("etcd metric served: C-10=%s NFR-10=%s, want both FAIL", rec.Get("C-10").Outcome, rec.Get("NFR-10").Outcome)
	}
}

func TestParseCompose(t *testing.T) {
	const text = `# a comment that mentions etcd and kafka must not count
x-common: &common
  image: engine:dev
  mem_limit: 300m
  environment:
    DATABASE_URL: postgres://pg:5432/db  # trailing comment about zookeeper
    LEASE_TTL: 10s

services:
  pg:
    image: pgvector/pgvector:pg16
    mem_limit: 400m
  node-a:
    <<: *common
  node-b:
    <<: *common
    mem_limit: 1g
`
	s := parseCompose(text)
	if len(s.Coordination) != 0 {
		t.Fatalf("comments were scanned: %v", s.Coordination)
	}
	got := map[string]int{}
	for _, svc := range s.Services {
		got[svc.Name] = svc.MemLimitMB
	}
	if got["pg"] != 400 || got["node-a"] != 300 || got["node-b"] != 1024 || len(got) != 3 {
		t.Fatalf("services = %v", got)
	}
	if s.LeaseTTL != "10s" || s.URLEnv["DATABASE_URL"] == "" {
		t.Fatalf("env not read: ttl=%q urls=%v", s.LeaseTTL, s.URLEnv)
	}

	bad := parseCompose("services:\n  broker:\n    image: bitnami/kafka:3\n")
	if len(bad.Coordination) != 1 {
		t.Fatalf("kafka image not flagged: %v", bad.Coordination)
	}

	mod := parseGoMod("module x\n\nrequire (\n\tgithub.com/jackc/pgx/v5 v5.0.0\n\tgo.etcd.io/etcd/client/v3 v3.5.0 // indirect\n)\nrequire github.com/nats-io/nats.go v1.0.0\n")
	if len(mod.Modules) != 3 || len(mod.Coordination) != 2 {
		t.Fatalf("go.mod scan: modules %v, coordination %v", mod.Modules, mod.Coordination)
	}
	if hits := coordinationTokens("signatures_total"); len(hits) != 0 {
		t.Fatalf("substring false positive: %v", hits)
	}
}

// TestParseRealComposeFile keeps the parser honest against the file the stack
// actually starts from. It reads a local file only, and skips if it is absent.
func TestParseRealComposeFile(t *testing.T) {
	s := ScanCompose(filepath.Join("..", "..", "deploy", "docker-compose.yml"))
	if !s.Read {
		t.Skip("deploy/docker-compose.yml not present: " + s.Err)
	}
	if len(s.Coordination) != 0 {
		t.Errorf("coordination product named in compose file: %v", s.Coordination)
	}
	var names []string
	for _, svc := range s.Services {
		names = append(names, svc.Name)
		if svc.MemLimitMB == 0 {
			t.Errorf("service %s has no mem_limit found", svc.Name)
		}
	}
	if !strings.Contains(strings.Join(names, ","), "postgres-leader") {
		t.Errorf("services = %v, want postgres-leader among them", names)
	}
}
