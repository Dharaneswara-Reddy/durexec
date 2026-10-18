package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/app/agent"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

// ---------------------------------------------------------------------------
// Controlled-similarity data.
// ---------------------------------------------------------------------------

// tableEmbedder returns a fixed vector for each known text and fails on any
// other. With it a test decides the exact similarity of every pair, so the
// sweep can be checked against numbers worked out by hand.
type tableEmbedder struct {
	vecs map[string][]float32
}

func (e tableEmbedder) Model() string { return "test-table" }
func (e tableEmbedder) Dim() int      { return 2 }
func (e tableEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v, ok := e.vecs[text]
	if !ok {
		return nil, fmt.Errorf("tableEmbedder: no vector for %q", text)
	}
	return v, nil
}

type synthPair struct {
	sim   float64
	truth Label
}

// The scores sit between grid points on purpose, so float32 rounding cannot
// move any of them across a threshold.
var synthetic = []synthPair{
	{0.975, Duplicate}, {0.925, Duplicate}, {0.875, Duplicate}, {0.805, Duplicate}, {0.725, Duplicate},
	{0.915, Distinct}, {0.845, Distinct}, {0.755, Distinct}, {0.500, Distinct}, {0.100, Distinct},
}

// syntheticRun builds pairs and an embedder that realise the scores above:
// side A of every pair points along x, side B is rotated away by the angle
// whose cosine is the pair's target similarity.
func syntheticRun(t *testing.T, cfg config) ([]Pair, core.Embedder) {
	t.Helper()
	emb := tableEmbedder{vecs: map[string][]float32{}}
	var pairs []Pair
	for i, sp := range synthetic {
		p := Pair{
			ID:       fmt.Sprintf("syn-%02d", i),
			Category: "paraphrase",
			A:        fmt.Sprintf("request alpha %d", i),
			B:        fmt.Sprintf("request beta %d", i),
			Truth:    sp.truth,
		}
		for side, vec := range map[string][]float32{
			p.A: {1, 0},
			p.B: {float32(sp.sim), float32(math.Sqrt(1 - sp.sim*sp.sim))},
		} {
			in, err := toolCallJSON(cfg.activityType, side)
			if err != nil {
				t.Fatal(err)
			}
			text, err := semantic.IntentText(cfg.activityType, in)
			if err != nil {
				t.Fatal(err)
			}
			emb.vecs[text] = vec
		}
		pairs = append(pairs, p)
	}
	return pairs, emb
}

func syntheticConfig() config {
	return config{
		activityType: agent.ActivityBookFlight, repeat: 1,
		tauMin: 0.70, tauMax: 0.99, tauStep: 0.01, bandWidth: 0.05,
		tauLow: 0.85, tauHigh: 0.90, retries: 1,
	}
}

