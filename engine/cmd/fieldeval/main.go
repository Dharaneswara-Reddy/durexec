// Command fieldeval measures where the deciding fields of an action should
// come from.
//
// The engine's hybrid duplicate check says two calls are the same call only
// when every DECIDING FIELD matches exactly and the free text around them is
// similar enough. That design is only as good as the thing that finds the
// fields, and until now there was exactly one such thing:
// internal/idem/semantic/itinerary.go, a hand-written flight vocabulary whose
// own file admits it was written by reading the corpus it is scored on. The
// live crash-and-rephrase run then measured it reading 100% of flight intents
// and 0% of the other four domains'.
//
// So this command scores THREE answers to the same question, over the same
// pairs, with the engine's own comparator code:
//
//	itinerary  the hand-written flight vocabulary — the baseline
//	generic    value shapes, no vocabulary, no network, deterministic
//	llm        one model call per intent, returning the deciding values as JSON
//
// and two further things the same run can measure for free, because both are
// decisions about how a verdict is used rather than how it is computed:
//
//	GATE RULE          whether a field only one side stated closes the gate
//	                   (strict, the published rule) or is treated as no
//	                   information (neutral)
//	ESCALATION POLICY  whether an uncertain verdict runs the activity anyway
//	                   (the engine's default) or blocks it for a human
//	                   (ESCALATION_BLOCKS, a real engine setting that is off)
//
// Against them stand the two existing comparators, measured on the identical
// pairs so the comparison cannot be a difference in measurement: the engine's
// exact key, and the shipped pure-semantic path.
//
// THREE CORPORA, because no one of them can answer the question alone:
//
//	flight-213     the published synthetic flight corpus. Comparable with
//	               docs/results/semantic-experiment-*.
//	multidomain    300 pairs, ten domains, fifteen duplicates and fifteen
//	               designed negatives each. The only source of a
//	               false-positive rate per domain.
//	rephrase-live  the genuine rewordings a real planner produced in
//	               docs/results/rephrase-live-model.csv. Real wordings, real
//	               labels — and NO negative class, so it can report detection
//	               and nothing else.
//
// It needs the embedding service. It needs GROQ_API_KEY only for the LLM arms,
// which are skipped, loudly, without one — a skipped arm is reported as
// skipped and never as a zero.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "fieldeval: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	csvPath     string
	txtPath     string
	endpoint    string
	model       string
	dim         int
	timeout     time.Duration
	cacheDir    string
	llmCache    string
	repeat      int
	tauLow      float64
	tauHigh     float64
	tauMin      float64
	tauMax      float64
	tauStep     float64
	rate        float64
	withLLM     bool
	reportOnly  bool
	corpora     string
	rephraseCSV string
	minAgree    int
	progress    int
	limit       int
	render      string
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.csvPath, "csv", "fieldeval-rows.csv",
		"per-row CSV; one row per comparator per pair, written and flushed as it is measured")
	flag.StringVar(&c.txtPath, "txt", "fieldeval-report.txt",
		"where to write the report; every table in it is recomputed from the CSV on disk")
	flag.StringVar(&c.endpoint, "endpoint", "http://localhost:8000/embed", "embedding service URL")
	flag.StringVar(&c.model, "model", semantic.DefaultModel, "embedding model name, recorded in the output")
	flag.IntVar(&c.dim, "dim", semantic.DefaultDim, "embedding dimension")
	flag.DurationVar(&c.timeout, "timeout", semantic.DefaultTimeout, "per-embed timeout")
	flag.StringVar(&c.cacheDir, "cache-dir", defaultCacheDir(), "embedding cache directory")
	flag.StringVar(&c.llmCache, "llm-cache", defaultLLMCache(),
		"file holding the model's raw extraction replies, so a re-run costs no calls")
	flag.IntVar(&c.repeat, "repeat", 3, "how many times each check is timed")
	flag.Float64Var(&c.tauLow, "tau-low", 0.85, "lower edge of the shipped escalation band")
	flag.Float64Var(&c.tauHigh, "tau-high", 0.90, "upper edge of the shipped escalation band")
	flag.Float64Var(&c.tauMin, "tau-min", 0.30,
		"lowest threshold in the sweep. It reaches well below the shipped band on purpose: "+
			"the hybrid comparator scores a REMAINDER, which is short prose whose cosines sit "+
			"far lower than a whole sentence's, and a sweep that started at the shipped band "+
			"would not show that")
	flag.Float64Var(&c.tauMax, "tau-max", 0.99, "highest threshold in the sweep")
	flag.Float64Var(&c.tauStep, "tau-step", 0.05, "step between thresholds in the sweep")
	flag.Float64Var(&c.rate, "rate", 22, "model extraction calls per minute, to stay inside the free tier")
	flag.BoolVar(&c.withLLM, "llm", false, "measure the LLM extractor arms (needs GROQ_API_KEY)")
	flag.BoolVar(&c.reportOnly, "report-only", false,
		"do not measure anything; recompute the report from the CSV already on disk")
	flag.StringVar(&c.corpora, "corpora", "flight,multidomain,rephrase",
		"comma-separated list of corpora to score")
	flag.StringVar(&c.rephraseCSV, "rephrase-csv", "",
		"path to docs/results/rephrase-live-model.csv; empty looks beside the repository's docs/results")
	flag.IntVar(&c.minAgree, "min-agreeing", 1,
		"how many deciding fields must both be stated and agree before the NEUTRAL gate passes a pair")
	flag.IntVar(&c.progress, "progress-every", 100, "print a progress line every N rows measured")
	flag.StringVar(&c.render, "render", renderSentence,
		"how an activity's input becomes the text that is embedded and handed to an extractor. "+
			"'sentence' is what the ENGINE does for the agent's tool calls: cmd/engined wires "+
			"Checker.RenderIntent to app/agent's own renderer, which returns the bare canonical "+
			"argument sentence. 'envelope' is the checker's generic fallback, "+
			"`Type({\"args\":\"...\",\"tool\":\"Type\"})`, which is what cmd/experiment and the "+
			"published semantic-experiment result files embedded — mostly constant scaffolding "+
			"per activity type, so it inflates every cosine")
	flag.IntVar(&c.limit, "limit", 0,
		"score only the first N pairs of each corpus, for a smoke test. The corpora are "+
			"interleaved by domain and by label, so a prefix is a balanced sample rather than "+
			"the first domain only. Zero means all of them")
	flag.Parse()
	return c
}

func defaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ".experiment-cache"
	}
	// The SAME directory cmd/experiment uses, keyed by (model, text), so a
	// vector embedded by either harness is reused by the other. Two caches
	// would mean two sets of model round-trips for identical inputs.
	return filepath.Join(base, "durexec", "embeddings")
}

func defaultLLMCache() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ".fieldeval-llm-cache.json"
	}
	return filepath.Join(base, "durexec", "fieldeval-llm-replies.json")
}

// commandLine reproduces the invocation for the report header, rewriting the
// temporary path `go run` puts in argv[0] into the command a reader would
// actually have to type.
func commandLine() string {
	args := append([]string{}, os.Args...)
	if len(args) > 0 && strings.Contains(args[0], "/go-build") {
		args[0] = "go run ./cmd/fieldeval"
	}
	return strings.Join(args, " ")
}

// resumeCommand is what goes in the report header: the exact line that
// continues an interrupted run. It is the same command, because rows already
// in the CSV are skipped.
func resumeCommand(cfg config) string {
	return "set -a; . ./.env; set +a\n                 " + commandLine()
}

func run(cfg config) error {
	started := time.Now().UTC()

	pairs, prov, err := loadCorpora(cfg)
	if err != nil {
		return err
	}
	prov.StartedAt = started
	prov.CommandLine = commandLine()
	prov.ResumeCommand = resumeCommand(cfg)
	prov.CSVPath = cfg.csvPath
	prov.TauLow, prov.TauHigh = cfg.tauLow, cfg.tauHigh
	prov.MinAgreeing = cfg.minAgree
	prov.Repeat = cfg.repeat

	// The arm order is fixed by buildArms, including in report-only mode:
	// without it the comparators would be listed alphabetically and two
	// reports of the same data would read differently. The LLM arms are
	// included here whether or not a key is present, because this only
	// decides the ORDER of whatever rows the CSV turns out to hold.
	prov.Arms = armNames(buildArms(cfg, true))

	if cfg.reportOnly {
		// Recomputing from the CSV is not a convenience, it is the honesty
		// rule: a partial run must yield a valid partial report, and the only
		// way to be sure the report says what the file says is to build it
		// from the file.
		fmt.Printf("report-only: reading %s\n", cfg.csvPath)
		rows, err := readRows(cfg.csvPath)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("%s holds no rows, so there is nothing to report", cfg.csvPath)
		}
		prov.FinishedAt = time.Now().UTC()
		return writeReport(cfg, prov, rows)
	}

	ctx := context.Background()
	m, err := newMeasurer(ctx, cfg, prov)
	if err != nil {
		return err
	}
	defer m.Close()

	arms := buildArms(cfg, m.HaveLLM)
	prov.Arms = armNames(arms)
	prov.LLMSkipped = !m.HaveLLM && cfg.withLLM
	prov.LLMModel = m.LLMModel

	done, err := alreadyMeasured(cfg.csvPath)
	if err != nil {
		return err
	}
	writer, err := openRowWriter(cfg.csvPath)
	if err != nil {
		return err
	}
	defer writer.Close()

	total := len(pairs) * len(arms)
	fmt.Printf("%d pairs x %d comparators = %d rows; %d already on disk\n",
		len(pairs), len(arms), total, len(done))
	printHeader(prov, pairs)

	measured, skipped := 0, 0
	for _, a := range arms {
		for i := range pairs {
			key := rowKey(a.Name, pairs[i].Corpus, pairs[i].Pair.ID)
			if done[key] {
				skipped++
				continue
			}
			row, err := m.Measure(ctx, a, pairs[i])
			if err != nil {
				// One unmeasurable pair must not destroy the rest. The row is
				// written with its error in the note column so the gap is
				// visible in the data rather than only in a log.
				row.Note = "ERROR: " + err.Error()
				fmt.Fprintf(os.Stderr, "fieldeval: %s on %s: %v\n", a.Name, pairs[i].Pair.ID, err)
			}
			if err := writer.Write(row); err != nil {
				return fmt.Errorf("write row: %w", err)
			}
			measured++
			if cfg.progress > 0 && measured%cfg.progress == 0 {
				fmt.Printf("  %d/%d measured (%d skipped, %d model calls)\n",
					measured, total-skipped, skipped, m.LLMCalls())
			}
		}
	}
	fmt.Printf("measured %d rows, skipped %d already on disk, %d model extraction calls\n",
		measured, skipped, m.LLMCalls())

	// Close the writer before reading the file back, so the report cannot see
	// a half-flushed final row.
	if err := writer.Close(); err != nil {
		return err
	}
	rows, err := readRows(cfg.csvPath)
	if err != nil {
		return err
	}
	prov.FinishedAt = time.Now().UTC()
	prov.EmbedCalls, prov.EmbedP50, prov.EmbedP99 = m.EmbedStats()
	prov.LLMCallsMade = m.LLMCalls()
	prov.LLMNanos = m.LLMNanos()
	prov.LLMFailures = m.LLMFailures()
	return writeReport(cfg, prov, rows)
}

var errNoEmbedder = errors.New(
	"the embedding service did not answer a probe, so this run could only produce false numbers " +
		"and will not start")
