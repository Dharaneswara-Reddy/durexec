// Command experiment measures how well the engine's idempotency checks tell a
// repeated action from a genuinely new one, and what those checks cost.
//
// It is the harness behind the precision/recall figure the project's
// contribution rests on. It takes a set of retry pairs — two phrasings of a
// request, with a ground-truth label saying whether they are the same request —
// and runs each pair through several deduplication strategies:
//
//	exact       the default: two calls are the same only when their arguments
//	            are byte-identical once canonicalised
//	tau=X       semantic matching with one threshold: call it a duplicate when
//	            the two meanings are at least X similar
//	band        semantic matching with three outcomes, where a score in the
//	            uncertain band between two thresholds is handed to a human
//	            instead of being guessed either way
//
// The threshold is swept, by default from 0.70 to 0.99 in steps of 0.01, so
// the single-threshold and banded strategies each trace a precision/recall
// curve rather than sitting at three isolated points.
//
// For each strategy it reports how many real duplicates were caught, how often
// a distinct request was wrongly called a duplicate, precision, recall and F1,
// and how long a single check took. It writes a per-arm CSV, an optional
// per-pair CSV, and a self-contained HTML report with the curve drawn as
// inline SVG.
//
// It needs no database, no network and no model server: with the default fake
// embedder it runs anywhere Go runs. THAT IS ALSO ITS BIGGEST LIMITATION. A
// hash-based pseudo-embedding cannot measure meaning, so a fake-embedder run
// measures the plumbing and not the contribution. Every run says so in its
// header and again at the end.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/app/agent"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/exact"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "experiment: %v\n", err)
		if errors.Is(err, errIncomplete) {
			// A distinct exit status, so a script or Makefile that chains this
			// command can tell "the run broke" from "the run finished but some
			// of it was never measured" — and must not treat either as success.
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// errIncomplete is returned when the run finished but at least one embedding
// never arrived. The outputs are still written, each one stamped INCOMPLETE, so
// the failure can be diagnosed; but the process exits non-zero so nothing
// downstream mistakes the run for a finished measurement.
var errIncomplete = errors.New("run INCOMPLETE: some embeddings could not be obtained, " +
	"so the outputs are marked INCOMPLETE and must not be quoted as results")

// ---------------------------------------------------------------------------
// Configuration.
// ---------------------------------------------------------------------------

type config struct {
	csvPath      string
	pairsCSVPath string
	htmlPath     string
	embedderKind string
	endpoint     string
	model        string
	dim          int
	timeout      time.Duration
	tauMin       float64
	tauMax       float64
	tauStep      float64
	bandWidth    float64
	tauLow       float64
	tauHigh      float64
	repeat       int
	activityType string
	cacheDir     string
	retries      int
	retryBase    time.Duration
	hybrid       bool
	verbose      bool
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.csvPath, "csv", "experiment-results.csv",
		"path to write the per-arm summary CSV to")
	flag.StringVar(&c.pairsCSVPath, "pairs-csv", "",
		"optional path to write one row per pair per arm, for error analysis")
	flag.StringVar(&c.htmlPath, "html", "experiment-report.html",
		"path to write the self-contained HTML report to; empty disables it")
	flag.StringVar(&c.embedderKind, "embedder", "fake",
		"which embedder to measure: 'fake' for the built-in hash embedder (needs no model "+
			"server) or 'http' to call a real embedding service")
	flag.StringVar(&c.endpoint, "endpoint", "http://localhost:8000/embed",
		"embedding service URL, used when -embedder=http; `make up-semantic` publishes "+
			"the all-MiniLM-L6-v2 service at this address")
	flag.StringVar(&c.model, "model", semantic.DefaultModel,
		"model name to record in the output and to key the cache by, used when -embedder=http")
	flag.IntVar(&c.dim, "dim", semantic.DefaultDim, "embedding dimension")
	flag.DurationVar(&c.timeout, "timeout", semantic.DefaultTimeout, "per-embed timeout")
	flag.Float64Var(&c.tauMin, "tau-min", 0.70, "lowest threshold in the sweep")
	flag.Float64Var(&c.tauMax, "tau-max", 0.99, "highest threshold in the sweep")
	flag.Float64Var(&c.tauStep, "tau-step", 0.01, "step between thresholds in the sweep")
	flag.Float64Var(&c.bandWidth, "band-width", 0.05,
		"width of the escalation band; the banded arm at tau covers [tau, tau+width)")
	flag.Float64Var(&c.tauLow, "tau-low", core.DefaultThresholds().TauLow,
		"lower edge of the shipped escalation band, marked on the curve as the operating point")
	flag.Float64Var(&c.tauHigh, "tau-high", core.DefaultThresholds().TauHigh,
		"upper edge of the shipped escalation band")
	flag.IntVar(&c.repeat, "repeat", 5,
		"how many times to run each check, for the latency percentiles")
	flag.StringVar(&c.activityType, "activity", agent.ActivityBookFlight,
		"activity type the pairs are scheduled under")
	flag.StringVar(&c.cacheDir, "cache-dir", defaultCacheDir(),
		"directory for cached embeddings, keyed by (model name, intent text); empty disables caching")
	flag.IntVar(&c.retries, "retries", 4,
		"how many attempts each embedding gets before the run is called incomplete")
	flag.DurationVar(&c.retryBase, "retry-base", 200*time.Millisecond,
		"first backoff delay between embedding attempts; it doubles each time")
	flag.BoolVar(&c.hybrid, "hybrid", false,
		"also measure the OPT-IN hybrid comparator (deciding fields compared exactly, similarity "+
			"reserved for the free text), on the same pairs and the same sweep as the semantic arms. "+
			"Off by default so that a plain run reproduces the published baseline exactly")
	flag.BoolVar(&c.verbose, "v", false, "list every misclassified pair")
	flag.Parse()
	return c
}

