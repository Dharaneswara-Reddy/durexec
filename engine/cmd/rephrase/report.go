package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// Reporting.
// ---------------------------------------------------------------------------
//
// The shape of this report follows the other files in docs/results/: a
// provenance header that names every moving part, then a section saying how to
// read the classification columns, then the measurements, then a closing
// section that separates what was measured from what was assumed and from what
// this run cannot say anything about. The last section is not politeness. A
// result file that does not fence off its own limits invites being quoted past
// them.
//
// Everything below reads the trials and nothing else, so the same code
// produces the report during a run, after a run, and from a CSV alone.

// provenance is everything a reader needs in order to know what produced the
// numbers below it.
type provenance struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Model      string
	Encoder    string
	Dim        int
	Endpoint   string
	Thresholds core.Thresholds
	Temp       float64
	RatePerMin float64
	Trials     int
	Controls   int
	ModelCalls int
	CSVPath    string
	NotesPath  string
	Command    string
	Note       string

	// Partial marks a report written while the run is still going. A reader
	// who finds the file on disk has to be able to tell a finished measurement
	// from a snapshot of one in progress.
	Partial bool
}

// writeReport renders the report to a file atomically-ish: it is built in
// memory first, so a reader who opens the file mid-write sees the previous
// complete report rather than half of this one.
func writeReport(prov provenance, trials []trial, path string) error {
	var buf bytes.Buffer
	report(&buf, prov, trials)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// report prints everything.
func report(w io.Writer, prov provenance, trials []trial) {
	main, controls := split(trials)

	printHeader(w, prov, trials)
	printHowToRead(w)
	printOutcomes(w, main, controls)
	printPremise(w, main)
	printPerDomain(w, main)
	printDrift(w, main)
	printCatchRates(w, main)
	printDoubleActions(w, main)
	printDistribution(w, main)
	printGate(w, main)
	printFalseSuppression(w, main)
	printControls(w, controls)
	printSweep(w, main)
	printLimits(w, prov, main, controls)
}

func split(trials []trial) (main, controls []trial) {
	for _, t := range trials {
		if t.Arm == armJournalIntact {
			controls = append(controls, t)
		} else {
			main = append(main, t)
		}
	}
	return main, controls
}

// usable filters to the trials that produced a pair of commit sentences.
func usable(ts []trial) []trial {
	var out []trial
	for _, t := range ts {
		if t.Outcome == outcomeUsable {
			out = append(out, t)
		}
	}
	return out
}

// genuine filters to the usable trials that are genuine rewordings: the model
// changed the words and did not change the action. These are the only trials a
// catch rate can honestly be computed over.
func genuine(ts []trial) []trial {
	var out []trial
	for _, t := range usable(ts) {
		if !t.Drifted && !t.ByteIdentical {
			out = append(out, t)
		}
	}
	return out
}

func drifted(ts []trial) []trial {
	var out []trial
	for _, t := range usable(ts) {
		if t.Drifted {
			out = append(out, t)
		}
	}
	return out
}

func inDomain(ts []trial, name string) []trial {
	var out []trial
	for _, t := range ts {
		if t.Domain == name {
			out = append(out, t)
		}
	}
	return out
}

const rule = "==========================================================================="

func printHeader(w io.Writer, p provenance, trials []trial) {
	fmt.Fprintln(w, rule)
	fmt.Fprintln(w, " CRASH AND REPHRASE — DOES A REAL MODEL REWORD A LOST ACTION, AND WHO CATCHES IT")
	fmt.Fprintln(w, rule)
	if p.Partial {
		fmt.Fprintf(w, " STATUS        : PARTIAL — written while the run was still going. %d trial(s) so far.\n",
			len(trials))
		fmt.Fprintln(w, "                 Regenerate with -report-only when the run ends.")
	}
	if !p.StartedAt.IsZero() {
		fmt.Fprintf(w, " run at        : %s\n", p.StartedAt.Format(time.RFC3339))
		fmt.Fprintf(w, " finished at   : %s (wall clock %s)\n",
			p.FinishedAt.Format(time.RFC3339), p.FinishedAt.Sub(p.StartedAt).Round(time.Second))
	}
	if p.Model == "" {
		fmt.Fprintln(w, " NOTE          : regenerated from the CSV with -report-only. The planner and encoder")
		fmt.Fprintln(w, "                 named in the original run's header are the ones that produced these rows.")
	} else {
		fmt.Fprintf(w, " planner       : LIVE language model, %s, over the network\n", p.Model)
		fmt.Fprintf(w, " decoding      : temperature %.1f, max_tokens 300, no tool-calling API, JSON in the message body\n", p.Temp)
		fmt.Fprintf(w, " embedder      : HTTPEmbedder, model %s at %s (dim=%d)\n", p.Encoder, p.Endpoint, p.Dim)
		fmt.Fprintf(w, " pacing        : %.0f planning calls per minute, spaced evenly\n", p.RatePerMin)
		fmt.Fprintf(w, " model calls   : %d let through the limiter this run (counted, not estimated; one\n", p.ModelCalls)
		fmt.Fprintln(w, "                 call may have been sent twice if the provider rate-limited it)")
	}
	fmt.Fprintln(w, " comparators   : exact (the engine's own key), semantic (shipped), hybrid (opt-in, deciding fields exact)")
	fmt.Fprintf(w, " thresholds    : tau_low %.2f, tau_high %.2f — THE SHIPPED VALUES, not tuned for this run\n",
		p.Thresholds.TauLow, p.Thresholds.TauHigh)
	fmt.Fprintln(w, " policy        : the engine's default — DUPLICATE skips the activity, ESCALATE and DISTINCT both RUN it")
	fmt.Fprintf(w, " domains       : %s\n", strings.Join(domainOrder, ", "))
	fmt.Fprintf(w, " goals         : %d declared requests, %d per domain, interleaved so any prefix is balanced\n",
		len(goals), len(goals)/len(domainOrder))
	fmt.Fprintf(w, " trials        : %d recorded (%d on the lost-journal arm, %d on the journal-intact control arm)\n",
		len(trials), p.Trials, p.Controls)
	fmt.Fprintln(w, " labels        : NOT generated by construction. Every sentence compared here was written by")
	fmt.Fprintln(w, "                 the model. The only hand-made thing is the goal each trial starts from and")
	fmt.Fprintln(w, "                 the fields that goal declares, which is what \"drift\" is measured against.")
	fmt.Fprintf(w, " raw data      : %s — one row per trial, written and flushed as each trial finished\n", p.CSVPath)
	if p.NotesPath != "" {
		fmt.Fprintf(w, " column notes  : %s\n", p.NotesPath)
	}
	if p.Note != "" {
		fmt.Fprintf(w, " note          : %s\n", p.Note)
	}
	fmt.Fprintln(w, " command       : set -a; . ./.env; set +a")
	if p.Command != "" {
		fmt.Fprintf(w, "                 %s\n", p.Command)
		fmt.Fprintln(w, " resume        : the same command again. Trials already in the CSV are skipped.")
		fmt.Fprintf(w, " report only   : go run ./cmd/rephrase -report-only -csv=%s\n", p.CSVPath)
	}
	fmt.Fprintln(w)
}

func printHowToRead(w io.Writer) {
	fmt.Fprintln(w, "HOW TO READ THE OUTCOME COLUMN")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " Four outcomes, and two of them are neither a success nor a failure. They are the")
	fmt.Fprintln(w, " model doing something the scenario cannot use, and they are named rather than folded")
	fmt.Fprintln(w, " into a rate, because a rate computed over only the convenient trials is not a rate.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  usable       both incarnations invoked the domain's commit tool — the one with a side")
	fmt.Fprintln(w, "               effect. There is a pair to compare, and these are the ONLY trials any")
	fmt.Fprintln(w, "               catch rate below is computed over.")
	fmt.Fprintln(w, "  never-acted  the pre-crash incarnation spent its whole step budget without invoking")
	fmt.Fprintln(w, "               the commit tool, so there was no irreversible action for the crash to")
	fmt.Fprintln(w, "               follow. Nothing was repeated and nothing was at risk.")
	fmt.Fprintln(w, "  no-repeat    the recovered incarnation did not invoke the commit tool. It may have")
	fmt.Fprintln(w, "               done nothing or only looked something up; after_tool_call_count in the")
	fmt.Fprintln(w, "               CSV says which. On the control arm this is the CORRECT answer and is")
	fmt.Fprintln(w, "               that arm's entire finding.")
	fmt.Fprintln(w, "  failed       no pair was produced — unusable JSON from the model, or quota exhausted.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " And three classes WITHIN the usable trials, from ground truth read independently of")
	fmt.Fprintln(w, " every comparator (cmd/rephrase/truth.go, deliberately not the engine's extractor):")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  identical    the model produced the same bytes twice. Exact matching catches these,")
	fmt.Fprintln(w, "               and they are evidence AGAINST the project's premise, not for it.")
	fmt.Fprintln(w, "  reworded     different bytes, same action. This is the case the project is about.")
	fmt.Fprintln(w, "  drifted      one of the two sentences contradicts the goal on a deciding field. The")
	fmt.Fprintln(w, "               model did not reword an action, it planned a DIFFERENT one. Reported")
	fmt.Fprintln(w, "               separately and counted in NEITHER the catch rate nor the miss rate: on a")
	fmt.Fprintln(w, "               drifted trial the second action is correct, and a comparator that")
	fmt.Fprintln(w, "               suppressed it would have destroyed real work.")
	fmt.Fprintln(w)
}

func printOutcomes(w io.Writer, main, controls []trial) {
	fmt.Fprintln(w, "TRIAL OUTCOMES")
	fmt.Fprintln(w)
	const bar = " -------------+--------------+----------------"
	fmt.Fprintln(w, " outcome      | lost-journal | journal-intact")
	fmt.Fprintln(w, bar)
	for _, o := range allOutcomes {
		fmt.Fprintf(w, " %-12s | %12d | %14d\n", o, countOutcome(main, o), countOutcome(controls, o))
	}
	fmt.Fprintln(w, bar)
	fmt.Fprintf(w, " %-12s | %12d | %14d\n", "total", len(main), len(controls))
	fmt.Fprintln(w)
	if failed := countOutcome(main, outcomeFailed); failed > 0 {
		fmt.Fprintf(w, " %d trial(s) on the main arm failed. Their error text is in the CSV. They are\n", failed)
		fmt.Fprintln(w, " excluded from every rate below, and that exclusion is itself a caveat: trials")
		fmt.Fprintln(w, " that fail are not a random sample of trials attempted.")
		fmt.Fprintln(w)
	}
}

func countOutcome(ts []trial, o outcome) int {
	n := 0
	for _, t := range ts {
		if t.Outcome == o {
			n++
		}
	}
	return n
}

func printPremise(w io.Writer, main []trial) {
	u := usable(main)
	identical, reworded := 0, 0
	for _, t := range u {
		switch {
		case t.ByteIdentical:
			identical++
		case !t.Drifted:
			reworded++
		}
	}
	drift := len(drifted(main))
	changed := len(u) - identical

	fmt.Fprintln(w, "THE PREMISE: DOES THE MODEL REWORD AT ALL?")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " This is the finding the whole project depends on. If the model reliably repeats")
	fmt.Fprintln(w, " itself, an exact-text duplicate check is sufficient and nothing needs to be")
	fmt.Fprintln(w, " compared by meaning.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, " usable trials                              %4d\n", len(u))
	fmt.Fprintf(w, " byte-identical on both sides               %4d   %s\n", identical, pct(identical, len(u)))
	fmt.Fprintf(w, " different bytes (reworded OR drifted)      %4d   %s   <- the rephrase rate\n", changed, pct(changed, len(u)))
	fmt.Fprintf(w, "   of those, same action (GENUINE reword)   %4d   %s of usable\n", reworded, pct(reworded, len(u)))
	fmt.Fprintf(w, "   of those, a different action (drift)     %4d   %s of usable\n", drift, pct(drift, len(u)))
	fmt.Fprintln(w)
	canon := 0
	for _, t := range u {
		if !t.ByteIdentical && t.CanonicalEqual {
			canon++
		}
	}
	fmt.Fprintln(w, " Also measured, because the fixture has always claimed it: whether cheap")
	fmt.Fprintln(w, " normalisation would have been enough. If trimming and lowercasing closes the gap,")
	fmt.Fprintln(w, " no embedding is needed.")
	fmt.Fprintf(w, " differing pairs that trimming and lowercasing would match   %4d   %s\n",
		canon, pct(canon, changed))
	fmt.Fprintln(w)
}

// printPerDomain is the table the generality of the claim rests on. A rephrase
// rate that held in one domain and nowhere else would be a fact about flights.
func printPerDomain(w io.Writer, main []trial) {
	fmt.Fprintln(w, "BY DOMAIN")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " The claim is not about flights, so it is measured in five domains whose deciding")
	fmt.Fprintln(w, " fields are shaped differently: an ordered pair of places; one sum of money and a")
	fmt.Fprintln(w, " recipient; an opaque identifier and a reason; a person, a day, a time and a length;")
	fmt.Fprintln(w, " and one addressee with a body that is almost all free text.")
	fmt.Fprintln(w)
	const bar = " ---------+-------+--------+-------+----------+-------+-------+--------+--------"
	fmt.Fprintln(w, " domain   | usable| ident. | drift | reworded | exact |  sem  | hybrid | gate read")
	fmt.Fprintln(w, bar)
	for _, name := range domainOrder {
		d := inDomain(main, name)
		u := usable(d)
		g := genuine(d)
		ident := 0
		for _, t := range u {
			if t.ByteIdentical {
				ident++
			}
		}
		exactDup := countIf(g, func(t trial) bool { return t.ExactKeyEqual })
		semDup, _, _ := tally(g, func(t trial) core.DedupVerdict { return t.SemVerdict })
		hybDup, _, _ := tally(g, func(t trial) core.DedupVerdict { return t.HybridVerdict })
		recognised := countIf(u, func(t trial) bool { return !t.HybridFellBack })
		fmt.Fprintf(w, " %-8s | %5d | %6d | %5d | %8d | %s | %s | %s | %s\n",
			name, len(u), ident, len(drifted(d)), len(g),
			pct(exactDup, len(g)), pct(semDup, len(g)), pct(hybDup, len(g)),
			pct(recognised, len(u)))
	}
	fmt.Fprintln(w, bar)
	fmt.Fprintln(w)
	fmt.Fprintln(w, " exact/sem/hybrid are the share of that domain's GENUINE rewordings each comparator")
	fmt.Fprintln(w, " called DUPLICATE — the only verdict that stops the repeat happening.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " \"gate read\" is the share of usable trials in which the engine's deciding-field")
	fmt.Fprintln(w, " extractor could read the intent at all. The engine has exactly one extractor and it")
	fmt.Fprintln(w, " is flight-shaped (internal/idem/semantic/itinerary.go: airports, weekdays, dayparts,")
	fmt.Fprintln(w, " passenger counts). Where this number is low, the hybrid column above is NOT the")
	fmt.Fprintln(w, " hybrid comparator working — it is the hybrid comparator falling back to the")
	fmt.Fprintln(w, " pure-semantic path and reporting its verdict. No extractor was written for the other")
	fmt.Fprintln(w, " four domains on purpose: writing five scrapers fitted to five corpora is the")
	fmt.Fprintln(w, " criticism the existing hybrid result file already makes of itself, and how far one")
	fmt.Fprintln(w, " hand-written schema generalises IS the finding.")
	fmt.Fprintln(w)
}

