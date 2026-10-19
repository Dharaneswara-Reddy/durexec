package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// SCORING: EVERYTHING DERIVED, NOTHING REMEASURED
// ---------------------------------------------------------------------------
//
// A verdict is a threshold applied to a similarity, plus the gate's veto. Both
// are stored per row, so one pass over the corpus scores every threshold in
// the sweep, both escalation policies and every ablation. The alternative —
// re-running the comparator once per threshold — would multiply the model
// calls by thirty and measure nothing new.
//
// That shortcut is only valid while it agrees with the engine EXACTLY, so
// verifyAgainstChecker re-derives the shipped-threshold verdict for every row
// and compares it with the verdict semantic.Checker actually returned. A
// mismatch means the harness and the engine have drifted apart and every
// number in the sweep would be fiction, so it is reported as a failure of the
// run rather than a warning.

// verdict is the three-valued answer, kept as the core type so that the
// classification is the engine's own function and not a copy of it.
type verdictKind = core.DedupVerdict

// deriveVerdict is the one place a row becomes a verdict.
func deriveVerdict(r row, th core.Thresholds, ab string) verdictKind {
	switch {
	case r.Comparator == "exact":
		if r.ExactHit {
			return core.VerdictDuplicate
		}
		return core.VerdictDistinct

	case r.Extractor == "":
		// The pure-semantic path: no gate, so the threshold decides alone.
		return th.Classify(r.Similarity)

	case ab == ablationGate:
		// The gate alone, with similarity removed. An extractor that could
		// not read a side has no opinion, and no opinion must not become a
		// duplicate: that is the direction that suppresses a real action.
		if r.Gated || !r.RecognisedA || !r.RecognisedB {
			return core.VerdictDistinct
		}
		return core.VerdictDuplicate

	default:
		// The comparator as built. A pair the gate rejected is DISTINCT at
		// every threshold, because nothing was measured for it.
		if r.Gated {
			return core.VerdictDistinct
		}
		v := th.Classify(r.Similarity)
		if r.GateDecides && v == core.VerdictDistinct {
			// The gate passed on real field evidence and only the leftover
			// prose disagreed. That is doubt, not a different action.
			v = core.VerdictEscalate
		}
		return v
	}
}

// prevented reports whether this verdict stops the repeat under one policy.
//
// This is the whole escalation-policy question in one function. Under the
// engine's default an ESCALATE verdict runs the activity, so it prevents
// nothing; under ESCALATION_BLOCKS it holds the activity for a person, so it
// does — at the cost of asking someone.
func prevented(v verdictKind, p policy) bool {
	if v == core.VerdictDuplicate {
		return true
	}
	return p.EscalateStops && v == core.VerdictEscalate
}

// ---------------------------------------------------------------------------
// Confusion matrices.
// ---------------------------------------------------------------------------

// confusion is one comparator's answer over a set of pairs, under one policy.
//
// "Positive" means "the comparator prevented the action". That is the
// definition that matters: a verdict that does not prevent the repeat has not
// caught it, however confident it sounded.
type confusion struct {
	TP, FP, TN, FN int

	// Escalated is how many pairs landed in the uncertain band, counted
	// separately because under the blocking policy every one of them is a
	// person being asked a question. A policy that catches more by escalating
	// everything has not improved the comparator, and the only way to see
	// that is to count the escalations next to the catches.
	Escalated int

	// HardFP and Hard are the false positives over the lexically close
	// negatives, and how many of those there were. Precision over easy
	// negatives is close to free, so this is the number worth reading.
	HardFP, Hard int

	// Recognised is how many pairs the extractor could read on BOTH sides,
	// and ModelUsed how many needed an embedding. Both are counted here
	// rather than reported elsewhere because they are what tells a reader
	// whether a figure describes the gate or the model.
	Recognised, ModelUsed, Fallback, Total int

	Latencies []float64
}