// defaultCacheDir keeps cached vectors out of the repository. It lands under
// the user's cache directory, or beside the binary if the platform has none.
func defaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ".experiment-cache"
	}
	return filepath.Join(base, "durexec", "embeddings")
}

// commandLine reproduces the invocation, so the report can state exactly what
// produced it and a reader can re-run it without guessing.
//
// `go run` builds into a temporary directory, so argv[0] is a path like
// /tmp/go-build.../exe/experiment that exists for the length of one run and
// tells a reader nothing. It is rewritten to the command that would rebuild
// it, which is what somebody re-running this actually has to type.
func commandLine() string {
	args := append([]string{}, os.Args...)
	if len(args) > 0 && strings.Contains(args[0], "/go-build") {
		args[0] = "go run ./cmd/experiment"
	}
	return strings.Join(args, " ")
}

// ---------------------------------------------------------------------------
// The in-memory candidate store.
// ---------------------------------------------------------------------------

// memIntents is core.IntentStore backed by a map, so the harness can drive the
// real semantic.Checker with no database behind it.
//
// Rows are keyed by run and activity type, the same scoping the SQL store uses.
// That scoping is not a detail: it is what keeps the candidate set to the
// length of one workflow's history, and it also stops one pair in this corpus
// from seeing another pair's candidate.
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
// Arms.
// ---------------------------------------------------------------------------

type armKind string

const (
	kindExact    armKind = "exact"
	kindSemantic armKind = "semantic"

	// kindHybrid is the opt-in comparator: every deciding field of the intent
	// must match exactly, and the threshold applies only to the free text
	// that is left. Its arms are swept over the same taus as the semantic
	// ones so the two are read off the same grid.
	kindHybrid armKind = "hybrid"
)

// Series names the curve an arm belongs to on the chart. Single-threshold and
// banded arms are plotted separately because they are two different policies,
// not two points on one policy: the banded one can refuse to answer.
const (
	seriesExact  = "exact"
	seriesSingle = "single-threshold"
	seriesBand   = "escalation-band"

	// The hybrid comparator gets its own two curves rather than extra points
	// on the semantic ones. It is a different policy, not a different
	// threshold of the same policy, and plotting them on one line would
	// invite reading a jump between policies as movement along a curve.
	seriesHybridSingle = "hybrid-single-threshold"
	seriesHybridBand   = "hybrid-escalation-band"
)

// arm is one deduplication strategy under test. Within the semantic mechanism
// an arm is nothing more than a different pair of thresholds.
type arm struct {
	Name string
	Kind armKind

	// Thresholds matter only for kindSemantic. A single-threshold arm sets
	// TauLow == TauHigh, which makes core.Thresholds.Classify two-valued:
	// there is no band left for an ESCALATE verdict to land in.
	Thresholds core.Thresholds

	// Series is which curve this arm is a point on.
	Series string

	// OperatingPoint marks the thresholds the engine actually ships with. It
	// is called out on the chart and in the tables, because a curve without a
	// marked operating point invites a reader to quote whichever point on it
	// flatters the result.
	OperatingPoint bool

	// Landmark marks the handful of arms worth printing in full on a
	// terminal. The complete sweep goes to the CSV and the HTML report.
	Landmark bool

	// Candidate marks the OPT-IN comparator at the shipped thresholds: not
	// the operating point, because the engine does not run this way by
	// default, but the arm a reader comparing the two comparators wants. It
	// is marked separately so that nothing in the output can be mistaken for
	// a claim about what the engine currently does.
	Candidate bool
}

// Escalating reports whether this arm can return ESCALATE at all.
func (a arm) Escalating() bool {
	return a.Kind != kindExact && a.Thresholds.TauLow < a.Thresholds.TauHigh
}

// Thresholded reports whether a threshold means anything to this arm. Both
// comparators classify a score; only the exact baseline has no score to
// classify.
func (a arm) Thresholded() bool { return a.Kind != kindExact }

// outcome is what one arm decided about one pair.
type outcome struct {
	Verdict    core.DedupVerdict
	Similarity float64
	Latency    time.Duration
	Fallback   bool

	// Detail is the journaled explanation, which the hybrid comparator writes
	// and the pure-semantic one leaves empty. It is carried through to the
	// per-pair CSV so that an error analysis can read WHY a verdict was
	// reached and not only what it was.
	Detail string
}

// landmarkTaus are the thresholds printed individually on the terminal: the
// two ends of the sweep and the values the design document names.
var landmarkTaus = []float64{0.70, 0.80, 0.85, 0.90, 0.95, 0.99}