func countIf(ts []trial, pred func(trial) bool) int {
	n := 0
	for _, t := range ts {
		if pred(t) {
			n++
		}
	}
	return n
}

func printDrift(w io.Writer, main []trial) {
	d := drifted(main)
	fmt.Fprintln(w, "DRIFT: TRIALS WHERE THE MODEL PLANNED A DIFFERENT ACTION")
	fmt.Fprintln(w)
	if len(d) == 0 {
		fmt.Fprintln(w, " None. On every usable trial both sentences agreed with the goal on every deciding")
		fmt.Fprintln(w, " field the goal declared.")
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, " %d of %d usable trials drifted. Each is listed with the contradiction that\n",
		len(d), len(usable(main)))
	fmt.Fprintln(w, " classified it, so a reader can disagree with the classification by eye.")
	fmt.Fprintln(w)
	for _, t := range d {
		fmt.Fprintf(w, " %s [%s]  goal: %s\n", t.Goal.ID, t.Domain, t.Goal.Text)
		fmt.Fprintf(w, "      before: %q\n", t.Before)
		fmt.Fprintf(w, "      after : %q\n", t.After)
		fmt.Fprintf(w, "      why   : %s\n", joinOr(nonEmpty(t.BeforeContradictions, t.AfterContradictions), "(none)"))
	}
	fmt.Fprintln(w)
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func printCatchRates(w io.Writer, main []trial) {
	g := genuine(main)
	fmt.Fprintln(w, "CATCH RATE ON THE GENUINE REWORDINGS, AT THE SHIPPED THRESHOLDS")
	fmt.Fprintln(w)
	if len(g) == 0 {
		fmt.Fprintln(w, " There were no genuine rewordings, so there is no catch rate to report. That is a")
		fmt.Fprintln(w, " finding about the premise, not a missing measurement.")
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, " Denominator: %d trials where the model changed the words and kept the action.\n", len(g))
	fmt.Fprintln(w, " A catch is a DUPLICATE verdict, which is the only verdict that stops the repeat.")
	fmt.Fprintln(w, " ESCALATE does not: under the engine's default policy the activity runs.")
	fmt.Fprintln(w)
	const bar = " -----------+-----------+----------+----------+----------+------------------"
	fmt.Fprintln(w, " comparator | DUPLICATE | ESCALATE | DISTINCT |  caught  | repeat prevented")
	fmt.Fprintln(w, bar)

	exactDup := countIf(g, func(t trial) bool { return t.ExactKeyEqual })
	fmt.Fprintf(w, " %-10s | %9d | %8d | %8d | %s  | %d of %d\n",
		"exact", exactDup, 0, len(g)-exactDup, pct(exactDup, len(g)), exactDup, len(g))
	for _, c := range []struct {
		name string
		get  func(trial) core.DedupVerdict
	}{
		{"semantic", func(t trial) core.DedupVerdict { return t.SemVerdict }},
		{"hybrid", func(t trial) core.DedupVerdict { return t.HybridVerdict }},
	} {
		dup, esc, dis := tally(g, c.get)
		fmt.Fprintf(w, " %-10s | %9d | %8d | %8d | %s  | %d of %d\n",
			c.name, dup, esc, dis, pct(dup, len(g)), dup, len(g))
	}
	fmt.Fprintln(w, bar)
	fmt.Fprintln(w)
	fmt.Fprintln(w, " exact is byte equality of the engine's own canonical idempotency key, not a")
	fmt.Fprintln(w, " weakened stand-in for it. It can only ever report DUPLICATE or DISTINCT.")
	fmt.Fprintln(w)
}

func tally(ts []trial, get func(trial) core.DedupVerdict) (dup, esc, dis int) {
	for _, t := range ts {
		switch get(t) {
		case core.VerdictDuplicate:
			dup++
		case core.VerdictEscalate:
			esc++
		default:
			dis++
		}
	}
	return dup, esc, dis
}

func printDoubleActions(w io.Writer, main []trial) {
	g := genuine(main)
	if len(g) == 0 {
		return
	}
	fmt.Fprintln(w, "REPEATED ACTIONS THAT WOULD ACTUALLY HAVE HAPPENED")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " Under the default policy — DUPLICATE skips, ESCALATE and DISTINCT both run — the")
	fmt.Fprintln(w, " action happens a second time on every genuine rewording the comparator did not")
	fmt.Fprintf(w, " call a DUPLICATE. These counts are over the same %d trials.\n", len(g))
	fmt.Fprintln(w)
	const bar = " ----------------+-----------------+----------"
	fmt.Fprintln(w, " comparator      | repeated actions| of trials")
	fmt.Fprintln(w, bar)
	exactDup := countIf(g, func(t trial) bool { return t.ExactKeyEqual })
	semDup, _, _ := tally(g, func(t trial) core.DedupVerdict { return t.SemVerdict })
	hybDup, _, _ := tally(g, func(t trial) core.DedupVerdict { return t.HybridVerdict })
	fmt.Fprintf(w, " %-15s | %15d | %d\n", "no check at all", len(g), len(g))
	fmt.Fprintf(w, " %-15s | %15d | %d\n", "exact", len(g)-exactDup, len(g))
	fmt.Fprintf(w, " %-15s | %15d | %d\n", "semantic", len(g)-semDup, len(g))
	fmt.Fprintf(w, " %-15s | %15d | %d\n", "hybrid", len(g)-hybDup, len(g))
	fmt.Fprintln(w, bar)
	fmt.Fprintln(w)
	effects := 0
	for _, t := range g {
		if t.SideEffects >= 2 {
			effects++
		}
	}
	fmt.Fprintf(w, " \"no check at all\" is not a hypothetical. The commit tool really ran twice on %d\n", effects)
	fmt.Fprintf(w, " of these %d trials, counted by the tool itself (side_effects in the CSV), because\n", len(g))
	fmt.Fprintln(w, " the simulator has no deduplication path at all. The engine's exact key does not")
	fmt.Fprintln(w, " help, because the two calls are not byte-equal.")
	fmt.Fprintln(w)
}

