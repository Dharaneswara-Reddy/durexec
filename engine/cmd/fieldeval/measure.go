package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/app/agent"
	"github.com/dharaneswar339/durexec/app/agent/llm"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/exact"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

// ---------------------------------------------------------------------------
// ONE MEASUREMENT, THROUGH THE ENGINE'S OWN CODE
// ---------------------------------------------------------------------------
//
// Every verdict here comes from semantic.Checker.Check — the function the
// engine calls on the live path — with the arm's extractor and gate rule
// configured on it, and from exact.Key for the baseline. Nothing is
// reimplemented. A harness that reimplemented the gate would be measuring
// itself, and the existing experiment harness says so in the same words.
//
// Side A is embedded and journaled OUTSIDE the timer, because in the engine
// that work happened when A was first scheduled, long before the crash. The
// timed section is exactly what a retry adds: render the intent, extract its
// fields, load the candidates, compare.

// measurer holds everything a run needs: the embedder stack, the LLM
// extractor and its on-disk reply cache.
type measurer struct {
	cfg  config
	emb  core.Embedder
	prov *provenance

	HaveLLM  bool
	LLMModel string

	llm      *llm.FieldExtractor
	llmCache *replyCache
	limiter  *limiter

	embedStats *latencyLog

	// extractFailed is set when the extraction model did not answer for the
	// row being measured.
	//
	// It exists because the failure cannot be returned: semantic.Checker
	// treats an extractor error as "this intent could not be read" and falls
	// back to the pure-semantic path, which is correct behaviour for the
	// ENGINE — a model outage must not fail a workflow — and wrong for a
	// MEASUREMENT, where it would record a provider rate-limit as a property
	// of the extractor. The flag lets Measure turn it into an ERROR row
	// instead, which a resume retries.
	extractFailed atomic.Bool
}

func newMeasurer(ctx context.Context, cfg config, prov *provenance) (*measurer, error) {
	m := &measurer{cfg: cfg, prov: prov, embedStats: &latencyLog{}}

	emb, err := buildEmbedder(ctx, cfg, m.embedStats)
	if err != nil {
		return nil, err
	}
	m.emb = emb

	if cfg.withLLM {
		ex := llm.NewFieldExtractor()
		if ex == nil {
			// Loud, and not fatal: the deterministic arms are the bulk of the
			// result and must still be measurable on a machine with no key.
			fmt.Fprintln(os.Stderr,
				"fieldeval: GROQ_API_KEY is not set, so the LLM extractor arms are SKIPPED. "+
					"They are reported as skipped, never as a zero score. "+
					"Load the key with: set -a; . ./.env; set +a")
		} else {
			cache, err := newReplyCache(cfg.llmCache)
			if err != nil {
				return nil, err
			}
			m.llm, m.llmCache, m.HaveLLM = ex, cache, true
			m.LLMModel = ex.Model()
			m.limiter = newLimiter(cfg.rate)
		}
	}
	return m, nil
}

func (m *measurer) Close() error {
	if m.llmCache != nil {
		return m.llmCache.Flush()
	}
	return nil
}

func (m *measurer) LLMCalls() int64 {
	if m.llm == nil {
		return 0
	}
	return m.llm.Calls.Load()
}

func (m *measurer) LLMFailures() int64 {
	if m.llm == nil {
		return 0
	}
	return m.llm.Failures.Load()
}

func (m *measurer) LLMNanos() int64 {
	if m.llm == nil {
		return 0
	}
	return m.llm.Nanos.Load()
}

func (m *measurer) EmbedStats() (int, time.Duration, time.Duration) {
	return m.embedStats.Summary()
}