// sweepTaus expands the sweep into exact grid values.
//
// The grid is built by integer multiplication rather than by adding the step
// repeatedly. Repeated addition of 0.01 drifts — after thirty additions the
// value that should be 0.99 is not — and a threshold that is a hair off is a
// threshold whose printed name does not match the comparison it performed.
func sweepTaus(min, max, step float64) ([]float64, error) {
	switch {
	case step <= 0:
		return nil, fmt.Errorf("-tau-step must be positive, got %.4f", step)
	case min <= 0 || min > 1:
		return nil, fmt.Errorf("-tau-min %.4f is outside the cosine range (0,1]", min)
	case max <= 0 || max > 1:
		return nil, fmt.Errorf("-tau-max %.4f is outside the cosine range (0,1]", max)
	case min > max:
		return nil, fmt.Errorf("-tau-min %.4f is above -tau-max %.4f", min, max)
	}
	n := int(math.Round((max - min) / step))
	out := make([]float64, 0, n+1)
	for i := 0; i <= n; i++ {
		tau := roundTau(min + float64(i)*step)
		if tau > 1 {
			break
		}
		out = append(out, tau)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the sweep from %.4f to %.4f in steps of %.4f is empty", min, max, step)
	}
	return out, nil
}

// roundTau snaps a threshold to four decimal places. Thresholds are quoted in
// prose and used as map keys, so they must be exactly the value they are
// printed as.
func roundTau(v float64) float64 { return math.Round(v*10000) / 10000 }

func buildArms(cfg config) ([]arm, error) {
	taus, err := sweepTaus(cfg.tauMin, cfg.tauMax, cfg.tauStep)
	if err != nil {
		return nil, err
	}
	if cfg.bandWidth <= 0 {
		return nil, fmt.Errorf("-band-width must be positive, got %.4f", cfg.bandWidth)
	}
	if cfg.tauLow > cfg.tauHigh {
		return nil, fmt.Errorf("-tau-low %.2f is above -tau-high %.2f", cfg.tauLow, cfg.tauHigh)
	}

	landmark := make(map[float64]bool, len(landmarkTaus))
	for _, t := range landmarkTaus {
		landmark[t] = true
	}

	arms := []arm{{
		Name:     "exact",
		Kind:     kindExact,
		Series:   seriesExact,
		Landmark: true,
	}}

	for _, tau := range taus {
		arms = append(arms, arm{
			Name:       fmt.Sprintf("semantic tau=%.2f", tau),
			Kind:       kindSemantic,
			Thresholds: core.Thresholds{TauLow: tau, TauHigh: tau},
			Series:     seriesSingle,
			Landmark:   landmark[tau],
		})
	}

	operating := core.Thresholds{TauLow: roundTau(cfg.tauLow), TauHigh: roundTau(cfg.tauHigh)}
	haveOperating := false
	for _, tau := range taus {
		// The top of the band is clamped: a band that ran past 1.0 would have
		// a region above it that no score can reach, so its upper edge would
		// silently stop meaning anything.
		hi := roundTau(math.Min(tau+cfg.bandWidth, 1.0))
		th := core.Thresholds{TauLow: tau, TauHigh: hi}
		a := arm{
			Name:       bandArmName(th),
			Kind:       kindSemantic,
			Thresholds: th,
			Series:     seriesBand,
			Landmark:   landmark[tau],
		}
		if th == operating {
			a.OperatingPoint, a.Landmark, haveOperating = true, true, true
		}
		arms = append(arms, a)
	}

	// The shipped thresholds must appear whether or not they happen to land on
	// the sweep grid. They are the numbers the engine runs with, so a report
	// that does not contain them is not a report about the engine.
	if !haveOperating {
		arms = append(arms, arm{
			Name:           bandArmName(operating),
			Kind:           kindSemantic,
			Thresholds:     operating,
			Series:         seriesBand,
			OperatingPoint: true,
			Landmark:       true,
		})
	}

	if cfg.hybrid {
		arms = append(arms, hybridArms(taus, cfg.bandWidth, operating, landmark)...)
	}
	return arms, nil
}

// hybridArms mirrors the semantic arms one for one.
//
// The mirror is the point. Every hybrid arm has a semantic arm with the same
// threshold and the same band width, over the same pairs, so a difference
// between the two tables is a difference between the comparators and cannot
// be a difference in how they were measured.
func hybridArms(taus []float64, bandWidth float64, operating core.Thresholds, landmark map[float64]bool) []arm {
	var arms []arm
	for _, tau := range taus {
		arms = append(arms, arm{
			Name:       fmt.Sprintf("hybrid tau=%.2f", tau),
			Kind:       kindHybrid,
			Thresholds: core.Thresholds{TauLow: tau, TauHigh: tau},
			Series:     seriesHybridSingle,
			Landmark:   landmark[tau],
		})
	}
	haveShipped := false
	for _, tau := range taus {
		hi := roundTau(math.Min(tau+bandWidth, 1.0))
		th := core.Thresholds{TauLow: tau, TauHigh: hi}
		a := arm{
			Name:       hybridBandArmName(th),
			Kind:       kindHybrid,
			Thresholds: th,
			Series:     seriesHybridBand,
			Landmark:   landmark[tau],
		}
		if th == operating {
			a.Candidate, a.Landmark, haveShipped = true, true, true
		}
		arms = append(arms, a)
	}
	if !haveShipped {
		arms = append(arms, arm{
			Name:       hybridBandArmName(operating),
			Kind:       kindHybrid,
			Thresholds: operating,
			Series:     seriesHybridBand,
			Candidate:  true,
			Landmark:   true,
		})
	}
	return arms
}