func printDistribution(w io.Writer, main []trial) {
	g := genuine(main)
	if len(g) == 0 {
		return
	}
	var sims []float64
	for _, t := range g {
		sims = append(sims, t.Similarity)
	}
	sorted := sortedFloats(sims)

	fmt.Fprintln(w, "DISTRIBUTION OF THE REAL SIMILARITY SCORES")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " Cosine between the two canonicalised commit sentences, from the real encoder,")
	fmt.Fprintf(w, " over the %d genuine rewordings.\n", len(g))
	fmt.Fprintln(w)
	fmt.Fprintf(w, " min %.4f   p10 %.4f   median %.4f   p90 %.4f   max %.4f\n",
		sorted[0], quantile(sorted, 0.10), quantile(sorted, 0.50),
		quantile(sorted, 0.90), sorted[len(sorted)-1])
	fmt.Fprintln(w)
	fmt.Fprintln(w, " bucket        | n   | histogram")
	fmt.Fprintln(w, " --------------+-----+----------------------------------------")
	below := 0
	for _, s := range sims {
		if s < 0.50 {
			below++
		}
	}
	fmt.Fprintf(w, " below 0.50    | %3d | %s\n", below, strings.Repeat("#", below))
	for lo := 0.50; lo < 0.999; lo += 0.05 {
		hi := lo + 0.05
		n := 0
		for _, s := range sims {
			if s >= lo && (s < hi || (hi > 0.999 && s <= 1.0)) {
				n++
			}
		}
		mark := ""
		if lo > 0.849 && lo < 0.851 {
			mark = "  <- the escalate band"
		}
		fmt.Fprintf(w, " [%.2f, %.2f) | %3d | %s%s\n", lo, hi, n, strings.Repeat("#", n), mark)
	}
	fmt.Fprintln(w)

	th := core.DefaultThresholds()
	band, above, under := 0, 0, 0
	for _, s := range sims {
		switch {
		case s >= th.TauHigh:
			above++
		case s >= th.TauLow:
			band++
		default:
			under++
		}
	}
	fmt.Fprintf(w, " at or above tau_high %.2f (DUPLICATE)      %3d   %s\n", th.TauHigh, above, pct(above, len(g)))
	fmt.Fprintf(w, " in the escalate band [%.2f, %.2f)          %3d   %s\n", th.TauLow, th.TauHigh, band, pct(band, len(g)))
	fmt.Fprintf(w, " below tau_low %.2f (DISTINCT)              %3d   %s\n", th.TauLow, under, pct(under, len(g)))
	fmt.Fprintln(w)
	fmt.Fprintln(w, " Per domain, because one domain's sentences may be far more alike than another's:")
	fmt.Fprintln(w, " domain   |  n  | median | min    | max")
	fmt.Fprintln(w, " ---------+-----+--------+--------+--------")
	for _, name := range domainOrder {
		dg := genuine(inDomain(main, name))
		if len(dg) == 0 {
			fmt.Fprintf(w, " %-8s | %3d |    n/a |    n/a |    n/a\n", name, 0)
			continue
		}
		var ds []float64
		for _, t := range dg {
			ds = append(ds, t.Similarity)
		}
		s := sortedFloats(ds)
		fmt.Fprintf(w, " %-8s | %3d | %6.4f | %6.4f | %6.4f\n",
			name, len(s), quantile(s, 0.50), s[0], s[len(s)-1])
	}
	fmt.Fprintln(w)
}