// intentFor renders one side of a pair as the intent text the engine would
// journal.
//
// On the engine's real path that goes through agent.CanonicalIntent, which
// lowercases and collapses whitespace — so the text any extractor sees has no
// capitals left. The case-preserved arm skips only that lowercasing, keeping
// everything else identical, which isolates one decision instead of changing
// two things at once.
func intentFor(cfg config, a arm, activityType, side string) (json.RawMessage, string, error) {
	args := agent.CanonicalIntent(side)
	if a.CasePreserved {
		args = strings.Join(strings.Fields(side), " ")
	}
	raw, err := json.Marshal(agent.ToolCall{Tool: activityType, Args: args})
	if err != nil {
		return nil, "", err
	}
	if a.Render == renderEnvelope || (a.Render == "" && cfg.render == renderEnvelope) {
		text, err := semantic.IntentText(activityType, raw)
		if err != nil {
			return nil, "", err
		}
		return raw, text, nil
	}
	// The engine's own text for these activities: app/agent's toolCallIntent
	// returns CanonicalIntent(args) and nothing else, and cmd/engined hands
	// that to Checker.RenderIntent. Reproducing it here rather than
	// reimplementing it would be better still, but toolCallIntent is
	// unexported; CanonicalIntent is the whole of its body and is exported,
	// so this is the same string by construction.
	if args == "" {
		return nil, "", fmt.Errorf("pair side has no text to compare")
	}
	return raw, args, nil
}