func bandArmName(th core.Thresholds) string {
	return fmt.Sprintf("semantic escalate [%.2f,%.2f)", th.TauLow, th.TauHigh)
}

func hybridBandArmName(th core.Thresholds) string {
	return fmt.Sprintf("hybrid escalate [%.2f,%.2f)", th.TauLow, th.TauHigh)
}

// ---------------------------------------------------------------------------
// Observation: everything that does not depend on a threshold, measured once.
// ---------------------------------------------------------------------------

// observation is one pair's measurement, stripped of any threshold.
//
// Splitting the run this way is what makes a thirty-point sweep affordable. A
// pair's similarity score is a property of the pair and the model; it does not
// move when a threshold moves. Re-embedding the whole corpus once per
// threshold would multiply the model calls by the number of arms and measure
// nothing new.
//
// The verdict for an arm is then core.Thresholds.Classify(similarity). That is
// not a reimplementation of the checker: it is the identical function call
// semantic.Checker itself makes on the line that produces its verdict. The
// harness proves it, rather than asserting it, by ALSO recording the real
// checker's verdict at the shipped thresholds and refusing to continue if the
// two ever disagree — see verifyClassification.
type observation struct {
	Pair Pair

	// ExactDuplicate is the byte-comparison baseline's answer.
	ExactDuplicate bool
	ExactLatencies []time.Duration

	// Similarity is the cosine the real semantic.Checker computed.
	Similarity float64
	// CheckerVerdict is what the real checker returned at the shipped
	// thresholds, kept only to cross-check the classification shortcut.
	CheckerVerdict core.DedupVerdict
	SemLatencies   []time.Duration

	// Fallback means the checker reached its verdict without the model, so
	// this pair's similarity of zero is an absence of a measurement rather
	// than a measurement of zero.
	Fallback bool

	// Failed means an embedding for this pair could not be obtained even
	// after every retry.
	Failed bool
	Reason string

	// ---- the hybrid comparator, measured only when -hybrid was passed -----
	//
	// It is recorded alongside the semantic measurement rather than in a
	// second run, so both comparators see the identical pair, the identical
	// intent text and the identical cached vectors. A difference between the
	// two tables can then only be the comparators.

	HybridMeasured bool

	// HybridGated is the gate's answer: a deciding field differed, so no
	// threshold can make this pair a duplicate. Scoring an arm needs this and
	// the similarity, and nothing else — see score.
	HybridGated bool

	// HybridReadable is false when the extractor could not read one side's
	// intent at all, in which case the engine fell back to the pure-semantic
	// comparison for this pair and the verdict is not a gated one.
	HybridReadable bool

	// HybridModelUsed reports whether the free-text comparison actually
	// needed an embedding. Counted because a corpus on which it is rarely
	// true is a corpus on which the similarity half of this comparator did
	// nothing, and the report has to say so.
	HybridModelUsed bool

	HybridSimilarity float64
	HybridVerdict    core.DedupVerdict
	HybridField      string
	HybridDetail     string
	HybridLatencies  []time.Duration
}

// observe measures every pair once per repetition.
//
// The embeddings are warmed first, outside the timed section. That is
// deliberate and it changes what the latency figures mean: after the warm-up
// every embedding is served from the local cache, so the per-check timings
// measure the engine's check path WITHOUT the model round-trip. The round-trip
// is measured separately, during the warm-up, and reported on its own line.
// Mixing the two would produce a bimodal distribution whose p50 and p99
// describe nothing.
func observe(ctx context.Context, cfg config, pairs []Pair, emb core.Embedder, st *runStatus) ([]observation, error) {
	warmEmbeddings(ctx, cfg, pairs, emb, st)

	repeat := cfg.repeat
	if repeat < 1 {
		repeat = 1
	}
	operating := arm{
		Kind:       kindSemantic,
		Thresholds: core.Thresholds{TauLow: roundTau(cfg.tauLow), TauHigh: roundTau(cfg.tauHigh)},
	}
	exact := arm{Kind: kindExact}

	out := make([]observation, 0, len(pairs))
	for _, p := range pairs {
		o := observation{Pair: p}

		for i := 0; i < repeat; i++ {
			ex, err := checkPair(ctx, cfg, exact, p, emb)
			if err != nil {
				// An exact-key failure is a bug in the harness, not a service
				// problem: it means the corpus produced arguments that are not
				// valid JSON. There is nothing to degrade to.
				return nil, fmt.Errorf("exact check of pair %s: %w", p.ID, err)
			}
			o.ExactLatencies = append(o.ExactLatencies, ex.Latency)
			o.ExactDuplicate = ex.Verdict == core.VerdictDuplicate

			sem, err := checkPair(ctx, cfg, operating, p, emb)
			if err != nil {
				// checkSemantic only errors when the stored side could not be
				// embedded at all. Record it and carry on: one unreachable
				// pair must not destroy the other two hundred measurements,
				// but it does make the run incomplete.
				o.Failed, o.Reason = true, err.Error()
				st.embedFailed(p.ID, err)
				break
			}
			o.SemLatencies = append(o.SemLatencies, sem.Latency)
			o.Similarity = sem.Similarity
			o.CheckerVerdict = sem.Verdict
			o.Fallback = sem.Fallback

			if !cfg.hybrid {
				continue
			}
			hyb, cmp, herr := checkHybridPair(ctx, cfg, operating, p, emb)
			if herr != nil {
				o.Failed, o.Reason = true, herr.Error()
				st.embedFailed(p.ID, herr)
				break
			}
			o.HybridMeasured = true
			o.HybridLatencies = append(o.HybridLatencies, hyb.Latency)
			o.HybridSimilarity = hyb.Similarity
			o.HybridVerdict = hyb.Verdict
			o.HybridDetail = hyb.Detail
			o.HybridGated = cmp.Gated
			o.HybridReadable = cmp.MineReadable && cmp.TheirsReadable
			o.HybridModelUsed = cmp.ModelUsed
			o.HybridField = cmp.DifferingField
			if hyb.Fallback {
				// The model was needed for the free text and was not there.
				// Counted as a fallback for the same reason the semantic path
				// is: a similarity of zero that was never measured must not be
				// read as a measurement.
				o.Fallback = true
			}
		}

		if o.Fallback {
			st.fellBack(p.ID)
		}
		out = append(out, o)
	}
	return out, nil
}

