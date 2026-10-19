// Command rephrase measures the premise the project rests on.
//
// THE CLAIM. When an AI agent crashes mid-task and re-plans, it re-issues the
// same intention in different words. An exact-text duplicate check compares
// bytes, so it does not see the repeat, and the action happens twice. The
// project's answer is to compare what the two calls MEAN.
//
// The claim is general, so the measurement is too. Five unrelated domains —
// booking a flight, sending a payment, issuing a refund, scheduling a meeting,
// sending an email — chosen because the values that decide WHICH action it is
// are shaped differently in each: an ordered pair of places, a single sum of
// money, an opaque identifier, a time range, and almost nothing but free text.
// See domains.go.
//
// Every number this command prints comes from a real run. The planner is a
// live language model called over the network, on both sides of the crash; the
// similarity scores come from a real sentence encoder; the comparators are the
// engine's own code and not a copy of it. Nothing here is scripted, simulated
// or reworded by hand: the two sentences a trial compares were written by the
// model, independently, from the same goal.
//
// WHAT ONE TRIAL IS. app/agent's DoubleBooking fixture runs the agent
// workflow, kills it while a planning call is in flight, and lets a fresh
// incarnation recover and re-decide the step that was lost. With
// LoseUncommittedJournal set, the crash discards the uncommitted history, so
// the recovered incarnation plans the lost step from exactly the inputs the
// first one had — the same goal and an empty transcript. The side effect the
// first incarnation caused has still happened. So the two sentences are two
// independent answers to one identical question, and whether a duplicate check
// catches the second one decides whether the action happens twice.
//
// SURVIVING INTERRUPTION. Each trial is written to the CSV and flushed before
// the next one starts, and a restart skips trials already recorded. The report
// is computed from the CSV rather than from memory, so a run that is killed
// part way through still leaves both usable raw data and a valid partial
// report. -report-only regenerates everything from the CSV with no model and
// no encoder. See store.go.
//
// It needs GROQ_API_KEY in the environment and an embedding service to talk
// to. It will not fall back to a scripted planner or a hash embedder: a run
// that cannot reach the real model and the real encoder has nothing to
// measure, and should fail rather than produce numbers that look like results.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dharaneswar339/durexec/app/agent"
	"github.com/dharaneswar339/durexec/app/agent/llm"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/exact"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