// Measure produces one CSV row: one comparator's answer about one pair.
func (m *measurer) Measure(ctx context.Context, a arm, ep evalPair) (row, error) {
	p := ep.Pair
	activity := p.Activity
	if activity == "" {
		activity = agent.ActivityBookFlight
	}

	r := row{
		Corpus:        ep.Corpus,
		Domain:        p.Domain,
		Activity:      activity,
		PairID:        p.ID,
		Category:      p.Category,
		Hard:          p.Hard,
		Truth:         bool(p.Truth),
		Comparator:    a.Name,
		Extractor:     a.Extractor,
		GateMode:      a.GateMode.String(),
		MinAgreeing:   a.MinAgreeing,
		CasePreserved: a.CasePreserved,
		GateDecides:   a.GateDecides,
		Render:        a.renderWith(m.cfg),
	}

	m.extractFailed.Store(false)

	inputA, textA, err := intentFor(m.cfg, a, activity, p.A)
	if err != nil {
		return r, err
	}
	inputB, textB, err := intentFor(m.cfg, a, activity, p.B)
	if err != nil {
		return r, err
	}

	if a.Kind == kindExact {
		keyA, err := exact.Key(activity, inputA)
		if err != nil {
			return r, err
		}
		start := time.Now()
		keyB, err := exact.Key(activity, inputB)
		if err != nil {
			return r, err
		}
		hit := keyA == keyB
		r.LatencyUS = float64(time.Since(start).Microseconds())
		r.ExactHit = hit
		// The exact baseline has no score and no gate. Recording a similarity
		// of 1 for a hit and 0 otherwise is not a measurement and is not
		// used as one: scoreRow reads ExactHit, not Similarity.
		if hit {
			r.Similarity = 1
		}
		return r, nil
	}

	// Both the semantic baseline and every hybrid arm run through the real
	// checker. The only difference is whether Fields is set on it.
	store := newMemIntents()
	vecA, err := m.emb.Embed(ctx, textA)
	if err != nil {
		return r, fmt.Errorf("embed the recorded side: %w", err)
	}
	const (
		shard = core.ShardID(0)
		runID = core.RunID("run-1")
		seqA  = core.Seq(1)
	)
	wf := a.Name + "-" + p.ID
	if err := store.Put(ctx, core.Intent{
		ShardID: shard, WorkflowID: wf, RunID: runID,
		ScheduledSeq: seqA, ActivityType: activity,
		IntentText: textA, Embedding: vecA,
	}); err != nil {
		return r, err
	}

	th := core.Thresholds{TauLow: m.cfg.tauLow, TauHigh: m.cfg.tauHigh}
	checker := semantic.NewChecker(m.emb, store)
	checker.Thresholds = th
	// The checker renders the QUERY's intent itself, so it has to be given
	// the same renderer the harness used for the stored side. Without this
	// the two sides would be rendered differently and every similarity would
	// be a comparison between a sentence and an envelope.
	checker.RenderIntent = func(at string, in json.RawMessage) (string, error) {
		_, text, err := intentFor(m.cfg, a, at, argsOf(in))
		return text, err
	}
	if a.Kind == kindHybrid {
		fields, err := m.extractorFor(a)
		if err != nil {
			return r, err
		}
		checker.Fields = fields
		checker.GateMode = a.GateMode
		checker.MinAgreeing = a.MinAgreeing
		checker.GateDecides = a.GateDecides
		checker.GateOnly = a.GateOnly
	}

	// Timed repeats. The extraction and the embeddings are warm after the
	// first, so these timings are the engine's check path with the model
	// round-trip served from cache — stated in the report, because a p50 that
	// silently included a network call would describe nothing.
	var best time.Duration
	var ev core.DedupEvaluated
	repeat := m.cfg.repeat
	if repeat < 1 {
		repeat = 1
	}
	for i := 0; i < repeat; i++ {
		start := time.Now()
		out, err := checker.Check(ctx, semantic.CheckRequest{
			ShardID:      shard,
			WorkflowID:   wf,
			RunID:        runID,
			K:            1,
			ActivityType: activity,
			Input:        inputB,
			Thresholds:   th,
			KBySeq:       map[core.Seq]int{seqA: 0},
		})
		elapsed := time.Since(start)
		if err != nil {
			return r, fmt.Errorf("check: %w", err)
		}
		if i == 0 || elapsed < best {
			best = elapsed
		}
		ev = out
	}
	r.LatencyUS = float64(best.Microseconds())
	r.CheckerVerdict = string(ev.Verdict)
	r.Fallback = semantic.IsFallback(ev)
	r.Similarity = ev.Similarity
	r.Detail = ev.Detail

	if a.Kind == kindSemantic {
		// The pure path consults the model on every pair by construction, so
		// there is nothing to gate and nothing to attribute.
		r.ModelUsed = !r.Fallback
		r.RecognisedA, r.RecognisedB = true, true
		return r, nil
	}

	// The threshold-free halves of the hybrid answer, taken outside the timed
	// section: whether the gate closed, on what, how much the two sides
	// agreed on, and what the free text scored. None of them move when a
	// threshold moves, which is what licenses sweeping thresholds at scoring
	// time. B is the call being made and A the candidate it might repeat,
	// which is the direction the engine asks in.
	cmp, err := checker.Compare(ctx, activity, textB, textA)
	if err != nil {
		return r, fmt.Errorf("compare: %w", err)
	}
	r.RecognisedA = cmp.TheirsReadable
	r.RecognisedB = cmp.MineReadable
	r.Gated = cmp.Gated
	r.GateField = cmp.DifferingField
	r.AgreeingFields = cmp.AgreeingFields
	r.ModelUsed = cmp.ModelUsed
	r.SimilarityFree = cmp.Similarity
	r.SignatureA = cmp.TheirsFields.Signature()
	r.SignatureB = cmp.MineFields.Signature()
	r.RemainderA = cmp.TheirsFields.Remainder
	r.RemainderB = cmp.MineFields.Remainder
	if !cmp.MineReadable {
		// Compare returns a zero Comparison when the querying side could not
		// be read at all, which is the fall-back-to-semantic case. Recording
		// it explicitly stops a reader taking Gated=false as "the gate
		// passed this pair".
		r.SignatureB = "(unreadable)"
	}
	if m.extractFailed.Load() {
		return r, fmt.Errorf("the extraction model did not answer for this pair, so this row " +
			"would record a provider failure as a property of the extractor")
	}
	return r, nil
}

// extractorFor returns the arm's extractor, wrapping the LLM one in its disk
// cache and rate limiter.
func (m *measurer) extractorFor(a arm) (semantic.FieldExtractor, error) {
	if a.Extractor != "llm" {
		return a.Fields, nil
	}
	if m.llm == nil {
		return nil, fmt.Errorf("the LLM extractor arm was built without a key")
	}
	return m.cachedLLMFields, nil
}