// warmEmbeddings fills the cache and is where a failing service is caught.
//
// It walks both sides of every pair. A side that cannot be embedded after
// every retry is recorded on the run status, which is what marks the whole run
// incomplete — the alternative, letting the checker quietly fall back to
// DISTINCT for that pair, would report a recall shortfall that is really a
// network problem.
func warmEmbeddings(ctx context.Context, cfg config, pairs []Pair, emb core.Embedder, st *runStatus) {
	if emb == nil {
		return
	}
	seen := make(map[string]bool, 2*len(pairs))
	for _, p := range pairs {
		for _, side := range []string{p.A, p.B} {
			input, err := toolCallJSON(cfg.activityType, side)
			if err != nil {
				st.embedFailed(p.ID, err)
				continue
			}
			text, err := semantic.IntentText(cfg.activityType, input)
			if err != nil {
				st.embedFailed(p.ID, err)
				continue
			}
			if seen[text] {
				continue
			}
			seen[text] = true
			if _, err := emb.Embed(ctx, text); err != nil {
				st.embedFailed(p.ID, fmt.Errorf("warm %q: %w", text, err))
			}

			if !cfg.hybrid {
				continue
			}
			// The hybrid comparator embeds the FREE TEXT REMAINDER, not the
			// whole intent, so it needs its own vectors warmed or its first
			// timed check would pay for a model round-trip the semantic arms
			// were not charged for. An empty remainder needs no vector at all
			// — that is the case the comparator answers without a model.
			fields, ferr := semantic.ItineraryFields(cfg.activityType, text)
			if ferr != nil || fields.Remainder == "" || seen[fields.Remainder] {
				continue
			}
			seen[fields.Remainder] = true
			if _, err := emb.Embed(ctx, fields.Remainder); err != nil {
				st.embedFailed(p.ID, fmt.Errorf("warm free text %q: %w", fields.Remainder, err))
			}
		}
	}
	st.UniqueIntents = len(seen)
}

// verifyClassification is the guard that licenses scoring an arm by calling
// core.Thresholds.Classify on a stored similarity instead of re-running the
// checker for every threshold.
//
// For every pair it re-derives the shipped arm's verdict from the stored score
// and compares it with the verdict the real semantic.Checker returned. A
// mismatch means the shortcut is no longer equivalent to the engine — the
// checker grew a rule the harness does not know about — and every number in
// the sweep would be a fiction. So it is a hard error, not a warning.
func verifyClassification(obs []observation, operating core.Thresholds) error {
	for _, o := range obs {
		if o.HybridMeasured && !o.Failed {
			// The same guard for the hybrid comparator, and it has one more
			// thing to prove: that a gated pair really is DISTINCT at every
			// threshold rather than merely at this one.
			want := operating.Classify(o.HybridSimilarity)
			if o.HybridGated {
				want = core.VerdictDistinct
			}
			if want != o.HybridVerdict {
				return fmt.Errorf(
					"pair %s: scoring the hybrid arm by its gate and threshold gives %s but "+
						"semantic.Checker returned %s at similarity %.6f (gated=%t). The harness "+
						"scores the whole sweep that way, and it is only valid while it agrees "+
						"with the checker exactly",
					o.Pair.ID, want, o.HybridVerdict, o.HybridSimilarity, o.HybridGated)
			}
		}
		if o.Failed || o.CheckerVerdict == "" {
			continue
		}
		if got := operating.Classify(o.Similarity); got != o.CheckerVerdict {
			return fmt.Errorf(
				"pair %s: scoring by threshold gives %s but semantic.Checker returned %s at "+
					"similarity %.6f. The harness scores the whole sweep by re-classifying "+
					"stored similarities, and that is only valid while it agrees with the "+
					"checker exactly. It no longer does, so no number in this run can be trusted",
				o.Pair.ID, got, o.CheckerVerdict, o.Similarity)
		}
	}
	return nil
}