func (c *confusion) add(r row, v verdictKind, p policy) {
	c.Total++
	if r.RecognisedA && r.RecognisedB {
		c.Recognised++
	}
	if r.ModelUsed {
		c.ModelUsed++
	}
	if r.Fallback {
		c.Fallback++
	}
	if r.LatencyUS > 0 {
		c.Latencies = append(c.Latencies, r.LatencyUS)
	}
	if v == core.VerdictEscalate {
		c.Escalated++
	}
	stop := prevented(v, p)
	switch {
	case r.Truth && stop:
		c.TP++
	case r.Truth && !stop:
		c.FN++
	case !r.Truth && stop:
		c.FP++
		if r.Hard {
			c.HardFP++
		}
	default:
		c.TN++
	}
	if !r.Truth && r.Hard {
		c.Hard++
	}
}

func (c confusion) Duplicates() int { return c.TP + c.FN }
func (c confusion) Distincts() int  { return c.FP + c.TN }

// Detect is the duplicate-detection rate, which is recall.
func (c confusion) Detect() (k, n int) { return c.TP, c.TP + c.FN }

// FPR is the share of genuinely distinct pairs the comparator suppressed.
// Every one of them is a real action that would not have happened.
func (c confusion) FPR() (k, n int) { return c.FP, c.FP + c.TN }

func (c confusion) HardFPR() (k, n int) { return c.HardFP, c.Hard }

func (c confusion) Precision() float64 {
	if c.TP+c.FP == 0 {
		return math.NaN()
	}
	return float64(c.TP) / float64(c.TP+c.FP)
}

func (c confusion) Recall() float64 {
	if c.TP+c.FN == 0 {
		return math.NaN()
	}
	return float64(c.TP) / float64(c.TP+c.FN)
}

func (c confusion) F1() float64 {
	p, r := c.Precision(), c.Recall()
	if math.IsNaN(p) || math.IsNaN(r) || p+r == 0 {
		return math.NaN()
	}
	return 2 * p * r / (p + r)
}

func (c confusion) Accuracy() float64 {
	n := c.TP + c.FP + c.TN + c.FN
	if n == 0 {
		return math.NaN()
	}
	return float64(c.TP+c.TN) / float64(n)
}

func (c confusion) Percentile(q float64) float64 {
	if len(c.Latencies) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), c.Latencies...)
	sort.Float64s(s)
	i := int(q * float64(len(s)-1))
	return s[i]
}

// ---------------------------------------------------------------------------
// Grouping.
// ---------------------------------------------------------------------------

// filter selects rows. A nil field means "any".
type filter struct {
	Corpus     string
	Comparator string
	Domain     string
}

func (f filter) match(r row) bool {
	if strings.HasPrefix(r.Note, "ERROR") {
		// A row the harness could not measure is not a measurement. It is
		// written so the gap is visible in the data, counted in the
		// completeness table, retried by a resume — and excluded from every
		// figure, because counting it would turn a provider failure into a
		// property of the comparator.
		return false
	}
	if f.Corpus != "" && r.Corpus != f.Corpus {
		return false
	}
	if f.Comparator != "" && r.Comparator != f.Comparator {
		return false
	}
	if f.Domain != "" && r.Domain != f.Domain {
		return false
	}
	return true
}

// score builds one confusion matrix.
func score(rows []row, f filter, th core.Thresholds, p policy, ab string) confusion {
	var c confusion
	for _, r := range rows {
		if !f.match(r) {
			continue
		}
		c.add(r, deriveVerdict(r, th, ab), p)
	}
	return c
}