// cachedLLMFields is the LLM extractor with a disk cache in front of it.
//
// The cache is what makes this arm resumable and affordable: an interrupted
// run re-reads every reply it already paid for, and the verdicts recomputed
// from them are identical because ParseFields is deterministic. It also means
// a -report-only pass needs no key at all.
//
// It caches the RAW REPLY rather than the parsed fields, so that a change to
// the parsing is applied to the stored replies instead of silently keeping
// results parsed by older code.
func (m *measurer) cachedLLMFields(activityType, intentText string) (semantic.Fields, error) {
	key := replyKey(m.LLMModel, activityType, intentText)
	if raw, ok := m.llmCache.Get(key); ok {
		if f, ok := llm.ParseFields(raw); ok {
			return f, nil
		}
		// A cached reply that is PRESENT but does not parse is a real
		// measurement: the model answered and its answer was unusable.
		return semantic.Fields{}, nil
	}
	m.limiter.Wait()
	f, err := m.llm.ExtractRaw(activityType, intentText)
	if err != nil || strings.TrimSpace(f.Raw) == "" {
		// THE MODEL DID NOT ANSWER. That is not the same as "this intent has
		// no deciding fields", and the difference is the whole integrity of
		// this arm: an earlier version of this harness cached the empty reply,
		// and twenty rate-limited calls became twenty permanent rows claiming
		// the extractor could not read the intent. Nothing is cached, the flag
		// is set, and Measure turns the row into an ERROR that a resume
		// retries.
		m.extractFailed.Store(true)
		return semantic.Fields{}, nil
	}
	m.llmCache.Put(key, f.Raw)
	return f.Fields, nil
}

func replyKey(model, activityType, intentText string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + activityType + "\x00" + intentText))
	return hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------------------
// The reply cache.
// ---------------------------------------------------------------------------

// replyCache stores the model's raw replies in one JSON file.
//
// It is flushed after every miss, not at the end. A run killed mid-way must
// not throw away the calls it has already paid for — on a free tier those are
// the scarcest thing in the experiment.
type replyCache struct {
	path string
	mu   sync.Mutex
	data map[string]string
}