// TestSweepOnControlledSimilarities runs the real pipeline — observe, the
// cross-check against the checker, and score — over pairs whose similarities
// are known, and checks the confusion matrix at several points of the sweep
// against counts worked out by hand.
func TestSweepOnControlledSimilarities(t *testing.T) {
	cfg := syntheticConfig()
	pairs, emb := syntheticRun(t, cfg)
	st := newRunStatus()

	obs, err := observe(context.Background(), cfg, pairs, emb, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.failures) != 0 || len(st.fallbacks) != 0 {
		t.Fatalf("a clean synthetic run recorded failures %v / fallbacks %v", st.failures, st.fallbacks)
	}
	for i, o := range obs {
		if math.Abs(o.Similarity-synthetic[i].sim) > 1e-4 {
			t.Fatalf("pair %d measured %.6f, want %.3f", i, o.Similarity, synthetic[i].sim)
		}
	}
	if err := verifyClassification(obs, core.Thresholds{TauLow: 0.85, TauHigh: 0.90}); err != nil {
		t.Fatal(err)
	}

	arms, err := buildArms(cfg)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*armResult{}
	var single []*armResult
	for _, a := range arms {
		r, _ := score(a, obs)
		byName[a.Name] = r
		if a.Series == seriesSingle {
			single = append(single, r)
		}
	}

	type want struct {
		tp, fp, tn, fn, esc int
		defined             bool
	}
	for name, w := range map[string]want{
		// The pairs are all reworded, so byte comparison catches none of them.
		"exact":             {tp: 0, fp: 0, tn: 5, fn: 5},
		"semantic tau=0.70": {tp: 5, fp: 3, tn: 2, fn: 0, defined: true},
		"semantic tau=0.90": {tp: 2, fp: 1, tn: 4, fn: 3, defined: true},
		"semantic tau=0.95": {tp: 1, fp: 0, tn: 5, fn: 4, defined: true},
		// Nothing scores 0.99, so this arm never says DUPLICATE.
		"semantic tau=0.99": {tp: 0, fp: 0, tn: 5, fn: 5},
		// The shipped band: 0.875 is escalated rather than decided.
		"semantic escalate [0.85,0.90)": {tp: 2, fp: 1, tn: 4, fn: 3, esc: 1, defined: true},
	} {
		r := byName[name]
		if r == nil {
			t.Fatalf("no arm named %q", name)
		}
		got := want{r.TruePositives, r.FalsePositives, r.TrueNegatives, r.FalseNegatives,
			r.EscalatedDuplicates + r.EscalatedDistincts, r.PrecisionIsDefined()}
		if got != w {
			t.Errorf("%s: got %+v, want %+v", name, got, w)
		}
	}
	checkFloat(t, "precision at 0.90", byName["semantic tau=0.90"].Precision(), 2.0/3.0)
	checkFloat(t, "recall at 0.90", byName["semantic tau=0.90"].Recall(), 0.4)
	if !byName["semantic escalate [0.85,0.90)"].Arm.OperatingPoint {
		t.Error("the shipped band is not marked as the operating point")
	}

	// Raising the threshold can only ever lose catches.
	for i := 1; i < len(single); i++ {
		if single[i].Recall() > single[i-1].Recall()+1e-12 {
			t.Fatalf("recall rose from %.3f at %s to %.3f at %s",
				single[i-1].Recall(), single[i-1].Arm.Name, single[i].Recall(), single[i].Arm.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// The cache.
// ---------------------------------------------------------------------------

// countingEmbedder counts calls so a test can see whether the cache answered.
type countingEmbedder struct {
	model string
	dim   int
	mu    sync.Mutex
	calls int
}

func (e *countingEmbedder) Model() string { return e.model }
func (e *countingEmbedder) Dim() int      { return e.dim }
func (e *countingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	v := make([]float32, e.dim)
	for i := range v {
		v[i] = float32(len(text)+i) / 7
	}
	return v, nil
}

func TestCacheHitAndMiss(t *testing.T) {
	dir := t.TempDir()
	cache, err := newEmbedCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	inner := &countingEmbedder{model: "model-a", dim: 4}
	e := newCachingEmbedder(inner, cache)
	ctx := context.Background()

	first, err := e.Embed(ctx, "book BLR to DEL")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Embed(ctx, "book BLR to DEL")
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("the encoder was called %d times for one text, want 1 (the second must be a hit)", inner.calls)
	}
	for i := range first {
		if math.Float32bits(first[i]) != math.Float32bits(second[i]) {
			t.Fatalf("component %d came back as %v, stored %v; a cached vector must be bit-identical",
				i, second[i], first[i])
		}
	}

	if _, err := e.Embed(ctx, "book BLR to BOM"); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 2 {
		t.Fatalf("a new text was served from the cache; encoder calls = %d, want 2", inner.calls)
	}

	s := cache.Stats()
	if s.Hits != 1 || s.Misses != 2 || s.Writes != 2 || s.Errors != 0 {
		t.Fatalf("cache stats %+v, want 1 hit, 2 misses, 2 writes, 0 errors", s)
	}
	if n := len(e.MissLatencies()); n != 2 {
		t.Fatalf("recorded %d real round-trips, want 2", n)
	}

	// A fresh process over the same directory is served entirely from disk.
	cache2, _ := newEmbedCache(dir)
	inner2 := &countingEmbedder{model: "model-a", dim: 4}
	if _, err := newCachingEmbedder(inner2, cache2).Embed(ctx, "book BLR to DEL"); err != nil {
		t.Fatal(err)
	}
	if inner2.calls != 0 {
		t.Fatal("a second run over the same cache directory re-embedded a cached text")
	}
}

// TestCacheNeverCrossesModels is the property that makes the cache safe: a
// vector computed by one encoder must never be handed back for another.
func TestCacheNeverCrossesModels(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	text := "the same sentence"

	cacheA, _ := newEmbedCache(dir)
	a := &countingEmbedder{model: "all-MiniLM-L6-v2", dim: 4}
	if _, err := newCachingEmbedder(a, cacheA).Embed(ctx, text); err != nil {
		t.Fatal(err)
	}

	cacheB, _ := newEmbedCache(dir)
	b := &countingEmbedder{model: "all-mpnet-base-v2", dim: 4}
	if _, err := newCachingEmbedder(b, cacheB).Embed(ctx, text); err != nil {
		t.Fatal(err)
	}
	if b.calls != 1 {
		t.Fatal("a different model name was served a vector cached under another model")
	}
	if s := cacheB.Stats(); s.Hits != 0 || s.Misses != 1 {
		t.Fatalf("changed-model lookup stats %+v, want a clean miss", s)
	}

	// Even a file planted at the other model's path is refused, because the
	// model name stored inside the entry is checked on the way out.
	pathA := cacheA.path("all-MiniLM-L6-v2", text)
	pathB := cacheB.path("all-mpnet-base-v2", text)
	raw, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheB.Get("all-mpnet-base-v2", text, 4); ok {
		t.Fatal("an entry written for one model was accepted under another model's key")
	}
}

func TestCacheTreatsDamageAsAMiss(t *testing.T) {
	dir := t.TempDir()
	cache, _ := newEmbedCache(dir)
	cache.Put("m", "t", []float32{1, 2, 3})

	if _, ok := cache.Get("m", "t", 4); ok {
		t.Fatal("a vector of the wrong width was served")
	}
	if err := os.WriteFile(cache.path("m", "t"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get("m", "t", 3); ok {
		t.Fatal("a corrupt entry was served")
	}
	if s := cache.Stats(); s.Errors < 2 {
		t.Fatalf("unusable entries were not counted: %+v", s)
	}

	off, _ := newEmbedCache("")
	off.Put("m", "t", []float32{1})
	if _, ok := off.Get("m", "t", 1); ok || off.Enabled() {
		t.Fatal("an empty cache directory must disable the cache")
	}
}

// ---------------------------------------------------------------------------
// Failure handling.
// ---------------------------------------------------------------------------

// flakyEmbedder fails the first n calls with the retryable error, then works.
// With n < 0 it never works.
type flakyEmbedder struct {
	n     int
	calls int
}

func (e *flakyEmbedder) Model() string { return "flaky" }
func (e *flakyEmbedder) Dim() int      { return 2 }
func (e *flakyEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.calls++
	if e.n < 0 || e.calls <= e.n {
		return nil, fmt.Errorf("%w: connection refused", core.ErrEmbeddingUnavailable)
	}
	return []float32{1, 0}, nil
}

func noSleep(r *retryEmbedder) *retryEmbedder { r.sleep = func(time.Duration) {}; return r }

func TestRetryRecoversFromTransientFailure(t *testing.T) {
	inner := &flakyEmbedder{n: 2}
	r := noSleep(newRetryEmbedder(inner, 4, time.Millisecond))
	if _, err := r.Embed(context.Background(), "x"); err != nil {
		t.Fatalf("two transient failures inside a four-attempt budget still failed: %v", err)
	}
	retries, failures := r.Counters()
	if retries != 2 || failures != 0 {
		t.Fatalf("retries=%d failures=%d, want 2 and 0", retries, failures)
	}
}

func TestRetryDoesNotRetryAPermanentError(t *testing.T) {
	calls := 0
	perm := embedFunc(func() error { calls++; return errors.New("bad request") })
	r := noSleep(newRetryEmbedder(perm, 4, time.Millisecond))
	if _, err := r.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("a non-transient error was attempted %d times, want 1", calls)
	}
}

type embedFunc func() error

func (f embedFunc) Model() string { return "func" }
func (f embedFunc) Dim() int      { return 2 }
func (f embedFunc) Embed(context.Context, string) ([]float32, error) {
	return nil, f()
}

// TestFailingEmbedderMarksTheRunIncomplete is requirement five: an encoder that
// never answers must produce a run stamped INCOMPLETE, never a clean-looking
// run with zero recall.
func TestFailingEmbedderMarksTheRunIncomplete(t *testing.T) {
	cfg := syntheticConfig()
	pairs, _ := syntheticRun(t, cfg)
	st := newRunStatus()
	dead := &flakyEmbedder{n: -1}
	emb := noSleep(newRetryEmbedder(dead, 3, time.Millisecond))
	st.retrying = emb

	obs, err := observe(context.Background(), cfg, pairs, emb, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if !o.Failed {
			t.Fatalf("pair %s was reported as measured by an encoder that never answered", o.Pair.ID)
		}
	}

	prov := &provenance{Retries: 3}
	prov.finish(st)
	if !prov.Incomplete() {
		t.Fatal("a run in which every embedding failed is not marked incomplete")
	}
	if len(prov.FailedPairs) != len(pairs) {
		t.Fatalf("%d failed pairs recorded, want %d", len(prov.FailedPairs), len(pairs))
	}
	if !strings.Contains(prov.Verdict(), "INCOMPLETE") {
		t.Fatalf("verdict %q does not say INCOMPLETE", prov.Verdict())
	}
	if prov.SemanticNumbersAreReal() {
		t.Fatal("an incomplete run claims its semantic numbers are real")
	}
	if prov.EmbedRetries == 0 {
		t.Fatal("no retries were spent before giving up")
	}
	if !strings.HasPrefix(completeColumn(prov), "no") {
		t.Fatalf("the CSV completeness column reads %q", completeColumn(prov))
	}
}

// TestHTTPProbeFailsLoudly checks that an unreachable service stops the run
// before anything is measured. The server is a local httptest one, closed
// before use, so nothing leaves the machine.
func TestHTTPProbeFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/embed"
	srv.Close()

	cfg := config{embedderKind: "http", endpoint: url, model: "m", dim: 4,
		timeout: time.Second, cacheDir: t.TempDir(), retries: 1}
	_, _, err := buildEmbedder(cfg, newRunStatus())
	if err == nil {
		t.Fatal("an unreachable endpoint was accepted")
	}
	if !strings.Contains(err.Error(), "did not answer a probe") || !strings.Contains(err.Error(), url) {
		t.Fatalf("the error does not name the probe and the URL: %v", err)
	}
}

// fakeEncoderServer speaks the embedder service's wire format, answering with
// the FakeEmbedder's vectors. It exercises the real HTTP client path.
func fakeEncoderServer(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	fake := semantic.NewFakeEmbedder(dim)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v, _ := fake.Embed(r.Context(), req.Text)
		_ = json.NewEncoder(w).Encode(map[string][]float32{"embedding": v})
	}))
}