// score turns the threshold-free observations into one arm's confusion matrix.
func score(a arm, obs []observation) (*armResult, []perPairRow) {
	res := newArmResult(a, len(obs))
	rows := make([]perPairRow, 0, len(obs))

	for _, o := range obs {
		var out outcome
		switch a.Kind {
		case kindExact:
			out.Verdict = core.VerdictDistinct
			if o.ExactDuplicate {
				out.Verdict, out.Similarity = core.VerdictDuplicate, 1.0
			}
			res.Latencies = append(res.Latencies, o.ExactLatencies...)
		case kindHybrid:
			// The gate is threshold-free: a pair whose deciding fields differ
			// is DISTINCT at every threshold in the sweep, and no score was
			// measured for it. Everything else is classified exactly as the
			// semantic arms are, which is what makes the two tables
			// comparable row for row.
			out.Fallback = o.Fallback
			out.Detail = o.HybridDetail
			switch {
			case o.Failed || !o.HybridMeasured:
				out.Verdict, out.Similarity = core.VerdictDistinct, 0
			case o.HybridGated:
				out.Verdict, out.Similarity = core.VerdictDistinct, 0
			default:
				out.Similarity = o.HybridSimilarity
				out.Verdict = a.Thresholds.Classify(o.HybridSimilarity)
			}
			res.Latencies = append(res.Latencies, o.HybridLatencies...)

		default:
			out.Similarity = o.Similarity
			out.Verdict = a.Thresholds.Classify(o.Similarity)
			out.Fallback = o.Fallback
			if o.Failed {
				// No score was ever obtained, so no verdict can be claimed.
				// The engine's own degraded path answers DISTINCT here, and
				// counting it that way is what keeps the arm's pair count
				// honest; the run is separately marked incomplete.
				out.Verdict, out.Similarity = core.VerdictDistinct, 0
			}
			res.Latencies = append(res.Latencies, o.SemLatencies...)
		}

		res.record(o.Pair, out)
		row := perPairRow{ArmName: a.Name, Arm: a, Pair: o.Pair, Outcome: out}
		if a.Kind == kindHybrid {
			row.Gated, row.DecidingField, row.ModelUsed = o.HybridGated, o.HybridField, o.HybridModelUsed
		}
		rows = append(rows, row)
	}
	return res, rows
}

// ---------------------------------------------------------------------------
// Running.
// ---------------------------------------------------------------------------

func run(cfg config) error {
	started := time.Now().UTC()

	pairs, err := Corpus()
	if err != nil {
		return err
	}
	arms, err := buildArms(cfg)
	if err != nil {
		return err
	}

	st := newRunStatus()
	emb, prov, err := buildEmbedder(cfg, st)
	if err != nil {
		return err
	}
	prov.StartedAt = started
	prov.Corpus = Summarize(pairs)
	prov.Arms = len(arms)
	prov.Repeat = cfg.repeat
	prov.ActivityType = cfg.activityType
	prov.TauMin, prov.TauMax, prov.TauStep = cfg.tauMin, cfg.tauMax, cfg.tauStep
	prov.BandWidth = cfg.bandWidth
	prov.TauLow, prov.TauHigh = roundTau(cfg.tauLow), roundTau(cfg.tauHigh)
	prov.Comparators = hybridHeaderLine(cfg)
	prov.CommandLine = commandLine()

	writeHeader(cfg, prov)

	ctx := context.Background()
	obs, err := observe(ctx, cfg, pairs, emb, st)
	if err != nil {
		return err
	}
	operating := core.Thresholds{TauLow: roundTau(cfg.tauLow), TauHigh: roundTau(cfg.tauHigh)}
	if err := verifyClassification(obs, operating); err != nil {
		return err
	}

	results := make([]*armResult, 0, len(arms))
	var perPair []perPairRow
	for _, a := range arms {
		res, rows := score(a, obs)
		res.Embedder = prov.EmbedderLabel
		results = append(results, res)
		perPair = append(perPair, rows...)
	}

	prov.FinishedAt = time.Now().UTC()
	prov.finish(st)

	printTable(results)
	printHybridComparison(results, prov)
	printBest(results)
	printFixtureVerdicts(perPair)
	printBreakdown(results, perPair)
	printGateSummary(obs)

	var probes []probeResult
	if cfg.hybrid {
		probes, err = runFreeTextProbes(ctx, cfg, emb)
		if err != nil {
			return err
		}
		printFreeTextProbes(probes, prov)
	}
	if cfg.verbose {
		printMistakes(perPair)
	}

	if err := writeSummaryCSV(cfg.csvPath, results, prov); err != nil {
		return err
	}
	fmt.Printf("\nwrote the per-arm summary to %s\n", cfg.csvPath)

	if cfg.pairsCSVPath != "" {
		if err := writePairsCSV(cfg.pairsCSVPath, perPair, prov); err != nil {
			return err
		}
		fmt.Printf("wrote the per-pair detail to %s\n", cfg.pairsCSVPath)
	}

	if cfg.htmlPath != "" {
		if err := writeHTMLReport(cfg.htmlPath, results, perPair, prov); err != nil {
			return err
		}
		fmt.Printf("wrote the HTML report to %s\n", cfg.htmlPath)
	}

	printClosingCaveat(prov)
	printHybridClosingNote(results, obs)
	if prov.Incomplete() {
		return fmt.Errorf("%w (%d pair(s) failed, %d fell back to the degraded path)",
			errIncomplete, len(prov.FailedPairs), len(prov.FallbackPairs))
	}
	return nil
}