// comparatorsIn lists the comparators present in the rows, in the order the
// arms were defined where possible so two reports read the same way.
func comparatorsIn(rows []row, order []string) []string {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Comparator] = true
	}
	var out []string
	for _, name := range order {
		if seen[name] {
			out = append(out, name)
			delete(seen, name)
		}
	}
	rest := make([]string, 0, len(seen))
	for name := range seen {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func corporaIn(rows []row) []string {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Corpus] = true
	}
	// A fixed order, most informative first: the balanced multi-domain
	// corpus, then the published flight one, then the live wordings.
	var out []string
	for _, c := range []string{corpusMultiDomain, corpusFlight, corpusRephrase} {
		if seen[c] {
			out = append(out, c)
			delete(seen, c)
		}
	}
	rest := make([]string, 0, len(seen))
	for c := range seen {
		rest = append(rest, c)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func domainsIn(rows []row, corpus string) []string {
	seen := map[string]bool{}
	for _, r := range rows {
		if corpus == "" || r.Corpus == corpus {
			seen[r.Domain] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Confidence intervals.
// ---------------------------------------------------------------------------

// wilson is a 95% confidence interval for a proportion, by Wilson's score
// method.
//
// It is here because the per-domain denominators are small — fifteen pairs,
// or five live rewordings — and a bare percentage over five trials is not a
// result. Wilson rather than the normal approximation because the normal
// interval is wrong exactly where this data lives: near 0 and near 1, and at
// small n, where it produces bounds below zero and above one.
//
// It returns NaNs for n = 0 rather than zeros, so an empty cell cannot be
// read as a measured zero.
func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return math.NaN(), math.NaN()
	}
	const z = 1.959963984540054 // two-sided 95%
	p := float64(k) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / den
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	lo, hi = centre-half, centre+half
	return math.Max(0, lo), math.Min(1, hi)
}

// rateCI formats a rate with its interval, or a clear absence.
func rateCI(k, n int) string {
	if n == 0 {
		return "   n/a      "
	}
	lo, hi := wilson(k, n)
	return fmt.Sprintf("%.3f [%.2f-%.2f]", float64(k)/float64(n), lo, hi)
}

// ---------------------------------------------------------------------------
// McNemar's test.
// ---------------------------------------------------------------------------

// mcnemar compares two comparators on the SAME pairs.
//
// It is the correct test for paired binary outcomes and the one a reviewer
// asks for: two comparators scored on one corpus are not two independent
// samples, so comparing their accuracies with anything that assumes
// independence overstates the evidence. What matters is only the DISAGREEMENTS
// — the pairs one got right and the other got wrong — and whether they fall
// more on one side than chance would explain.
//
// The p-value is the EXACT two-sided binomial test, not the chi-squared
// approximation, because the discordant counts here are often small and the
// approximation is unreliable below about twenty-five.
type mcnemarResult struct {
	A, B string

	// OnlyA is the number of pairs A got right and B got wrong; OnlyB the
	// reverse. Both wrong and both right are discarded by the test, which is
	// the point of it.
	OnlyA, OnlyB  int
	Both, Neither int

	P float64
}

// Discordant is the sample the test actually runs on.
func (m mcnemarResult) Discordant() int { return m.OnlyA + m.OnlyB }

func mcnemar(rows []row, corpus, a, b string, th core.Thresholds, p policy) mcnemarResult {
	type outcome struct{ a, b int } // -1 unknown, 0 wrong, 1 right
	byPair := map[string]*outcome{}
	for _, r := range rows {
		if strings.HasPrefix(r.Note, "ERROR") {
			continue
		}
		if corpus != "" && r.Corpus != corpus {
			continue
		}
		if r.Comparator != a && r.Comparator != b {
			continue
		}
		o := byPair[r.PairID]
		if o == nil {
			o = &outcome{a: -1, b: -1}
			byPair[r.PairID] = o
		}
		right := 0
		if prevented(deriveVerdict(r, th, ablationFull), p) == r.Truth {
			right = 1
		}
		if r.Comparator == a {
			o.a = right
		} else {
			o.b = right
		}
	}

	res := mcnemarResult{A: a, B: b}
	for _, o := range byPair {
		if o.a < 0 || o.b < 0 {
			// A pair only one comparator measured is not a paired
			// observation and must not be counted as one.
			continue
		}
		switch {
		case o.a == 1 && o.b == 1:
			res.Both++
		case o.a == 1 && o.b == 0:
			res.OnlyA++
		case o.a == 0 && o.b == 1:
			res.OnlyB++
		default:
			res.Neither++
		}
	}
	res.P = exactBinomialTwoSided(res.OnlyA, res.OnlyA+res.OnlyB)
	return res
}

// exactBinomialTwoSided is P(|X - n/2| >= |k - n/2|) for X ~ Binomial(n, 1/2).
//
// Computed by summing the tail exactly in log space, so a corpus of several
// hundred pairs cannot overflow a binomial coefficient and quietly return a
// wrong p-value.
func exactBinomialTwoSided(k, n int) float64 {
	if n == 0 {
		// No disagreement at all: there is nothing to test, and reporting a
		// p-value of 1 would imply a test was run.
		return math.NaN()
	}
	lower := k
	if n-k < lower {
		lower = n - k
	}
	sum := 0.0
	for i := 0; i <= lower; i++ {
		sum += math.Exp(logChoose(n, i) - float64(n)*math.Ln2)
	}
	p := 2 * sum
	if p > 1 {
		p = 1
	}
	return p
}

func logChoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n) + 1)
	b, _ := math.Lgamma(float64(k) + 1)
	c, _ := math.Lgamma(float64(n-k) + 1)
	return a - b - c
}