// ---------------------------------------------------------------------------
// End to end and the HTML report.
// ---------------------------------------------------------------------------

func runToDir(t *testing.T, cfg config) (string, error) {
	t.Helper()
	dir := t.TempDir()
	cfg.csvPath = filepath.Join(dir, "summary.csv")
	cfg.pairsCSVPath = filepath.Join(dir, "pairs.csv")
	cfg.htmlPath = filepath.Join(dir, "report.html")
	cfg.cacheDir = filepath.Join(dir, "cache")
	cfg.activityType = agent.ActivityBookFlight
	cfg.tauMin, cfg.tauMax, cfg.tauStep, cfg.bandWidth = 0.70, 0.99, 0.01, 0.05
	d := core.DefaultThresholds()
	cfg.tauLow, cfg.tauHigh = d.TauLow, d.TauHigh
	cfg.repeat, cfg.retries, cfg.retryBase = 1, 2, time.Millisecond
	cfg.timeout = 2 * time.Second
	return dir, run(cfg)
}

func TestRunEndToEndOverHTTP(t *testing.T) {
	srv := fakeEncoderServer(t, 16)
	defer srv.Close()
	dir, err := runToDir(t, config{embedderKind: "http", endpoint: srv.URL + "/embed",
		model: "loopback-test-model", dim: 16})
	if err != nil {
		t.Fatalf("a run against a working HTTP encoder failed: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(dir, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "MEASURED AGAINST A REAL ENCODER") ||
		!strings.Contains(string(page), "loopback-test-model") {
		t.Fatal("an HTTP run's report does not name its model and verdict")
	}
}

func TestRunEndToEndWithFakeEmbedderWritesEveryOutput(t *testing.T) {
	dir, err := runToDir(t, config{embedderKind: "fake", dim: 64})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"summary.csv", "pairs.csv", "report.html"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Size() == 0 {
			t.Fatalf("%s was not written: %v", name, err)
		}
	}

	f, err := os.Open(filepath.Join(dir, "summary.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1+1+30+30 {
		t.Fatalf("the summary CSV has %d rows, want a header plus 61 arms", len(rows))
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	for _, r := range rows[1:] {
		if !strings.HasPrefix(r[col["labels_are_hand_labelled"]], "no - generated by construction") {
			t.Fatalf("row %s does not say its labels were generated", r[0])
		}
		if !strings.HasPrefix(r[col["embedder_is_real"]], "no - FakeEmbedder") {
			t.Fatalf("row %s does not say its embedder was fake", r[0])
		}
		if r[col["precision_defined"]] == "false" && r[col["precision"]] != "" {
			t.Fatalf("row %s has undefined precision but prints %q", r[0], r[col["precision"]])
		}
	}
}

func fakeProvenance() *provenance {
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	return &provenance{
		StartedAt: start, FinishedAt: start.Add(3 * time.Second),
		EmbedderKind: "fake", Fake: true,
		EmbedderLabel: "FakeEmbedder (sha256 pseudo-embeddings, dim=64)",
		Model:         "fake-hash-all-MiniLM-L6-v2", Dim: 64,
		Corpus: CorpusSummary{Total: 10, Duplicates: 5, Distincts: 5, ByCategory: map[string]int{"paraphrase": 10}},
		TauMin: 0.70, TauMax: 0.99, TauStep: 0.01, BandWidth: 0.05,
		Repeat: 1, CommandLine: "experiment -embedder=fake",
	}
}

// syntheticResults scores the controlled pairs, for report tests that need
// realistic arms including one with undefined precision.
func syntheticResults(t *testing.T) ([]*armResult, []perPairRow) {
	t.Helper()
	cfg := syntheticConfig()
	pairs, emb := syntheticRun(t, cfg)
	obs, err := observe(context.Background(), cfg, pairs, emb, newRunStatus())
	if err != nil {
		t.Fatal(err)
	}
	arms, err := buildArms(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var results []*armResult
	var rows []perPairRow
	for _, a := range arms {
		r, pr := score(a, obs)
		results = append(results, r)
		rows = append(rows, pr...)
	}
	return results, rows
}

func TestHTMLReportLeadsWithProvenance(t *testing.T) {
	results, rows := syntheticResults(t)
	page := renderHTML(results, rows, fakeProvenance())

	marker := strings.Index(page, ProvenanceMarker)
	hero := strings.Index(page, `class="hero-figure`)
	chart := strings.Index(page, "<svg")
	if marker < 0 {
		t.Fatal("the report has no provenance block")
	}
	if hero < 0 || chart < 0 || marker > hero || marker > chart {
		t.Fatalf("the provenance block (at %d) must come before the headline (%d) and the chart (%d)",
			marker, hero, chart)
	}
	top := page[marker:hero]
	for _, want := range []string{
		"SMOKE TEST", "FakeEmbedder", "2026-09-21", ">10<",
		"generated by construction", "hand-labelled",
	} {
		if !strings.Contains(top, want) {
			t.Errorf("the provenance block does not contain %q", want)
		}
	}
	if !strings.Contains(page, `hero-figure provisional`) {
		t.Error("a fake-embedder headline is not marked provisional")
	}
}

func TestHTMLReportHasNoExternalReferences(t *testing.T) {
	results, rows := syntheticResults(t)
	page := strings.ToLower(renderHTML(results, rows, fakeProvenance()))
	for _, bad := range []string{
		"http://", "https://", "//cdn", "<script", "<link", "<img", "<iframe",
		"src=", "href=", "url(", "@import", "@font-face",
	} {
		if strings.Contains(page, bad) {
			i := strings.Index(page, bad)
			t.Errorf("the report contains %q, near %q", bad, page[max(0, i-40):min(len(page), i+40)])
		}
	}
	if !strings.HasPrefix(page, "<!doctype html>") || !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Error("the report is not a complete standalone HTML document")
	}
}

func TestHTMLReportWithholdsTheHeadlineWhenIncomplete(t *testing.T) {
	results, rows := syntheticResults(t)
	prov := fakeProvenance()
	prov.Fake, prov.EmbedderKind, prov.Model = false, "http", "all-MiniLM-L6-v2"
	prov.FailedPairs = []string{"syn-03"}
	page := renderHTML(results, rows, prov)

	if !strings.Contains(page, "INCOMPLETE RUN") {
		t.Fatal("an incomplete run's report does not say so")
	}
	if !strings.Contains(page, "hero-figure withheld") {
		t.Fatal("an incomplete run still published a headline figure")
	}
	if regexp.MustCompile(`class="hero-figure[^"]*">[0-9.]+%`).MatchString(page) {
		t.Fatal("an incomplete run printed a headline percentage")
	}
}

// ---------------------------------------------------------------------------
// Undefined precision.
// ---------------------------------------------------------------------------

// TestUndefinedPrecisionIsNeitherPlottedNorPrintedAsZero covers an arm that
// never said DUPLICATE. Its precision is 0/0. It must not appear on the chart
// as a point at precision 0, and the table must say "undefined" rather than
// print a number.
func TestUndefinedPrecisionIsNeitherPlottedNorPrintedAsZero(t *testing.T) {
	silent := newArmResult(arm{Name: "semantic tau=0.99", Kind: kindSemantic, Series: seriesSingle,
		Thresholds: core.Thresholds{TauLow: 0.99, TauHigh: 0.99}}, 4)
	silent.record(Pair{Truth: Duplicate}, outcome{Verdict: core.VerdictDistinct})
	silent.record(Pair{Truth: Duplicate}, outcome{Verdict: core.VerdictEscalate})
	silent.record(Pair{Truth: Distinct}, outcome{Verdict: core.VerdictDistinct})
	silent.record(Pair{Truth: Distinct}, outcome{Verdict: core.VerdictEscalate})

	if silent.MadePositivePredictions() || silent.PrecisionIsDefined() {
		t.Fatal("an arm that only said DISTINCT and ESCALATE is treated as having predicted a duplicate")
	}
	if silent.Precision() != 0 || math.IsNaN(silent.Precision()) {
		t.Fatalf("Precision() = %v, want the 0 placeholder", silent.Precision())
	}

	speaking := newArmResult(arm{Name: "semantic tau=0.80", Kind: kindSemantic, Series: seriesSingle,
		Thresholds: core.Thresholds{TauLow: 0.80, TauHigh: 0.80}}, 2)
	speaking.record(Pair{Truth: Duplicate}, outcome{Verdict: core.VerdictDuplicate})
	speaking.record(Pair{Truth: Distinct}, outcome{Verdict: core.VerdictDistinct})

	results := []*armResult{speaking, silent}
	single, _, _, _ := buildSeries(results)
	if len(single.Points) != 2 || !single.Points[0].Plotted || single.Points[1].Plotted {
		t.Fatalf("plotted flags %+v, want the silent arm left off the line", single.Points)
	}

	svg := renderCurve(results)
	if n := strings.Count(svg, "<circle"); n != 1 {
		t.Fatalf("the chart draws %d points, want 1 (the silent arm must not be drawn)", n)
	}
	floor := fmt.Sprintf(`cy="%.2f"`, py(0))
	if strings.Contains(svg, "<circle") && strings.Contains(svg, floor) {
		t.Fatal("a point was drawn on the precision-zero floor")
	}

	page := renderHTML(results, nil, fakeProvenance())
	if !strings.Contains(page, `class="num undef"`) || !strings.Contains(page, ">undefined<") {
		t.Fatal("the HTML table does not mark the silent arm's precision as undefined")
	}
	if !strings.Contains(page, "1 arm(s) are missing from the line") {
		t.Fatal("the caption does not name the arm left off the chart")
	}
	if got := precisionText(silent); got != "undef" {
		t.Fatalf("terminal precision for the silent arm is %q, want undef", got)
	}
	if got := precisionCSV(silent); got != "" {
		t.Fatalf("CSV precision for the silent arm is %q, want an empty cell", got)
	}
}