func printGate(w io.Writer, main []trial) {
	u := usable(main)
	if len(u) == 0 {
		return
	}
	fmt.Fprintln(w, "WAS THE EMBEDDING MODEL ACTUALLY CONSULTED BY THE HYBRID COMPARATOR?")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " This section exists because of what happened on the synthetic corpus. There the")
	fmt.Fprintln(w, " hybrid comparator scored 1.000 and docs/results/semantic-experiment-hybrid-minilm.txt")
	fmt.Fprintln(w, " had to record that THE MODEL WAS NEVER CONSULTED: once the deciding fields were")
	fmt.Fprintln(w, " removed there was no free text left, so the figure measured a hand-written field")
	fmt.Fprintln(w, " extractor and not semantic similarity. The same question is asked again here.")
	fmt.Fprintln(w)
	gated, fellBack, modelUsed, noModel := 0, 0, 0, 0
	for _, t := range u {
		switch {
		case t.HybridFellBack:
			fellBack++
		case t.HybridGated:
			gated++
		case t.HybridModelUsed:
			modelUsed++
		default:
			noModel++
		}
	}
	fmt.Fprintf(w, " usable trials                                                     %4d\n", len(u))
	fmt.Fprintf(w, " gate closed on a deciding field (model not needed)                %4d   %s\n", gated, pct(gated, len(u)))
	fmt.Fprintf(w, " gate could not read the intent, fell back to pure semantic        %4d   %s\n", fellBack, pct(fellBack, len(u)))
	fmt.Fprintf(w, " gate passed, free text compared BY THE MODEL                      %4d   %s\n", modelUsed, pct(modelUsed, len(u)))
	fmt.Fprintf(w, " gate passed, answered WITHOUT the model (no free text left)       %4d   %s\n", noModel, pct(noModel, len(u)))
	fmt.Fprintln(w)
	if modelUsed == 0 {
		fmt.Fprintln(w, " THE MODEL WAS NEVER CONSULTED BY THE GATE. Every hybrid verdict that the gate")
		fmt.Fprintln(w, " actually decided was reached by field comparison or by the no-free-text shortcut.")
		fmt.Fprintln(w, " Where the gate decided, the hybrid figures measure the hand-written itinerary")
		fmt.Fprintln(w, " extractor and not semantic similarity, exactly as on the synthetic corpus; where")
		fmt.Fprintln(w, " it fell back, they are the pure-semantic figures under a different name. Neither")
		fmt.Fprintln(w, " reading supports quoting the hybrid column as a result about embeddings.")
	} else {
		fmt.Fprintf(w, " The model WAS consulted on %d of %d usable trials (%s). The hybrid figures are\n",
			modelUsed, len(u), strings.TrimSpace(pct(modelUsed, len(u))))
		fmt.Fprintln(w, " therefore part gate and part similarity; hybrid_model_used in the CSV says which")
		fmt.Fprintln(w, " for each row.")
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, " Per domain, which is where the gate's reach shows:")
	fmt.Fprintln(w, " domain   | usable| gate read | gate closed | fell back | model used")
	fmt.Fprintln(w, " ---------+-------+-----------+-------------+-----------+-----------")
	for _, name := range domainOrder {
		du := usable(inDomain(main, name))
		fmt.Fprintf(w, " %-8s | %5d | %9s | %11s | %9s | %10s\n", name, len(du),
			pct(countIf(du, func(t trial) bool { return !t.HybridFellBack }), len(du)),
			pct(countIf(du, func(t trial) bool { return t.HybridGated }), len(du)),
			pct(countIf(du, func(t trial) bool { return t.HybridFellBack }), len(du)),
			pct(countIf(du, func(t trial) bool { return t.HybridModelUsed }), len(du)))
	}
	fmt.Fprintln(w)

	byField := map[string]int{}
	for _, t := range u {
		if t.HybridGated {
			byField[t.HybridField]++
		}
	}
	if len(byField) > 0 {
		fmt.Fprintln(w, " What closed the gate:")
		for _, f := range sortedKeys(byField) {
			fmt.Fprintf(w, "   %-55s %4d\n", f, byField[f])
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, " A gate closing on a GENUINE rewording is a missed duplicate, i.e. a repeat that")
		fmt.Fprintln(w, " goes through; the next section counts the opposite error.")
		fmt.Fprintln(w)
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func printFalseSuppression(w io.Writer, main []trial) {
	d := drifted(main)
	if len(d) == 0 {
		return
	}
	fmt.Fprintln(w, "THE DANGEROUS ERROR: SUPPRESSING AN ACTION THAT WAS GENUINELY DIFFERENT")
	fmt.Fprintln(w)
	fmt.Fprintf(w, " On the %d drifted trials the second action is a DIFFERENT action, so the correct\n", len(d))
	fmt.Fprintln(w, " verdict is DISTINCT. A DUPLICATE here is the severe failure the project's own risk")
	fmt.Fprintln(w, " table names: a real action silently dropped, with no error anywhere to notice it by.")
	fmt.Fprintln(w)
	exactDup := countIf(d, func(t trial) bool { return t.ExactKeyEqual })
	semDup, semEsc, _ := tally(d, func(t trial) core.DedupVerdict { return t.SemVerdict })
	hybDup, hybEsc, _ := tally(d, func(t trial) core.DedupVerdict { return t.HybridVerdict })
	fmt.Fprintln(w, " comparator | wrongly DUPLICATE | ESCALATE | correctly DISTINCT")
	fmt.Fprintln(w, " -----------+-------------------+----------+-------------------")
	fmt.Fprintf(w, " %-10s | %17d | %8d | %d\n", "exact", exactDup, 0, len(d)-exactDup)
	fmt.Fprintf(w, " %-10s | %17d | %8d | %d\n", "semantic", semDup, semEsc, len(d)-semDup-semEsc)
	fmt.Fprintf(w, " %-10s | %17d | %8d | %d\n", "hybrid", hybDup, hybEsc, len(d)-hybDup-hybEsc)
	fmt.Fprintln(w)
	fmt.Fprintln(w, " The denominator is small and the drifted trials are whatever the model happened to")
	fmt.Fprintln(w, " get wrong, not a designed set of near-misses. This is an indication, not a")
	fmt.Fprintln(w, " false-positive rate.")
	fmt.Fprintln(w)
}

func printControls(w io.Writer, controls []trial) {
	fmt.Fprintln(w, "CONTROL ARM: WHAT A REAL MODEL DOES WHEN THE JOURNAL SURVIVED THE CRASH")
	fmt.Fprintln(w)
	if len(controls) == 0 {
		fmt.Fprintln(w, " Not run.")
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintln(w, " The main arm models a crash that lost its uncommitted events, so the recovered")
	fmt.Fprintln(w, " process re-plans the lost step from the same empty transcript. This arm changes one")
	fmt.Fprintln(w, " thing: the journal survives, so the recovered process replays the action and the")
	fmt.Fprintln(w, " model is asked for the NEXT step while looking at a transcript that already")
	fmt.Fprintln(w, " contains a CONFIRMED, irreversible result.")
	fmt.Fprintln(w)
	repeated := countOutcome(controls, outcomeUsable)
	silent := countIf(controls, func(t trial) bool {
		return t.Outcome == outcomeNoRepeat && t.AfterCallCount == 0
	})
	readOnly := countIf(controls, func(t trial) bool {
		return t.Outcome == outcomeNoRepeat && t.AfterCallCount > 0
	})
	other := len(controls) - repeated - silent - readOnly
	fmt.Fprintf(w, " control trials                                       %4d\n", len(controls))
	fmt.Fprintf(w, " recovered run repeated the action                    %4d   %s\n", repeated, pct(repeated, len(controls)))
	fmt.Fprintf(w, " recovered run issued no tool call at all             %4d   %s\n", silent, pct(silent, len(controls)))
	fmt.Fprintf(w, " recovered run called only the read tool              %4d   %s\n", readOnly, pct(readOnly, len(controls)))
	fmt.Fprintf(w, " never-acted or failed before that question arose     %4d   %s\n", other, pct(other, len(controls)))
	fmt.Fprintln(w)
	fmt.Fprintln(w, " This matters for how the fixture should be read. app/agent's scripted fixture")
	fmt.Fprintln(w, " re-issues the action in this configuration because its script says to. A real")
	fmt.Fprintln(w, " model, shown its own confirmed result, mostly does not — which means the rephrasing")
	fmt.Fprintln(w, " hazard does NOT arise from every crash. It arises from the crash that loses the")
	fmt.Fprintln(w, " planning step, which is the main arm, and that is a narrower claim than \"an agent")
	fmt.Fprintln(w, " that crashes re-issues its action in new words\".")
	fmt.Fprintln(w)
}

// printSweep is an ADDITIONAL table. The thresholds above are the shipped ones
// and were not chosen from this sweep; the sweep is here so a reader can see
// how sensitive the result is, not so a better pair can be picked from it.
func printSweep(w io.Writer, main []trial) {
	g := genuine(main)
	d := drifted(main)
	if len(g) == 0 {
		return
	}
	fmt.Fprintln(w, "ADDITIONAL, NOT THE RESULT: THRESHOLD SENSITIVITY OF THE PURE-SEMANTIC PATH")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " The figures above are at the shipped tau_low 0.85 / tau_high 0.90 and were not")
	fmt.Fprintln(w, " chosen from this table. The table shows what a different tau_high would have done")
	fmt.Fprintln(w, " to THIS set of pairs, which is a sensitivity check and not a tuning result —")
	fmt.Fprintln(w, " picking the best row would be selection on the only test set this run has.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, " tau_high | caught of %2d reworded | wrongly caught of %2d drifted\n", len(g), len(d))
	fmt.Fprintln(w, " ---------+-----------------------+-----------------------------")
	for tau := 0.70; tau < 1.0; tau += 0.05 {
		caught := countIf(g, func(t trial) bool { return t.Similarity >= tau })
		wrong := countIf(d, func(t trial) bool { return t.Similarity >= tau })
		star := " "
		if tau > 0.899 && tau < 0.901 {
			star = "*"
		}
		fmt.Fprintf(w, "%s   %.2f  | %21s | %s\n", star, tau,
			fmt.Sprintf("%d  (%s)", caught, strings.TrimSpace(pct(caught, len(g)))),
			fmt.Sprintf("%d  (%s)", wrong, strings.TrimSpace(pct(wrong, len(d)))))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, " * is the shipped tau_high.")
	fmt.Fprintln(w)
}

func printLimits(w io.Writer, p provenance, main, controls []trial) {
	fmt.Fprintln(w, "WHAT IS MEASURED, WHAT IS ASSUMED, WHAT IS NOT MEASURED")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " MEASURED")
	model := p.Model
	if model == "" {
		model = "the model named in the original run's header"
	}
	fmt.Fprintf(w, "  - Every sentence compared here was produced by %s over the network,\n", model)
	fmt.Fprintln(w, "    at temperature 0.8, from a goal that suggested no phrasing. Both sides of the")
	fmt.Fprintln(w, "    crash, in all five domains.")
	fmt.Fprintln(w, "  - Every similarity score is a cosine between two 384-dimensional vectors from a")
	fmt.Fprintln(w, "    running all-MiniLM-L6-v2 service. No cached vectors, no hash embedder.")
	fmt.Fprintln(w, "  - Every verdict is produced by the engine's own comparator code at the shipped")
	fmt.Fprintln(w, "    thresholds: exact.Key, semantic.Checker's pure path, and the same")
	fmt.Fprintln(w, "    Checker.Compare call the hybrid path makes per candidate.")
	fmt.Fprintln(w, "  - The side-effect counts are real invocations of each domain's commit tool,")
	fmt.Fprintln(w, "    counted by the tool, not inferred from the verdicts.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " ASSUMED")
	fmt.Fprintln(w, "  - That the fields each goal declares are the identity of the action. Everything")
	fmt.Fprintln(w, "    the drift classification does follows from that, and it is a modelling choice")
	fmt.Fprintln(w, "    rather than a measurement.")
	fmt.Fprintln(w, "  - That the drift reader in cmd/rephrase/truth.go reads the model's sentences")
	fmt.Fprintln(w, "    correctly. It is hand-written and it is deliberately NOT the engine's extractor,")
	fmt.Fprintln(w, "    so the two can disagree; the raw sentences and the per-field readings are in the")
	fmt.Fprintln(w, "    CSV so a reader can check every classification by eye. No person has done so")
	fmt.Fprintln(w, "    exhaustively. In the message domain the subject is read with accept-only rules,")
	fmt.Fprintln(w, "    so that domain cannot report drift on anything but the recipient.")
	fmt.Fprintln(w, "  - That the simulator's crash is a faithful stand-in for a real one. It reproduces")
	fmt.Fprintln(w, "    the one behaviour that matters — journaled work replays, lost work is redone —")
	fmt.Fprintln(w, "    and it does so without a database or a network.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, " NOT MEASURED, AND NOT TO BE INFERRED FROM THIS FILE")
	fmt.Fprintln(w, "  - Any other model. One model, one temperature, one prompt shape. A rephrase rate")
	fmt.Fprintln(w, "    is a property of the model and the prompt together, and this run varies neither.")
	fmt.Fprintln(w, "  - THE PROMPT ASKS FOR VARIATION. The planner's system prompt contains the line")
	fmt.Fprintln(w, "    \"Vary your phrasing naturally between calls; do not repeat an earlier sentence")
	fmt.Fprintln(w, "    word for word.\" It is the prompt the engine ships and runs, so measuring it is")
	fmt.Fprintln(w, "    correct, but the rephrase rate above is the rate FOR THAT PROMPT. On the main")
	fmt.Fprintln(w, "    arm the two planning calls each see an empty transcript, so there is no earlier")
	fmt.Fprintln(w, "    sentence for the instruction to point at; it still cannot be assumed inert.")
	fmt.Fprintln(w, "  - Precision and recall over a balanced corpus. This run has no negative class by")
	fmt.Fprintln(w, "    construction: the drifted trials are whatever the model happened to get wrong,")
	fmt.Fprintln(w, "    not a designed set of near-misses. docs/results/semantic-experiment-minilm.txt")
	fmt.Fprintln(w, "    is where the precision/recall curve lives, on a corpus built for it.")
	fmt.Fprintln(w, "  - The hybrid comparator on four of the five domains. The engine has one")
	fmt.Fprintln(w, "    deciding-field extractor and it is flight-shaped. Where it cannot read an intent")
	fmt.Fprintln(w, "    the hybrid column is the pure-semantic column. No extractors were written for")
	fmt.Fprintln(w, "    the other domains, deliberately; see BY DOMAIN above.")
	fmt.Fprintln(w, "  - End-to-end engine behaviour. The comparators are the engine's, but the workflow")
	fmt.Fprintln(w, "    ran in app/agent's in-memory simulator, not against PostgreSQL on the cluster.")
	fmt.Fprintln(w, "  - Whether a human reviewer would agree with any ESCALATE verdict. Nothing here")
	fmt.Fprintln(w, "    asked a human anything.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, rule)
	fmt.Fprintf(w, " %d main trials, %d control trials, %d goals in %d domains. All figures from this\n",
		len(main), len(controls), len(goals), len(domainOrder))
	fmt.Fprintln(w, " run alone, recomputed from the CSV.")
	fmt.Fprintln(w, rule)
}