func newReplyCache(path string) (*replyCache, error) {
	c := &replyCache{path: path, data: map[string]string{}}
	if path == "" {
		return c, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &c.data); err != nil {
			// A corrupt cache is a cache, not a result. Starting empty costs
			// model calls; refusing to start costs the whole run.
			fmt.Fprintf(os.Stderr, "fieldeval: ignoring unreadable LLM cache %s: %v\n", path, err)
			c.data = map[string]string{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return c, nil
}

func (c *replyCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok
}

func (c *replyCache) Put(key, raw string) {
	c.mu.Lock()
	c.data[key] = raw
	c.mu.Unlock()
	_ = c.Flush()
}

func (c *replyCache) Flush() error {
	if c.path == "" {
		return nil
	}
	c.mu.Lock()
	b, err := json.Marshal(c.data)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func (c *replyCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.data)
}

// ---------------------------------------------------------------------------
// Pacing.
// ---------------------------------------------------------------------------

// limiter spaces model calls evenly instead of bursting and backing off.
//
// Groq's free tier allows roughly thirty requests a minute. Bursting into a
// 429 and retrying works — the client honours Retry-After — but it makes the
// run's wall clock unpredictable and its latency figures bimodal. Spacing the
// calls keeps both readable.
type limiter struct {
	mu   sync.Mutex
	gap  time.Duration
	next time.Time
}

func newLimiter(perMin float64) *limiter {
	if perMin <= 0 {
		perMin = 22
	}
	return &limiter{gap: time.Duration(float64(time.Minute) / perMin)}
}

func (l *limiter) Wait() {
	if l == nil {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.After(now) {
		wait := l.next.Sub(now)
		l.next = l.next.Add(l.gap)
		l.mu.Unlock()
		time.Sleep(wait)
		return
	}
	l.next = now.Add(l.gap)
	l.mu.Unlock()
}

// ---------------------------------------------------------------------------
// The in-memory candidate store.
// ---------------------------------------------------------------------------

// memIntents is core.IntentStore backed by a map, so the real checker can run
// with no database behind it. Rows are keyed by run and activity type, the
// same scoping the SQL store uses, which is what keeps one pair from seeing
// another pair's candidate.
type memIntents struct {
	byRun map[string][]core.Intent
}

func newMemIntents() *memIntents { return &memIntents{byRun: map[string][]core.Intent{}} }

func (m *memIntents) key(shard core.ShardID, wf string, run core.RunID, at string) string {
	return fmt.Sprintf("%d|%s|%s|%s", shard, wf, run, at)
}

func (m *memIntents) Put(_ context.Context, in core.Intent) error {
	k := m.key(in.ShardID, in.WorkflowID, in.RunID, in.ActivityType)
	m.byRun[k] = append(m.byRun[k], in)
	return nil
}

func (m *memIntents) Candidates(_ context.Context, shard core.ShardID, wf string, run core.RunID, at string) ([]core.Intent, error) {
	return m.byRun[m.key(shard, wf, run, at)], nil
}

var _ core.IntentStore = (*memIntents)(nil)

// ---------------------------------------------------------------------------
// The embedder stack and its timings.
// ---------------------------------------------------------------------------

// latencyLog records how long the real model round-trips took, separately
// from the check timings. Mixing the two would produce a bimodal distribution
// whose percentiles describe nothing.
type latencyLog struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *latencyLog) Add(d time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d)
	l.mu.Unlock()
}

func (l *latencyLog) Summary() (int, time.Duration, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return 0, 0, 0
	}
	s := append([]time.Duration(nil), l.d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	q := func(p float64) time.Duration {
		i := int(p * float64(len(s)-1))
		return s[i]
	}
	return len(s), q(0.50), q(0.99)
}

// buildEmbedder assembles caching over the engine's own HTTP embedder, and
// refuses to start if the service is not answering.
//
// The refusal matters more than it looks. An unreachable service would not
// stop the run: every check would degrade to the checker's DISTINCT fallback,
// every arm would score a recall of zero, and nothing about the output would
// look like a failure — it would look like a finding.
func buildEmbedder(ctx context.Context, cfg config, stats *latencyLog) (core.Embedder, error) {
	base := semantic.NewHTTPEmbedder(cfg.endpoint, cfg.timeout).WithModel(cfg.model, cfg.dim)
	probeCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	if _, err := base.Embed(probeCtx, "startup probe"); err != nil {
		return nil, fmt.Errorf("%w\n  endpoint: %s\n  cause: %v\n"+
			"  the service must serve POST {\"text\":\"...\"} -> {\"embedding\":[...%d floats]}",
			errNoEmbedder, cfg.endpoint, err, cfg.dim)
	}
	cache, err := newEmbedCache(cfg.cacheDir, cfg.model)
	if err != nil {
		return nil, err
	}
	return &cachingEmbedder{base: base, cache: cache, stats: stats}, nil
}

// cachingEmbedder serves a repeated intent from disk. The corpora reuse
// phrasings heavily, and re-embedding the same sentence would pay for an
// answer already known.
type cachingEmbedder struct {
	base  *semantic.HTTPEmbedder
	cache *embedCache
	stats *latencyLog
}

func (c *cachingEmbedder) Model() string { return c.base.Model() }
func (c *cachingEmbedder) Dim() int      { return c.base.Dim() }

func (c *cachingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if v, ok := c.cache.Get(text); ok {
		return v, nil
	}
	start := time.Now()
	v, err := c.base.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	c.stats.Add(time.Since(start))
	c.cache.Put(text, v)
	return v, nil
}

var _ core.Embedder = (*cachingEmbedder)(nil)

// renderSentence and renderEnvelope name the two renderings.
const (
	renderSentence = "sentence"
	renderEnvelope = "envelope"
)

// argsOf reads the argument sentence back out of a tool-call envelope, so the
// checker's RenderIntent hook can re-render it the way the harness does.
func argsOf(in json.RawMessage) string {
	var c agent.ToolCall
	if err := json.Unmarshal(in, &c); err != nil {
		return ""
	}
	return c.Args
}