// ---------------------------------------------------------------------------
// The guard that licenses all of the above.
// ---------------------------------------------------------------------------

// verifyAgainstChecker re-derives every row's verdict at the shipped
// thresholds and compares it with what semantic.Checker returned.
//
// It returns the mismatches rather than failing, so the report can print them
// and still be written: a run whose shortcut has drifted from the engine is a
// run with a serious problem, and hiding the rest of the output would make
// that problem harder to diagnose, not easier.
func verifyAgainstChecker(rows []row, th core.Thresholds) []string {
	var bad []string
	for _, r := range rows {
		if r.CheckerVerdict == "" || r.Note != "" {
			continue
		}
		got := deriveVerdict(r, th, ablationFull)
		if string(got) != r.CheckerVerdict {
			bad = append(bad, fmt.Sprintf(
				"%s / %s / %s: derived %s but semantic.Checker returned %s "+
					"(similarity %.6f, gated=%t)",
				r.Corpus, r.Comparator, r.PairID, got, r.CheckerVerdict, r.Similarity, r.Gated))
		}
		if len(bad) >= 20 {
			break
		}
	}
	return bad
}

// ---------------------------------------------------------------------------
// Threshold sweeps.
// ---------------------------------------------------------------------------

// sweepPoint is one threshold's precision and recall for one comparator.
type sweepPoint struct {
	Tau       float64
	Conf      confusion
	IsShipped bool
}

// sweep traces a precision/recall curve by moving the threshold, with the
// band collapsed to a single tau so the curve is a curve and not a family.
//
// The shipped operating point is marked rather than chosen. Reading the best
// row off this table and quoting it is selection on the test set, and the
// report says so next to the table.
func sweep(rows []row, f filter, p policy, ab string, min, max, step, shippedLow, shippedHigh float64) []sweepPoint {
	var out []sweepPoint
	n := int(math.Round((max - min) / step))
	for i := 0; i <= n; i++ {
		tau := math.Round((min+float64(i)*step)*10000) / 10000
		if tau > 1 {
			break
		}
		th := core.Thresholds{TauLow: tau, TauHigh: tau}
		out = append(out, sweepPoint{Tau: tau, Conf: score(rows, f, th, p, ab)})
	}
	// Mark the shipped tau_high, which is the threshold above which the
	// engine calls a pair a duplicate.
	for i := range out {
		if math.Abs(out[i].Tau-shippedHigh) < 1e-9 {
			out[i].IsShipped = true
		}
	}
	_ = shippedLow
	return out
}
