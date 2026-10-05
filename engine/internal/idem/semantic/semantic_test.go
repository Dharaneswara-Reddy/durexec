package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---- fakes ----------------------------------------------------------------

// memIntents is an in-memory core.IntentStore. No database, by design: the
// semantic verdict is pure logic over vectors, and anything that needs
// Postgres to test is testing the wrong thing.
type memIntents struct {
	rows []core.Intent
	err  error
}

func (m *memIntents) Put(_ context.Context, in core.Intent) error {
	if m.err != nil {
		return m.err
	}
	m.rows = append(m.rows, in)
	return nil
}

func (m *memIntents) Candidates(_ context.Context, shard core.ShardID, workflowID string, run core.RunID, activityType string) ([]core.Intent, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []core.Intent
	for _, r := range m.rows {
		if r.ShardID == shard && r.WorkflowID == workflowID && r.RunID == run && r.ActivityType == activityType {
			out = append(out, r)
		}
	}
	return out, nil
}

// downEmbedder stands in for a model server that is not answering.
type downEmbedder struct{}

func (downEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, core.ErrEmbeddingUnavailable
}
func (downEmbedder) Model() string { return "all-MiniLM-L6-v2" }
func (downEmbedder) Dim() int      { return 384 }

// brokenEmbedder fails in a way that is NOT "unavailable", to prove those two
// cases are handled differently.
type brokenEmbedder struct{}

func (brokenEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("malformed model response")
}
func (brokenEmbedder) Model() string { return "broken" }
func (brokenEmbedder) Dim() int      { return 384 }

// ---- IntentText -----------------------------------------------------------