// buildEmbedder assembles the stack the run measures through:
//
//	retry  -> cache -> the engine's own embedder
//
// Read outwards from the right. The engine's embedder is the thing under
// test. The cache sits above it so a re-run over the same corpus and model
// costs nothing. The retry sits above the cache so a transient failure is
// retried only on calls that genuinely reach the service — there is no point
// retrying a cache read.
func buildEmbedder(cfg config, st *runStatus) (core.Embedder, *provenance, error) {
	prov := &provenance{
		EmbedderKind: strings.ToLower(cfg.embedderKind),
		Model:        cfg.model,
		Dim:          cfg.dim,
		CacheDir:     cfg.cacheDir,
		Retries:      cfg.retries,
	}

	var base core.Embedder
	switch prov.EmbedderKind {
	case "fake":
		base = semantic.NewFakeEmbedder(cfg.dim)
		prov.Fake = true
		prov.Model = base.Model()
		prov.EmbedderLabel = fmt.Sprintf("FakeEmbedder (sha256 pseudo-embeddings, dim=%d)", cfg.dim)

	case "http":
		e := semantic.NewHTTPEmbedder(cfg.endpoint, cfg.timeout).WithModel(cfg.model, cfg.dim)
		// Probe once, before anything is measured.
		//
		// Without this an unreachable service would not stop the run. Every
		// check would degrade to the checker's DISTINCT fallback, the report
		// would show a recall of zero, and nothing about it would look like a
		// failure — it would look like a finding. A false result is worse than
		// no result, so the run refuses to start.
		ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
		defer cancel()
		if _, err := e.Embed(ctx, "startup probe"); err != nil {
			return nil, nil, fmt.Errorf(
				"the embedding service at %s did not answer a probe, so this run could only "+
					"produce false numbers and will not start.\n"+
					"  cause: %v\n"+
					"  check that the service is up and serving POST {\"text\":\"...\"} -> "+
					"{\"embedding\":[...%d floats]} at that URL,\n"+
					"  or re-run with -embedder=fake to smoke-test the harness with no model",
				cfg.endpoint, err, cfg.dim)
		}
		base = e
		prov.Endpoint = cfg.endpoint
		prov.EmbedderLabel = fmt.Sprintf("HTTPEmbedder, model %s at %s (dim=%d)", cfg.model, cfg.endpoint, cfg.dim)

	default:
		return nil, nil, fmt.Errorf("unknown -embedder %q, want 'fake' or 'http'", cfg.embedderKind)
	}

	cache, err := newEmbedCache(cfg.cacheDir)
	if err != nil {
		return nil, nil, err
	}
	caching := newCachingEmbedder(base, cache)
	retrying := newRetryEmbedder(caching, cfg.retries, cfg.retryBase)

	st.cache, st.caching, st.retrying = cache, caching, retrying
	return retrying, prov, nil
}

// toolCallJSON renders one side of a pair as the activity input the engine
// would schedule.
//
// The argument goes through agent.CanonicalIntent first. That mirrors
// app/agent's toolCallIntent, the function the BookFlight registration names as
// its intent renderer: it lowercases and collapses whitespace and deliberately
// does nothing more. Applying it here means whatever difference is left between
// the two sides is a difference of wording, not of typography — which is the
// difference the semantic check is supposed to be judged on.
func toolCallJSON(activityType, args string) (json.RawMessage, error) {
	b, err := json.Marshal(agent.ToolCall{Tool: activityType, Args: agent.CanonicalIntent(args)})
	if err != nil {
		return nil, fmt.Errorf("render tool call: %w", err)
	}
	return b, nil
}

// checkPair performs one arm's check on one pair and times it.
func checkPair(ctx context.Context, cfg config, a arm, p Pair, emb core.Embedder) (outcome, error) {
	inputA, err := toolCallJSON(cfg.activityType, p.A)
	if err != nil {
		return outcome{}, err
	}
	inputB, err := toolCallJSON(cfg.activityType, p.B)
	if err != nil {
		return outcome{}, err
	}

	if a.Kind == kindExact {
		return checkExact(cfg.activityType, inputA, inputB)
	}
	return checkSemantic(ctx, cfg, a, p, inputA, inputB, emb)
}

// checkExact is the baseline: hash both sides and see whether they collide.
//
// Only the second hash and the lookup are timed. The first side's key was
// computed and stored when that call completed, long before the crash, so
// charging the retry for it would overstate the baseline's cost and flatter the
// semantic arms by comparison.
func checkExact(activityType string, inputA, inputB json.RawMessage) (outcome, error) {
	keyA, err := exact.Key(activityType, inputA)
	if err != nil {
		return outcome{}, err
	}
	seen := map[string]bool{keyA: true}

	start := time.Now()
	keyB, err := exact.Key(activityType, inputB)
	if err != nil {
		return outcome{}, err
	}
	hit := seen[keyB]
	elapsed := time.Since(start)

	out := outcome{Verdict: core.VerdictDistinct, Latency: elapsed}
	if hit {
		out.Verdict, out.Similarity = core.VerdictDuplicate, 1.0
	}
	return out, nil
}