func main() {
	if err := run(parseFlags()); err != nil {
		fmt.Fprintf(os.Stderr, "rephrase: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Configuration.
// ---------------------------------------------------------------------------

type config struct {
	trials      int
	controls    int
	endpoint    string
	timeout     time.Duration
	ratePerMin  float64
	attempts    int
	retryTrials int
	csvPath     string
	txtPath     string
	notesPath   string
	reportOnly  bool
	note        string
}

func parseFlags() config {
	var c config
	flag.IntVar(&c.trials, "trials", 50,
		"how many crash-and-replan trials to run; one trial proves nothing, the paper needs a rate")
	flag.IntVar(&c.controls, "controls", 10,
		"how many additional trials to run with the journal INTACT, which asks a different "+
			"question: handed a transcript that already shows the action, does the model repeat it?")
	flag.StringVar(&c.endpoint, "endpoint", "http://localhost:8000/embed",
		"embedding service URL; `make up-semantic` publishes all-MiniLM-L6-v2 here")
	flag.DurationVar(&c.timeout, "embed-timeout", 10*time.Second, "per-embed timeout")
	flag.Float64Var(&c.ratePerMin, "rate", 24,
		"planning calls per minute, paced evenly; Groq's free tier allows roughly 30, and "+
			"pacing under the limit loses fewer trials than racing it and backing off")
	flag.IntVar(&c.attempts, "attempts", 6,
		"attempts per planning call before giving up, for rate limits and transient 5xx")
	flag.IntVar(&c.retryTrials, "retry-trials", 2,
		"extra attempts at a whole trial when the model does something the scenario cannot "+
			"use, such as declaring itself done before acting")
	flag.StringVar(&c.csvPath, "csv", "rephrase-trials.csv",
		"path to write the per-trial raw data to, appended and flushed per trial; "+
			"rows already present are skipped, so the same command resumes an interrupted run")
	flag.StringVar(&c.txtPath, "txt", "",
		"path to write the report to; it is rewritten after every trial, so there is always "+
			"a current report on disk even if the run is killed")
	flag.StringVar(&c.notesPath, "notes", "",
		"path to write the CSV's companion notes to; empty writes <csv>.NOTES.txt")
	flag.BoolVar(&c.reportOnly, "report-only", false,
		"recompute the whole report from an existing CSV and exit; needs no API key and no "+
			"embedding service")
	flag.StringVar(&c.note, "note", "", "free-text note recorded in the report header")
	flag.Parse()
	return c
}

// ---------------------------------------------------------------------------
// One trial.
// ---------------------------------------------------------------------------

// outcome says what a trial turned out to be, before any comparator is asked.
// It is a type and not a boolean because two of the four values are neither
// success nor failure: they are the model doing something the scenario cannot
// use, and lumping them in with either would hide them.
type outcome string

const (
	// outcomeUsable: both incarnations invoked the domain's commit tool, so
	// there is a pair to compare. These are the only trials a catch rate is
	// computed over.
	outcomeUsable outcome = "usable"

	// outcomeNeverActed: the pre-crash incarnation spent its whole step budget
	// without calling the commit tool — it looked something up, or declared
	// itself done. There was no irreversible action for the crash to follow.
	outcomeNeverActed outcome = "never-acted"

	// outcomeNoRepeat: the recovered incarnation did not call the commit tool.
	// It may have called nothing at all, or only the read tool;
	// after_tool_call_count says which. On the control arm this is the
	// expected, correct behaviour and is the control's whole finding.
	outcomeNoRepeat outcome = "no-repeat"

	// outcomeFailed: the trial never produced a pair — the model returned
	// unusable JSON, or the quota ran out. Counted and named, never silently
	// dropped: trials that fail are not a random sample of trials attempted.
	outcomeFailed outcome = "failed"
)

// allOutcomes fixes the order outcomes are reported in, so two runs of this
// harness print comparable tables.
var allOutcomes = []outcome{outcomeUsable, outcomeNeverActed, outcomeNoRepeat, outcomeFailed}

// trial is one crash-and-replan run with everything measured about it. Every
// field here becomes a CSV column, because a reader has to be able to
// recompute each reported figure without rerunning the model.
type trial struct {
	Key        string
	Index      int
	Arm        string
	Domain     string
	CommitTool string
	Goal       goalSpec
	Outcome    outcome
	Err        string

	BeforeTool string
	Before     string
	AfterTool  string
	After      string

	// AfterCallCount is how many tool calls the recovered incarnation
	// executed. More than one means it did not simply redo the lost step.
	AfterCallCount int

	// SideEffects is how many times the commit tool really ran across both
	// incarnations, with no deduplication at all. Two is the double action.
	SideEffects int

	// --- what each comparator saw ---

	ExactKeyEqual  bool
	ByteIdentical  bool
	CanonicalEqual bool

	Similarity float64
	SemVerdict core.DedupVerdict

	HybridGated       bool
	HybridFellBack    bool
	HybridField       string
	HybridMineValue   string
	HybridTheirsValue string
	HybridSimilarity  float64
	HybridModelUsed   bool
	HybridVerdict     core.DedupVerdict

	// --- ground truth, read independently of the comparators ---

	Drifted              bool
	BeforeFields         string
	AfterFields          string
	BeforeContradictions string
	AfterContradictions  string
	BeforeMissing        string
	AfterMissing         string
}

const (
	armLostJournal   = "lost-journal"
	armJournalIntact = "journal-intact"
)

func keyFor(arm string, index int) string { return fmt.Sprintf("%s#%03d", arm, index) }

// run is the whole measurement.
func run(c config) error {
	ctx := context.Background()
	cmdLine := reproCommand(c)

	if c.notesPath == "" {
		c.notesPath = c.csvPath + ".NOTES.txt"
	}

	// Whatever a previous run already paid for.
	done, err := readStore(c.csvPath)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, t := range done {
		have[t.Key] = true
	}

	if c.reportOnly {
		if len(done) == 0 {
			return fmt.Errorf("-report-only: %s holds no trials", c.csvPath)
		}
		return finish(provenanceFrom(c, "", "", 0, 0, cmdLine), done, c, cmdLine, true)
	}

	planner := llm.NewGroqPlanner()
	if planner == nil {
		return errors.New("GROQ_API_KEY is not set; this harness measures a real model and " +
			"will not substitute a scripted one")
	}
	planner.Attempts = c.attempts

	// One limiter shared by every trial, because the quota is per key and not
	// per trial. Pacing evenly under the limit is what keeps a 429 from
	// turning into a lost trial.
	paced := &pacedPlanner{inner: planner, minGap: gapFor(c.ratePerMin)}

	emb := semantic.NewHTTPEmbedder(c.endpoint, c.timeout)
	if err := checkEmbedder(ctx, emb); err != nil {
		return err
	}

	// The engine's own comparators, configured the way the engine configures
	// them. Measuring a reimplementation would measure the reimplementation.
	hybrid := &semantic.Checker{
		Embedder:   emb,
		Thresholds: core.DefaultThresholds(),
		Clock:      core.SystemClock{},
		Fields:     semantic.ItineraryFields,
	}

	store, err := openStore(c.csvPath)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := os.WriteFile(c.notesPath, []byte(notesFor(c.csvPath, cmdLine)), 0o644); err != nil {
		return err
	}

	started := time.Now().UTC()
	trials := append([]trial(nil), done...)
	if len(done) > 0 {
		fmt.Fprintf(os.Stderr, "resuming: %d trial(s) already in %s\n", len(done), c.csvPath)
	}

	type job struct {
		arm   string
		index int
	}
	var jobs []job
	for i := 1; i <= c.trials; i++ {
		jobs = append(jobs, job{armLostJournal, i})
	}
	for i := 1; i <= c.controls; i++ {
		jobs = append(jobs, job{armJournalIntact, i})
	}

	for n, j := range jobs {
		key := keyFor(j.arm, j.index)
		if have[key] {
			continue
		}
		// Goal selection is deterministic in the trial number, which is what
		// makes the resume key meaningful: trial 17 is always goal 17.
		g := goals[(j.index-1)%len(goals)]
		t := runTrial(ctx, c, j.arm, j.index, g, paced, emb, hybrid)
		t.Key = key
		if err := store.Append(t); err != nil {
			return fmt.Errorf("writing trial %s: %w", key, err)
		}
		trials = append(trials, t)
		progress(n+1, len(jobs), t)

		// The report on disk is rewritten after every trial, so an interrupted
		// run still leaves a readable, correct, partial result rather than
		// only a CSV someone would have to process themselves.
		if c.txtPath != "" {
			prov := provenanceFrom(c, planner.Model(), emb.Model(), emb.Dim(), paced.Calls(), cmdLine)
			prov.StartedAt, prov.FinishedAt = started, time.Now().UTC()
			prov.Partial = true
			_ = writeReport(prov, trials, c.txtPath)
		}
	}

	prov := provenanceFrom(c, planner.Model(), emb.Model(), emb.Dim(), paced.Calls(), cmdLine)
	prov.StartedAt, prov.FinishedAt = started, time.Now().UTC()

	// Re-read from disk rather than reporting the in-memory slice. It costs
	// nothing and it proves the CSV is a complete record of the run: if the
	// report from the file differs from the run, the file is wrong, and the
	// file is the artefact that outlives the run.
	fromDisk, err := readStore(c.csvPath)
	if err != nil {
		return err
	}
	return finish(prov, fromDisk, c, cmdLine, false)
}

// finish writes the notes, the report file and the report to stdout.
func finish(prov provenance, trials []trial, c config, cmdLine string, reportOnly bool) error {
	if !reportOnly {
		if err := os.WriteFile(c.notesPath, []byte(notesFor(c.csvPath, cmdLine)), 0o644); err != nil {
			return err
		}
	}
	if c.txtPath != "" {
		if err := writeReport(prov, trials, c.txtPath); err != nil {
			return err
		}
	}
	report(os.Stdout, prov, trials)
	return nil
}

func provenanceFrom(c config, model, encoder string, dim, calls int, cmdLine string) provenance {
	return provenance{
		Model:      model,
		Encoder:    encoder,
		Dim:        dim,
		Endpoint:   c.endpoint,
		Thresholds: core.DefaultThresholds(),
		Temp:       llm.DefaultTemperature,
		RatePerMin: c.ratePerMin,
		Trials:     c.trials,
		Controls:   c.controls,
		ModelCalls: calls,
		CSVPath:    c.csvPath,
		NotesPath:  c.notesPath,
		Command:    cmdLine,
		Note:       c.note,
	}
}

func reproCommand(c config) string {
	return fmt.Sprintf(
		"go run ./cmd/rephrase -trials=%d -controls=%d -rate=%.0f -endpoint=%s -csv=%s -txt=%s",
		c.trials, c.controls, c.ratePerMin, c.endpoint, c.csvPath, c.txtPath)
}

// runTrial drives the fixture once, retrying only when the model did something
// the scenario cannot use at all.
//
// A retry re-rolls the SAME goal rather than moving on, so that the goals a
// rate is computed over are the goals the corpus declares and not whichever
// ones happened to work. A retried trial is a fresh set of model calls; no
// output from a discarded attempt reaches the data.
func runTrial(
	ctx context.Context, c config, arm string, index int, g goalSpec,
	planner *pacedPlanner, emb core.Embedder, hybrid *semantic.Checker,
) trial {
	var last trial
	for attempt := 0; attempt <= c.retryTrials; attempt++ {
		last = oneTrial(ctx, arm, index, g, planner, emb, hybrid)
		switch last.Outcome {
		case outcomeUsable:
			return last
		case outcomeNoRepeat:
			if arm == armJournalIntact {
				// The control arm is asking exactly this question. Declining
				// to repeat the action is its answer, not a failed attempt.
				return last
			}
		}
	}
	return last
}

func oneTrial(
	ctx context.Context, arm string, index int, g goalSpec,
	planner *pacedPlanner, emb core.Embedder, hybrid *semantic.Checker,
) trial {
	d := domains[g.Domain]
	t := trial{
		Index: index, Arm: arm, Goal: g, Domain: g.Domain,
		CommitTool: d.CommitTool.Name,
	}

	// The planner for this domain: the same client, the same decoding, the
	// same quota, and a prompt that differs only in the tool list. Everything
	// else being identical is what lets one domain's rephrase rate be read
	// against another's.
	domainPlanner := planner.withPrompt(d.Prompt())

	effects := &sideEffects{}
	f := agent.NewDoubleBooking()
	f.Goal = g.Text
	f.Tools = toolsFor(d, effects)
	f.CrashAfterTool = d.CommitTool.Name
	// The same planner object on both sides. That is not a shortcut: a Groq
	// planner holds no conversation state at all — it sends the goal and the
	// transcript the engine hands it and nothing else — so two instances would
	// behave identically and would only make the rate limiter easier to
	// mismanage. The independence the experiment needs comes from the
	// transcript, not from the client.
	f.BeforeCrash = domainPlanner
	f.AfterCrash = domainPlanner
	f.LoseUncommittedJournal = arm == armLostJournal

	trace, _, err := f.Simulate()
	t.SideEffects = effects.Count()
	if err != nil {
		t.Outcome = outcomeFailed
		if errors.Is(err, agent.ErrNeverReachedCrash) {
			t.Outcome = outcomeNeverActed
		}
		t.Err = firstLine(err.Error())
		return t
	}

	t.BeforeTool, t.Before = trace.Before.Tool, trace.Before.Args
	t.AfterTool, t.After = trace.After.Tool, trace.After.Args
	t.AfterCallCount = len(trace.AfterCalls)

	if t.AfterTool != d.CommitTool.Name {
		// Simulate leaves After at its zero value when the recovered
		// incarnation never called the watched tool, whether it did nothing or
		// only looked something up.
		t.Outcome = outcomeNoRepeat
		return t
	}
	t.Outcome = outcomeUsable

	// Ground truth first, and from the goal rather than from the other
	// sentence, so that a drifted trial is identified before any comparator
	// gets a chance to call it a duplicate.
	beforeRead := read(g, t.Before)
	afterRead := read(g, t.After)
	t.Drifted = beforeRead.Drifted() || afterRead.Drifted()
	t.BeforeFields = joinOr(beforeRead.Stated(), "")
	t.AfterFields = joinOr(afterRead.Stated(), "")
	t.BeforeContradictions = joinOr(beforeRead.Contradictions(), "")
	t.AfterContradictions = joinOr(afterRead.Contradictions(), "")
	t.BeforeMissing = joinOr(beforeRead.Missing(), "")
	t.AfterMissing = joinOr(afterRead.Missing(), "")

	if err := scoreComparators(ctx, &t, emb, hybrid); err != nil {
		t.Outcome, t.Err = outcomeFailed, firstLine(err.Error())
	}
	return t
}

// scoreComparators asks all three duplicate checks about the one pair.
func scoreComparators(ctx context.Context, t *trial, emb core.Embedder, hybrid *semantic.Checker) error {
	// EXACT, as the engine computes it: a canonical hash of the activity's
	// input. Comparing the engine's real key rather than the two strings is
	// what makes this the baseline and not an approximation of it.
	beforeKey, err := exactKey(t.CommitTool, t.Before)
	if err != nil {
		return err
	}
	afterKey, err := exactKey(t.CommitTool, t.After)
	if err != nil {
		return err
	}
	t.ExactKeyEqual = beforeKey == afterKey
	t.ByteIdentical = t.Before == t.After
	t.CanonicalEqual = agent.CanonicalIntent(t.Before) == agent.CanonicalIntent(t.After)

	// PURE SEMANTIC, over the same text the engine would embed.
	mine := agent.CanonicalIntent(t.After)
	theirs := agent.CanonicalIntent(t.Before)
	va, err := emb.Embed(ctx, mine)
	if err != nil {
		return fmt.Errorf("embed the post-crash intent: %w", err)
	}
	vb, err := emb.Embed(ctx, theirs)
	if err != nil {
		return fmt.Errorf("embed the pre-crash intent: %w", err)
	}
	t.Similarity = semantic.Cosine(va, vb)
	t.SemVerdict = core.DefaultThresholds().Classify(t.Similarity)

	// HYBRID. Compare() is the identical call the engine's checkHybrid makes
	// per candidate, so the verdict below is the engine's verdict and not a
	// harness's idea of it.
	cmp, err := hybrid.Compare(ctx, t.CommitTool, mine, theirs)
	if err != nil {
		return fmt.Errorf("hybrid compare: %w", err)
	}
	switch {
	case !cmp.MineReadable:
		// The extractor could not read the new call at all. The engine falls
		// back to the pure-semantic verdict and records that it did; so does
		// this, because a hybrid figure that quietly borrowed the semantic
		// answer would overstate the gate's coverage.
		t.HybridFellBack = true
		t.HybridVerdict = t.SemVerdict
		t.HybridSimilarity = t.Similarity
	case cmp.Gated:
		t.HybridGated = true
		t.HybridField = cmp.DifferingField
		t.HybridMineValue, t.HybridTheirsValue = cmp.MineValue, cmp.TheirsValue
		t.HybridVerdict = core.VerdictDistinct
	default:
		t.HybridSimilarity = cmp.Similarity
		t.HybridModelUsed = cmp.ModelUsed
		t.HybridVerdict = core.DefaultThresholds().Classify(cmp.Similarity)
	}
	return nil
}

// exactKey is the engine's exact idempotency key for a call with these
// arguments.
func exactKey(tool, args string) (string, error) {
	raw, err := json.Marshal(agent.ToolCall{Tool: tool, Args: args})
	if err != nil {
		return "", err
	}
	return exact.Key(tool, raw)
}

// ---------------------------------------------------------------------------
// Pacing.
// ---------------------------------------------------------------------------

// pacedPlanner spaces planning calls evenly.
//
// It is a planner rather than a limiter the harness calls, because the calls
// are made from inside the fixture and the harness never sees them. Wrapping
// the planner is the only place that can count every one.
type pacedPlanner struct {
	inner  *llm.GroqPlanner
	minGap time.Duration
	prompt string

	mu   *sync.Mutex
	next *time.Time

	// calls counts every planning call that was let through, so the report can
	// state what the run cost in model calls rather than estimate it. It is a
	// pointer because withPrompt hands out a planner per domain and all of
	// them spend one budget; a per-copy counter would report a fifth of the
	// truth.
	calls *int
}

// Calls reports how many planning calls have been let through.
func (p *pacedPlanner) Calls() int {
	p.ensure()
	p.mu.Lock()
	defer p.mu.Unlock()
	return *p.calls
}

// withPrompt returns a planner for one domain that shares this one's rate
// budget. The clock and the mutex are shared by pointer deliberately: a
// per-domain copy with its own limiter would let five domains each run at the
// full rate and exhaust the quota five times over.
func (p *pacedPlanner) withPrompt(prompt string) *pacedPlanner {
	p.ensure()
	return &pacedPlanner{
		inner: p.inner, minGap: p.minGap, prompt: prompt,
		mu: p.mu, next: p.next, calls: p.calls,
	}
}

func (p *pacedPlanner) ensure() {
	if p.mu == nil {
		p.mu = &sync.Mutex{}
		p.next = new(time.Time)
		p.calls = new(int)
	}
}

func (p *pacedPlanner) Plan(ctx context.Context, in agent.PlanInput) (agent.Plan, error) {
	p.ensure()
	p.mu.Lock()
	now := time.Now()
	wait := time.Duration(0)
	if now.Before(*p.next) {
		wait = p.next.Sub(now)
	}
	*p.next = now.Add(wait).Add(p.minGap)
	*p.calls++
	p.mu.Unlock()

	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return agent.Plan{}, ctx.Err()
		case <-t.C:
		}
	}

	// The prompt is set per call rather than per client so that one rate
	// limiter and one HTTP client serve all five domains.
	caller := *p.inner
	caller.Prompt = p.prompt
	return caller.Plan(ctx, in)
}

func gapFor(perMin float64) time.Duration {
	if perMin <= 0 {
		return 0
	}
	return time.Duration(float64(time.Minute) / perMin)
}

// checkEmbedder refuses to start on an encoder that is not answering.
//
// Failing here rather than per trial is deliberate: HTTPEmbedder collapses
// every transport failure into ErrEmbeddingUnavailable so the engine can
// degrade, and a harness that degraded would print similarity scores of zero
// as though they had been measured.
func checkEmbedder(ctx context.Context, emb *semantic.HTTPEmbedder) error {
	v, err := emb.Embed(ctx, "harness reachability probe")
	if err != nil {
		return fmt.Errorf("the embedding service is not usable (%w); start it with "+
			"`make up-semantic` — this harness will not substitute the hash embedder", err)
	}
	if len(v) != emb.Dim() {
		return fmt.Errorf("encoder returned %d dimensions, expected %d", len(v), emb.Dim())
	}
	return nil
}

func progress(done, total int, t trial) {
	detail := string(t.Outcome)
	if t.Outcome == outcomeUsable {
		detail = fmt.Sprintf("%s  sim=%.4f  sem=%-9s hyb=%s", wordedOrNot(t),
			t.Similarity, t.SemVerdict, t.HybridVerdict)
	}
	if t.Err != "" {
		detail += "  (" + t.Err + ")"
	}
	fmt.Fprintf(os.Stderr, "[%3d/%3d] %-4s %-9s %-14s %s\n",
		done, total, t.Goal.ID, t.Domain, t.Arm, detail)
}

func wordedOrNot(t trial) string {
	switch {
	case t.ByteIdentical:
		return "identical"
	case t.Drifted:
		return "DRIFTED  "
	default:
		return "reworded "
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// Small shared helpers for the report.
// ---------------------------------------------------------------------------

// pct renders n out of d as a percentage, and says so plainly when there is
// nothing to take a percentage of. A zero denominator printed as "0.0%" is a
// claim about a measurement that was never made.
func pct(n, d int) string {
	if d == 0 {
		return "   n/a"
	}
	return fmt.Sprintf("%5.1f%%", 100*float64(n)/float64(d))
}

func sortedFloats(xs []float64) []float64 {
	out := append([]float64(nil), xs...)
	sort.Float64s(out)
	return out
}

// quantile is the nearest-rank percentile. With a few dozen samples there is
// no reason to interpolate, and interpolating would invent a score no pair had.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := int(math.Round(q * float64(len(sorted)-1)))
	return sorted[i]
}