func TestIntentText(t *testing.T) {
	tests := []struct {
		name     string
		activity string
		input    string
		want     string
	}{
		{"natural language argument", "search_flights", `{"q":"BLR to DEL next Friday"}`,
			`search_flights({"q":"BLR to DEL next Friday"})`},
		{"numeric argument", "charge_payment", `{"amount":450}`,
			`charge_payment({"amount":450})`},
		{"no arguments", "ping", ``, `ping(null)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IntentText(tc.activity, json.RawMessage(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("IntentText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIntentTextStable is the reason canonicalisation happens before
// embedding: key order and whitespace must never reach the model.
func TestIntentTextStable(t *testing.T) {
	a, err := IntentText("search_flights", json.RawMessage(`{"q":"BLR to DEL","limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := IntentText("search_flights", json.RawMessage("{\n \"limit\" : 5 ,\n \"q\" : \"BLR to DEL\"\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("intent text differed under reordering: %q vs %q", a, b)
	}
}

func TestIntentTextRejectsBadJSON(t *testing.T) {
	if _, err := IntentText("x", json.RawMessage(`{`)); err == nil {
		t.Fatal("expected an error for malformed input")
	}
}

// ---- Cosine ---------------------------------------------------------------

func TestCosine(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical vectors are 1", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"same direction different length is 1", []float32{1, 0}, []float32{7, 0}, 1},
		{"orthogonal vectors are 0", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite vectors are -1", []float32{1, 0}, []float32{-1, 0}, -1},
		{"zero vector is 0 and not NaN", []float32{0, 0, 0}, []float32{1, 2, 3}, 0},
		{"both zero is 0", []float32{0, 0}, []float32{0, 0}, 0},
		{"empty is 0", []float32{}, []float32{}, 0},
		{"nil is 0", nil, nil, 0},
		{"mismatched dimensions are 0", []float32{1, 0}, []float32{1, 0, 0}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Cosine(tc.a, tc.b)
			if math.IsNaN(got) {
				t.Fatalf("Cosine returned NaN; NaN poisons every threshold comparison")
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("Cosine = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCosineIsSymmetric(t *testing.T) {
	pairs := [][2][]float32{
		{{1, 2, 3}, {4, 5, 6}},
		{{0, 0, 0}, {4, 5, 6}},
		{{-1, 2, -3}, {3, -2, 1}},
	}
	for _, p := range pairs {
		ab, ba := Cosine(p[0], p[1]), Cosine(p[1], p[0])
		if math.Abs(ab-ba) > 1e-12 {
			t.Fatalf("Cosine is not symmetric: %v vs %v", ab, ba)
		}
	}
}

func TestCosineStaysInRange(t *testing.T) {
	// Floating-point error can push an identical pair a hair over 1.0, which
	// would land outside the band the thresholds were calibrated on.
	v := []float32{0.577350269, 0.577350269, 0.577350269}
	got := Cosine(v, v)
	if got > 1 || got < -1 {
		t.Fatalf("Cosine = %v, outside [-1, 1]", got)
	}
}

func TestBestMatchNoCandidates(t *testing.T) {
	idx, sim := BestMatch([]float32{1, 0}, nil)
	if idx != -1 || sim != 0 {
		t.Fatalf("BestMatch over nothing = (%d, %v), want (-1, 0)", idx, sim)
	}
}

func TestBestMatchPicksArgmax(t *testing.T) {
	idx, sim := BestMatch([]float32{1, 0}, [][]float32{
		{0, 1},     // 0.0
		{0.7, 0.7}, // ~0.707
		{1, 0},     // 1.0
		{-1, 0},    // -1.0
	})
	if idx != 2 {
		t.Fatalf("BestMatch index = %d, want 2", idx)
	}
	if math.Abs(sim-1) > 1e-9 {
		t.Fatalf("BestMatch similarity = %v, want 1", sim)
	}
}

// ---- thresholds -----------------------------------------------------------

// TestThresholdBoundaries pins the exact band edges the evaluation reports.
func TestThresholdBoundaries(t *testing.T) {
	th := core.DefaultThresholds()
	tests := []struct {
		sim  float64
		want core.DedupVerdict
	}{
		{1.00, core.VerdictDuplicate},
		{0.90, core.VerdictDuplicate},  // exactly TauHigh is inclusive
		{0.8999, core.VerdictEscalate}, // a hair below is uncertain
		{0.87, core.VerdictEscalate},
		{0.85, core.VerdictEscalate}, // exactly TauLow is still the band
		{0.8499, core.VerdictDistinct},
		{0.0, core.VerdictDistinct},
		{-1.0, core.VerdictDistinct},
	}
	for _, tc := range tests {
		if got := th.Classify(tc.sim); got != tc.want {
			t.Fatalf("Classify(%v) = %s, want %s", tc.sim, got, tc.want)
		}
	}
}

// ---- FakeEmbedder ---------------------------------------------------------

func TestFakeEmbedderIsDeterministicAndNormalised(t *testing.T) {
	f := NewFakeEmbedder(0)
	if f.Dim() != DefaultDim {
		t.Fatalf("Dim = %d, want %d", f.Dim(), DefaultDim)
	}

	a, err := f.Embed(context.Background(), "search_flights({\"q\":\"BLR to DEL\"})")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Embed(context.Background(), "search_flights({\"q\":\"BLR to DEL\"})")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != DefaultDim {
		t.Fatalf("vector has %d dimensions, want %d", len(a), DefaultDim)
	}
	if Cosine(a, b) < 1-1e-9 {
		t.Fatal("the same text must embed to the same vector")
	}

	var norm float64
	for _, v := range a {
		norm += float64(v) * float64(v)
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-5 {
		t.Fatalf("vector is not unit length: %v", math.Sqrt(norm))
	}

	c, err := f.Embed(context.Background(), "charge_payment({\"amount\":450})")
	if err != nil {
		t.Fatal(err)
	}
	// Hash-derived vectors of unrelated texts should be near-orthogonal, which
	// is what puts them safely under TauLow.
	if sim := Cosine(a, c); sim >= core.DefaultThresholds().TauLow {
		t.Fatalf("unrelated texts scored %v, which would be misread as a match", sim)
	}
}

// ---- vector literals ------------------------------------------------------

func TestVectorLiteralRoundTrip(t *testing.T) {
	in := []float32{0, 1, -1, 0.5, 0.123456}
	got, err := ParseVector(VectorLiteral(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("round trip changed length: %d vs %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("component %d changed: %v -> %v", i, in[i], got[i])
		}
	}
}

func TestVectorLiteralFormat(t *testing.T) {
	if got := VectorLiteral([]float32{1, 2, 3}); got != "[1,2,3]" {
		t.Fatalf("VectorLiteral = %q, want %q", got, "[1,2,3]")
	}
	if got := VectorLiteral(nil); got != "[]" {
		t.Fatalf("VectorLiteral(nil) = %q, want %q", got, "[]")
	}
}

func TestParseVectorErrors(t *testing.T) {
	for _, bad := range []string{"", "1,2,3", "[1,2", "[a,b]", "]["} {
		if _, err := ParseVector(bad); err == nil {
			t.Fatalf("ParseVector(%q) should have failed", bad)
		}
	}
	if v, err := ParseVector("[]"); err != nil || len(v) != 0 {
		t.Fatalf("ParseVector(\"[]\") = %v, %v", v, err)
	}
}

// ---- Checker --------------------------------------------------------------

// checkerFixture wires a checker to a fake embedder and an in-memory store,
// optionally pre-loading a prior invocation.
func checkerFixture(t *testing.T, priorText string, priorSeq core.Seq) (*Checker, *memIntents) {
	t.Helper()
	store := &memIntents{}
	emb := NewFakeEmbedder(0)
	if priorText != "" {
		vec, err := emb.Embed(context.Background(), priorText)
		if err != nil {
			t.Fatal(err)
		}
		store.rows = append(store.rows, core.Intent{
			ShardID:      7,
			WorkflowID:   "trip-42",
			RunID:        "run-1",
			ScheduledSeq: priorSeq,
			ActivityType: "search_flights",
			IntentText:   priorText,
			Embedding:    vec,
		})
	}
	return NewChecker(emb, store), store
}

func baseRequest(input string) CheckRequest {
	return CheckRequest{
		ShardID:      7,
		WorkflowID:   "trip-42",
		RunID:        "run-1",
		K:            4,
		ActivityType: "search_flights",
		Input:        json.RawMessage(input),
	}
}

// assertFullyPopulated is the audit requirement: every field a reader needs to
// re-derive the decision must be present on every verdict.
func assertFullyPopulated(t *testing.T, ev core.DedupEvaluated, wantK int) {
	t.Helper()
	if ev.K != wantK {
		t.Fatalf("K = %d, want %d", ev.K, wantK)
	}
	if ev.Verdict == "" {
		t.Fatal("Verdict is empty")
	}
	if ev.TauHigh != core.DefaultThresholds().TauHigh {
		t.Fatalf("TauHigh = %v, want %v", ev.TauHigh, core.DefaultThresholds().TauHigh)
	}
	if ev.TauLow != core.DefaultThresholds().TauLow {
		t.Fatalf("TauLow = %v, want %v", ev.TauLow, core.DefaultThresholds().TauLow)
	}
	if ev.Model == "" {
		t.Fatal("Model is empty; the verdict is not reproducible without it")
	}
	if ev.IntentText == "" {
		t.Fatal("IntentText is empty; the verdict is not explainable without it")
	}
}

func TestCheckIdenticalIntentIsDuplicate(t *testing.T) {
	const text = `search_flights({"q":"BLR to DEL next Friday"})`
	c, _ := checkerFixture(t, text, 11)
	// KBySeq maps the candidate's history position back to its program index.
	req := baseRequest(`{"q":"BLR to DEL next Friday"}`)
	req.KBySeq = map[core.Seq]int{11: 2}

	ev, err := c.Check(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertFullyPopulated(t, ev, 4)
	if ev.Verdict != core.VerdictDuplicate {
		t.Fatalf("Verdict = %s, want DUPLICATE", ev.Verdict)
	}
	if ev.MatchedK != 2 {
		t.Fatalf("MatchedK = %d, want 2", ev.MatchedK)
	}
	if ev.Similarity < 1-1e-9 {
		t.Fatalf("Similarity = %v, want 1", ev.Similarity)
	}
	if IsFallback(ev) {
		t.Fatal("a verdict from a working model must not be marked as a fallback")
	}
}

// TestCheckIgnoresKeyOrder proves the canonicalisation actually reaches the
// verdict: the same arguments spelled differently must still be a duplicate.
func TestCheckIgnoresKeyOrder(t *testing.T) {
	const text = `search_flights({"limit":5,"q":"BLR to DEL"})`
	c, _ := checkerFixture(t, text, 11)
	req := baseRequest(`{"q":"BLR to DEL","limit":5}`)

	ev, err := c.Check(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Verdict != core.VerdictDuplicate {
		t.Fatalf("Verdict = %s, want DUPLICATE", ev.Verdict)
	}
}

// TestCheckFallsBackToSeqWithoutMapping covers the gap in core: an Intent
// records ScheduledSeq while DedupEvaluated reports MatchedK, and with no
// mapping supplied the seq is reported as-is.
func TestCheckFallsBackToSeqWithoutMapping(t *testing.T) {
	const text = `search_flights({"q":"BLR to DEL next Friday"})`
	c, _ := checkerFixture(t, text, 11)

	ev, err := c.Check(context.Background(), baseRequest(`{"q":"BLR to DEL next Friday"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.MatchedK != 11 {
		t.Fatalf("MatchedK = %d, want the scheduled seq 11", ev.MatchedK)
	}
}

func TestCheckDifferentIntentIsDistinct(t *testing.T) {
	c, _ := checkerFixture(t, `search_flights({"q":"BLR to DEL next Friday"})`, 11)

	ev, err := c.Check(context.Background(), baseRequest(`{"q":"completely unrelated cargo booking"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertFullyPopulated(t, ev, 4)
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("Verdict = %s, want DISTINCT", ev.Verdict)
	}
	if ev.MatchedK != NoMatch {
		t.Fatalf("MatchedK = %d, want NoMatch (%d): a rejected match must not be named", ev.MatchedK, NoMatch)
	}
}

func TestCheckNoCandidatesIsDistinct(t *testing.T) {
	c, _ := checkerFixture(t, "", 0)

	ev, err := c.Check(context.Background(), baseRequest(`{"q":"BLR to DEL"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertFullyPopulated(t, ev, 4)
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("Verdict = %s, want DISTINCT", ev.Verdict)
	}
	if ev.MatchedK != NoMatch {
		t.Fatalf("MatchedK = %d, want NoMatch", ev.MatchedK)
	}
	if ev.Similarity != 0 {
		t.Fatalf("Similarity = %v, want 0", ev.Similarity)
	}
}

// TestCheckDifferentActivityTypeIsNotACandidate guards the scoping: a prior
// invocation of another activity must never be a match, however similar.
func TestCheckDifferentActivityTypeIsNotACandidate(t *testing.T) {
	c, store := checkerFixture(t, "", 0)
	vec, err := NewFakeEmbedder(0).Embed(context.Background(), `search_flights({"q":"BLR to DEL"})`)
	if err != nil {
		t.Fatal(err)
	}
	store.rows = append(store.rows, core.Intent{
		ShardID: 7, WorkflowID: "trip-42", RunID: "run-1", ScheduledSeq: 9,
		ActivityType: "cancel_flight", IntentText: `search_flights({"q":"BLR to DEL"})`, Embedding: vec,
	})

	ev, err := c.Check(context.Background(), baseRequest(`{"q":"BLR to DEL"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("Verdict = %s, want DISTINCT: a different activity type is not a candidate", ev.Verdict)
	}
}

// TestCheckDegradesWhenEmbedderUnavailable is the availability guarantee: a
// model that is down must never fail a workflow that has been running for
// weeks. The fallback is journaled so the log still explains itself.
func TestCheckDegradesWhenEmbedderUnavailable(t *testing.T) {
	c := NewChecker(downEmbedder{}, &memIntents{})

	ev, err := c.Check(context.Background(), baseRequest(`{"q":"BLR to DEL"}`))
	if err != nil {
		t.Fatalf("an unavailable model must not be an error: %v", err)
	}
	assertFullyPopulated(t, ev, 4)
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("Verdict = %s, want DISTINCT", ev.Verdict)
	}
	if !IsFallback(ev) {
		t.Fatalf("Model = %q, expected it to record that the fallback happened", ev.Model)
	}
	if ev.Model != FallbackPrefix+"all-MiniLM-L6-v2" {
		t.Fatalf("Model = %q, expected the intended model name to be preserved", ev.Model)
	}
}

func TestCheckDegradesWhenNoEmbedderConfigured(t *testing.T) {
	c := &Checker{Intents: &memIntents{}, Thresholds: core.DefaultThresholds()}

	ev, err := c.Check(context.Background(), baseRequest(`{"q":"BLR to DEL"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Verdict != core.VerdictDistinct || !IsFallback(ev) {
		t.Fatalf("got %s / model %q, want a DISTINCT fallback", ev.Verdict, ev.Model)
	}
}

// TestCheckPropagatesRealEmbedderErrors: only ErrEmbeddingUnavailable degrades.
// Anything else is a genuine bug and must surface rather than be swallowed.
func TestCheckPropagatesRealEmbedderErrors(t *testing.T) {
	c := NewChecker(brokenEmbedder{}, &memIntents{})
	if _, err := c.Check(context.Background(), baseRequest(`{"q":"x"}`)); err == nil {
		t.Fatal("an unexpected embedder error must not be silently degraded")
	}
}

func TestCheckPropagatesStoreErrors(t *testing.T) {
	c := NewChecker(NewFakeEmbedder(0), &memIntents{err: errors.New("connection refused")})
	if _, err := c.Check(context.Background(), baseRequest(`{"q":"x"}`)); err == nil {
		t.Fatal("an unreachable candidate store must be reported")
	}
}

func TestCheckRejectsMalformedInput(t *testing.T) {
	c, _ := checkerFixture(t, "", 0)
	req := baseRequest(`{`)
	if _, err := c.Check(context.Background(), req); err == nil {
		t.Fatal("malformed arguments must be rejected")
	}
}

// TestCheckHonoursPerRequestThresholds proves the thresholds actually used are
// the ones written into the event, so a later config change cannot rewrite the
// past.
func TestCheckHonoursPerRequestThresholds(t *testing.T) {
	c, _ := checkerFixture(t, `search_flights({"q":"BLR to DEL"})`, 11)
	req := baseRequest(`{"q":"BLR to DEL"}`)
	req.Thresholds = core.Thresholds{TauLow: 0.99, TauHigh: 0.999}

	ev, err := c.Check(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if ev.TauHigh != 0.999 || ev.TauLow != 0.99 {
		t.Fatalf("thresholds in the event are %v/%v, want the ones actually used", ev.TauLow, ev.TauHigh)
	}
	if ev.Verdict != core.VerdictDuplicate {
		t.Fatalf("Verdict = %s, want DUPLICATE at similarity 1.0", ev.Verdict)
	}
}

// recordingObserver captures the metric callback without a Prometheus registry.
type recordingObserver struct {
	calls []core.DedupVerdict
}

func (r *recordingObserver) ObserveDedup(_ string, v core.DedupVerdict, _ float64) {
	r.calls = append(r.calls, v)
}

func TestCheckObservesEveryVerdict(t *testing.T) {
	obs := &recordingObserver{}
	c, _ := checkerFixture(t, `search_flights({"q":"BLR to DEL"})`, 11)
	c.Observer = obs

	if _, err := c.Check(context.Background(), baseRequest(`{"q":"BLR to DEL"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background(), baseRequest(`{"q":"something else entirely"}`)); err != nil {
		t.Fatal(err)
	}
	if len(obs.calls) != 2 {
		t.Fatalf("observer saw %d verdicts, want 2", len(obs.calls))
	}
	if obs.calls[0] != core.VerdictDuplicate || obs.calls[1] != core.VerdictDistinct {
		t.Fatalf("observer saw %v", obs.calls)
	}
}

func TestCheckWithNilObserverDoesNotPanic(t *testing.T) {
	c, _ := checkerFixture(t, "", 0)
	if _, err := c.Check(context.Background(), baseRequest(`{"q":"x"}`)); err != nil {
		t.Fatal(err)
	}
}