// checkSemantic drives the real semantic.Checker, so what is measured is the
// engine's own code path and not a reimplementation of it.
//
// Side A is embedded and journaled first, outside the timer, because in the
// engine that work happened during A's own commit. The timed section is exactly
// what a retry adds: render the intent text, embed it, load the candidates and
// compare.
func checkSemantic(ctx context.Context, cfg config, a arm, p Pair, inputA, inputB json.RawMessage, emb core.Embedder) (outcome, error) {
	const (
		shard = core.ShardID(0)
		run   = core.RunID("run-1")
		seqA  = core.Seq(1)
	)
	wf := "pair-" + p.ID

	store := newMemIntents()
	textA, err := semantic.IntentText(cfg.activityType, inputA)
	if err != nil {
		return outcome{}, err
	}
	vecA, err := emb.Embed(ctx, textA)
	if err != nil {
		return outcome{}, fmt.Errorf("embed the recorded side of pair %s: %w", p.ID, err)
	}
	if err := store.Put(ctx, core.Intent{
		ShardID: shard, WorkflowID: wf, RunID: run,
		ScheduledSeq: seqA, ActivityType: cfg.activityType,
		IntentText: textA, Embedding: vecA,
	}); err != nil {
		return outcome{}, err
	}

	checker := semantic.NewChecker(emb, store)
	checker.Thresholds = a.Thresholds

	start := time.Now()
	ev, err := checker.Check(ctx, semantic.CheckRequest{
		ShardID:      shard,
		WorkflowID:   wf,
		RunID:        run,
		K:            1,
		ActivityType: cfg.activityType,
		Input:        inputB,
		Thresholds:   a.Thresholds,
		KBySeq:       map[core.Seq]int{seqA: 0},
	})
	elapsed := time.Since(start)
	if err != nil {
		return outcome{}, fmt.Errorf("check pair %s: %w", p.ID, err)
	}

	return outcome{
		Verdict:    ev.Verdict,
		Similarity: ev.Similarity,
		Latency:    elapsed,
		Fallback:   semantic.IsFallback(ev),
		Detail:     ev.Detail,
	}, nil
}

// checkHybridPair drives the real semantic.Checker with the field extractor
// configured, so what is measured is the engine's opt-in path and not a
// reimplementation of it.
//
// It is the same shape as checkSemantic — side A journaled first, outside the
// timer, because in the engine that happened during A's own commit — with two
// differences. The checker is given semantic.ItineraryFields, which is the
// whole switch; and the Comparison is taken a second time, outside the timed
// section, because scoring a thirty-point sweep from one pass needs the
// threshold-free halves of the answer: whether the gate rejected the pair, and
// what the free text scored. Those do not move when a threshold moves.
func checkHybridPair(ctx context.Context, cfg config, a arm, p Pair, emb core.Embedder) (outcome, semantic.Comparison, error) {
	const (
		shard = core.ShardID(0)
		run   = core.RunID("run-1")
		seqA  = core.Seq(1)
	)
	wf := "hybrid-pair-" + p.ID

	inputA, err := toolCallJSON(cfg.activityType, p.A)
	if err != nil {
		return outcome{}, semantic.Comparison{}, err
	}
	inputB, err := toolCallJSON(cfg.activityType, p.B)
	if err != nil {
		return outcome{}, semantic.Comparison{}, err
	}

	store := newMemIntents()
	textA, err := semantic.IntentText(cfg.activityType, inputA)
	if err != nil {
		return outcome{}, semantic.Comparison{}, err
	}
	// The row the engine would have written: the whole intent text and a
	// vector of the whole intent text. The hybrid path never reads that
	// vector — it compares remainders — and storing it anyway keeps the
	// harness faithful to what is actually in the database.
	vecA, err := emb.Embed(ctx, textA)
	if err != nil {
		return outcome{}, semantic.Comparison{}, fmt.Errorf("embed the recorded side of pair %s: %w", p.ID, err)
	}
	if err := store.Put(ctx, core.Intent{
		ShardID: shard, WorkflowID: wf, RunID: run,
		ScheduledSeq: seqA, ActivityType: cfg.activityType,
		IntentText: textA, Embedding: vecA,
	}); err != nil {
		return outcome{}, semantic.Comparison{}, err
	}

	checker := semantic.NewChecker(emb, store)
	checker.Thresholds = a.Thresholds
	checker.Fields = semantic.ItineraryFields

	start := time.Now()
	ev, err := checker.Check(ctx, semantic.CheckRequest{
		ShardID:      shard,
		WorkflowID:   wf,
		RunID:        run,
		K:            1,
		ActivityType: cfg.activityType,
		Input:        inputB,
		Thresholds:   a.Thresholds,
		KBySeq:       map[core.Seq]int{seqA: 0},
	})
	elapsed := time.Since(start)
	if err != nil {
		return outcome{}, semantic.Comparison{}, fmt.Errorf("hybrid check of pair %s: %w", p.ID, err)
	}

	textB, err := semantic.IntentText(cfg.activityType, inputB)
	if err != nil {
		return outcome{}, semantic.Comparison{}, err
	}
	// B is the call being made and A is the candidate it might repeat, which
	// is the direction the engine asks in.
	cmp, err := checker.Compare(ctx, cfg.activityType, textB, textA)
	if err != nil {
		return outcome{}, semantic.Comparison{}, fmt.Errorf("hybrid comparison of pair %s: %w", p.ID, err)
	}

	return outcome{
		Verdict:    ev.Verdict,
		Similarity: ev.Similarity,
		Latency:    elapsed,
		Fallback:   semantic.IsFallback(ev),
		Detail:     ev.Detail,
	}, cmp, nil
}
