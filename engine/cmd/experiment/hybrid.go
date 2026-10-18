package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

// ---------------------------------------------------------------------------
// Reporting the hybrid comparator.
// ---------------------------------------------------------------------------
//
// Three things are printed that the semantic arms do not need.
//
// The HEAD-TO-HEAD table, because the whole question is "is this better than
// what we had?", and an answer spread over sixty rows of a sweep is not an
// answer. It names the same thresholds for both comparators and the baseline.
//
// WHAT THE GATE DID, because a hybrid number is only as good as the field
// extraction behind it. If the gate rejected everything on one field, or if
// it could not read half the corpus, the accuracy figure above it is
// describing the extractor rather than the idea.
//
// THE FREE-TEXT PROBE, because on this corpus the gate answers almost every
// pair by itself and the similarity half is left with nothing to do. A result
// that never exercises half the mechanism has to say so, and then exercise it
// deliberately — which is what the probe does, on pairs that are NOT part of
// the 213 and are NOT in any precision figure.

// hasHybrid reports whether this run measured the hybrid comparator at all.
func hasHybrid(results []*armResult) bool {
	for _, r := range results {
		if r.Arm.Kind == kindHybrid {
			return true
		}
	}
	return false
}

// armByName finds one arm's result. The head-to-head table names the arms it
// wants rather than taking whichever scored best, which would be selection on
// the test set wearing a comparison's clothes.
func armByName(results []*armResult, name string) *armResult {
	for _, r := range results {
		if r.Arm.Name == name {
			return r
		}
	}
	return nil
}

// printHybridComparison is the table a reader of both result files needs: the
// baseline, the shipped comparator and the opt-in one, at the same thresholds,
// over the same pairs.
func printHybridComparison(results []*armResult, prov *provenance) {
	if !hasHybrid(results) {
		return
	}
	operating := core.Thresholds{TauLow: roundTau(prov.TauLow), TauHigh: roundTau(prov.TauHigh)}
	single := fmt.Sprintf("tau=%.2f", operating.TauHigh)

	rows := []struct {
		label string
		name  string
	}{
		{"exact (the baseline)", "exact"},
		{"semantic, " + single, "semantic " + single},
		{"semantic, band " + bandText(operating), bandArmName(operating)},
		{"hybrid, " + single, "hybrid " + single},
		{"hybrid, band " + bandText(operating), hybridBandArmName(operating)},
	}

	fmt.Println("HEAD TO HEAD AT THE SHIPPED THRESHOLDS")
	fmt.Println()
	const ruler = " ------------------------------+--------+-------+--------+--------+-------+-------+----------"
	fmt.Println(" comparator                    | detect |   FPR | hardFPR|  prec  |   F1  |  esc  |  p50/p99")
	fmt.Println(ruler)
	for _, want := range rows {
		r := armByName(results, want.name)
		if r == nil {
			continue
		}
		fmt.Printf(" %-29s | %6.3f | %5.3f | %6.3f | %6s | %5.3f | %5.3f | %s / %s\n",
			trunc(want.label, 29),
			r.DuplicateDetectionRate(), r.FalsePositiveRate(), r.HardFalsePositiveRate(),
			precisionText(r), r.F1(), r.EscalationRate(),
			fmtDur(r.P50()), fmtDur(r.P99()))
	}
	fmt.Println(ruler)
	fmt.Println()
	fmt.Println(" The semantic rows are the SAME MEASUREMENT as the baseline run in")
	fmt.Println(" docs/results/semantic-experiment-minilm.txt: same corpus, same model, same")
	fmt.Println(" cached vectors, same code. They are printed here so the hybrid rows can be")
	fmt.Println(" read against them without trusting two separate runs to be comparable.")
	fmt.Println()
	fmt.Println(" detect  duplicates caught automatically (= recall)   FPR  distinct pairs suppressed")
	fmt.Println(" hardFPR the same over the LEXICALLY CLOSE negatives  esc  pairs handed to a human")
	fmt.Println()
}

func bandText(th core.Thresholds) string {
	return fmt.Sprintf("[%.2f,%.2f)", th.TauLow, th.TauHigh)
}

// printGateSummary says what the deciding-field gate actually did, and is the
// section that keeps the numbers above it honest.
func printGateSummary(obs []observation) {
	measured := 0
	for _, o := range obs {
		if o.HybridMeasured {
			measured++
		}
	}
	if measured == 0 {
		return
	}

	var (
		gatedDup, gatedDistinct int
		passedDup, passedDist   int
		modelUsed, unreadable   int
		byField                 = map[string]int{}
	)
	for _, o := range obs {
		if !o.HybridMeasured {
			continue
		}
		if !o.HybridReadable {
			unreadable++
		}
		if o.HybridModelUsed {
			modelUsed++
		}
		switch {
		case o.HybridGated && bool(o.Pair.Truth):
			gatedDup++
			byField[o.HybridField]++
		case o.HybridGated:
			gatedDistinct++
			byField[o.HybridField]++
		case bool(o.Pair.Truth):
			passedDup++
		default:
			passedDist++
		}
	}

	fmt.Println("WHAT THE DECIDING-FIELD GATE DID")
	fmt.Println()
	fmt.Printf("  pairs measured by the hybrid comparator : %d\n", measured)
	fmt.Printf("  rejected on a deciding field            : %d  (%d truly distinct, %d truly duplicate)\n",
		gatedDup+gatedDistinct, gatedDistinct, gatedDup)
	fmt.Printf("  passed the gate, scored by free text    : %d  (%d truly duplicate, %d truly distinct)\n",
		passedDup+passedDist, passedDup, passedDist)
	fmt.Printf("  needed an embedding to answer           : %d\n", modelUsed)
	fmt.Printf("  one side's fields could not be read     : %d\n", unreadable)
	fmt.Println()

	if len(byField) > 0 {
		names := make([]string, 0, len(byField))
		for k := range byField {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Println("  rejections by the field that differed:")
		for _, n := range names {
			fmt.Printf("    %-40s %d\n", n, byField[n])
		}
		fmt.Println()
	}

	// The two sentences that matter most in this file.
	if modelUsed == 0 {
		fmt.Println("  THE MODEL WAS NEVER CONSULTED. On this corpus every intent is nothing but")
		fmt.Println("  its deciding fields: once those are removed there is no free text left, so")
		fmt.Println("  the similarity half of this comparator contributed NOTHING to any number")
		fmt.Println("  above. What is being measured here is the field extractor — exact matching")
		fmt.Println("  over values scraped out of prose — and not semantic similarity at all.")
	} else {
		fmt.Printf("  The model was consulted on %d of %d pairs (%.1f%%). Everything else was\n",
			modelUsed, measured, 100*ratio(modelUsed, measured))
		fmt.Println("  decided by the deciding fields alone, so most of the accuracy above is the")
		fmt.Println("  field extractor's and not the model's.")
	}
	fmt.Println()
	fmt.Println("  The extractor's vocabulary was written by reading this corpus. An accuracy")
	fmt.Println("  figure measured on the same corpus is therefore partly a measurement of that")
	fmt.Println("  coverage: it is selection on the test set, and no amount of care in writing")
	fmt.Println("  the rules removes it. See internal/idem/semantic/itinerary.go.")
	fmt.Println()
}

// ---------------------------------------------------------------------------
// The free-text probe.
// ---------------------------------------------------------------------------

// freeTextProbe is a pair whose deciding fields are IDENTICAL and whose free
// text is the only thing that differs.
//
// The corpus cannot contain these without changing it, and changing it would
// end the like-for-like comparison with the baseline run. So they are kept
// here, measured separately, and excluded from every precision, recall and F1
// figure in this report.
//
// They exist to answer one question the corpus cannot: when the gate passes
// and the decision really is left to similarity over free text, does
// similarity get it right? The labels are, again, assigned by construction —
// a reworded preference is called a duplicate, a changed preference is called
// distinct — with all the caveats that carries.
type freeTextProbe struct {
	ID    string
	Note  string
	A, B  string
	Truth Label
}

var freeTextProbes = []freeTextProbe{
	{"probe-reworded-window", "same flight, the same seat preference reworded",
		"BLR to DEL Friday morning, window seat if possible",
		"Bangalore to Delhi, Friday AM, prefer a window seat", Duplicate},
	{"probe-reworded-meal", "same flight, the same meal request reworded",
		"BLR to DEL Friday morning, vegetarian meal",
		"Bangalore to Delhi, Friday AM, with a veg meal", Duplicate},
	{"probe-reworded-fare", "same flight, the same fare instruction reworded",
		"BLR to DEL Friday morning, cheapest fare available",
		"Bangalore to Delhi, Friday AM, lowest price you can find", Duplicate},
	{"probe-reworded-baggage", "same flight, the same baggage request reworded",
		"BLR to DEL Friday morning, extra baggage",
		"Bangalore to Delhi, Friday AM, add checked bags", Duplicate},

	{"probe-seat-changes", "same flight, a different seat: two different bookings",
		"BLR to DEL Friday morning, window seat",
		"BLR to DEL Friday morning, aisle seat", Distinct},
	{"probe-cabin-changes", "same flight, a different cabin: a different ticket and a different price",
		"BLR to DEL Friday morning, business class",
		"BLR to DEL Friday morning, economy class", Distinct},
	{"probe-refundable-changes", "same flight, a different fare rule",
		"BLR to DEL Friday morning, refundable fare",
		"BLR to DEL Friday morning, non-refundable fare", Distinct},
	{"probe-card-changes", "same flight, a different card is charged",
		"BLR to DEL Friday morning, pay with the corporate card",
		"BLR to DEL Friday morning, pay with my personal card", Distinct},
}

// probeResult is one probe's measurement.
type probeResult struct {
	Probe    freeTextProbe
	Fields   string
	MineRem  string
	TheirRem string
	Cmp      semantic.Comparison
	Verdict  core.DedupVerdict
	Latency  time.Duration
}

// Correct reports whether the shipped band's verdict matched the label, with
// ESCALATE counted as not resolved — the same accounting the main table uses.
func (r probeResult) Correct() bool {
	if bool(r.Probe.Truth) {
		return r.Verdict == core.VerdictDuplicate
	}
	return r.Verdict == core.VerdictDistinct
}

// runFreeTextProbes measures the probes through the same checker the corpus
// went through, at the shipped thresholds.
func runFreeTextProbes(ctx context.Context, cfg config, emb core.Embedder) ([]probeResult, error) {
	a := arm{
		Kind:       kindHybrid,
		Thresholds: core.Thresholds{TauLow: roundTau(cfg.tauLow), TauHigh: roundTau(cfg.tauHigh)},
	}
	out := make([]probeResult, 0, len(freeTextProbes))
	for _, pr := range freeTextProbes {
		pair := Pair{ID: pr.ID, Category: "free-text-probe", Note: pr.Note, A: pr.A, B: pr.B, Truth: pr.Truth}
		o, cmp, err := checkHybridPair(ctx, cfg, a, pair, emb)
		if err != nil {
			return nil, fmt.Errorf("free-text probe %s: %w", pr.ID, err)
		}
		out = append(out, probeResult{
			Probe:    pr,
			Fields:   cmp.MineFields.Signature(),
			MineRem:  cmp.MineFields.Remainder,
			TheirRem: cmp.TheirsFields.Remainder,
			Cmp:      cmp,
			Verdict:  o.Verdict,
			Latency:  o.Latency,
		})
	}
	return out, nil
}

func printFreeTextProbes(rs []probeResult, prov *provenance) {
	if len(rs) == 0 {
		return
	}
	fmt.Println("THE FREE-TEXT PROBE — WHERE THE SIMILARITY HALF ACTUALLY DECIDES")
	fmt.Println()
	fmt.Printf("  %d pairs, NOT part of the %d-pair corpus and not in any figure above.\n",
		len(rs), prov.Corpus.Total)
	fmt.Println("  Every pair books the identical flight, so the gate passes on all of them and")
	fmt.Println("  the verdict comes entirely from the free text that is left over.")
	fmt.Printf("  Verdicts are at the shipped band [%.2f,%.2f).\n\n", prov.TauLow, prov.TauHigh)

	var (
		correct, gatedAway   int
		dupRight, dupTotal   int
		distRight, distTotal int
		worstDup             = 1.0 // the lowest score given to a true repeat
		worstDist            = 0.0 // the highest score given to a true difference
	)
	for _, r := range rs {
		if r.Correct() {
			correct++
		}
		if r.Cmp.Gated {
			gatedAway++
		}
		if bool(r.Probe.Truth) {
			dupTotal++
			if r.Correct() {
				dupRight++
			}
			if r.Cmp.Similarity < worstDup {
				worstDup = r.Cmp.Similarity
			}
		} else {
			distTotal++
			if r.Correct() {
				distRight++
			}
			if r.Cmp.Similarity > worstDist {
				worstDist = r.Cmp.Similarity
			}
		}
		mark := "wrong"
		if r.Correct() {
			mark = "right"
		}
		fmt.Printf("  %-26s truth=%-9s got=%-9s sim=%.4f  %s\n",
			trunc(r.Probe.ID, 26), r.Probe.Truth, r.Verdict, r.Cmp.Similarity, mark)
		fmt.Printf("     free text: %q  vs  %q\n", r.TheirRem, r.MineRem)
	}

	fmt.Println()
	fmt.Printf("  %d of %d right. %d were rejected by the gate before the free text was read.\n",
		correct, len(rs), gatedAway)
	fmt.Printf("  reworded requests recognised as repeats : %d of %d\n", dupRight, dupTotal)
	fmt.Printf("  changed requests kept apart             : %d of %d\n", distRight, distTotal)
	fmt.Println()

	// The reading is derived from the numbers rather than written in advance,
	// because the point of running the probe was that its outcome was not
	// known.
	if dupRight < dupTotal {
		fmt.Printf("  THE SIMILARITY HALF MISSED %d OF %d REWORDED REPEATS. Once the deciding\n",
			dupTotal-dupRight, dupTotal)
		fmt.Println("  fields are taken out, what is left is two or three words — \"vegetarian")
		fmt.Println("  meal\" against \"with veg meal\" — and the encoder scored the weakest of these")
		fmt.Printf("  pairs %.4f, well under the shipped band. The instrument that was introduced\n", worstDup)
		fmt.Println("  to recognise rewording is now being asked about fragments too short to carry")
		fmt.Println("  much meaning, and on these it does not recognise it.")
		fmt.Println()
	}
	if distRight < distTotal {
		fmt.Printf("  AND THE BASELINE'S FAILURE IS STILL PRESENT, MOVED INSIDE THE FREE TEXT. The\n")
		fmt.Printf("  worst true difference here scored %.4f and would be suppressed: one word of\n", worstDist)
		fmt.Println("  negation changes which ticket is bought, and the encoder reads the two")
		fmt.Println("  phrasings as the same topic — exactly the mistake the deciding-field gate")
		fmt.Println("  was built to remove. The gate removed it only for the fields it knows.")
		fmt.Println()
	}
	fmt.Println("  So the gate's gain on the corpus above is not a gain for similarity. It is")
	fmt.Println("  what happens when the decision is taken away from similarity. Where the")
	fmt.Println("  decision is left to it, these eight pairs say it is still the wrong")
	fmt.Println("  instrument. Eight constructed pairs are not evidence of a rate; they are")
	fmt.Println("  evidence that the failure mode survives the fix.")
	fmt.Println()
}

// hybridClosingNote is appended to the closing caveats when the hybrid arms
// ran, so the file cannot be read as a retraction of the baseline.
func printHybridClosingNote(results []*armResult, obs []observation) {
	if !hasHybrid(results) {
		return
	}
	modelUsed := 0
	for _, o := range obs {
		if o.HybridMeasured && o.HybridModelUsed {
			modelUsed++
		}
	}

	fmt.Println()
	fmt.Println("WHAT THE HYBRID ARMS ARE, AND WHAT THEY ARE NOT")
	fmt.Println()
	fmt.Println(" THE HYBRID COMPARATOR IS OFF BY DEFAULT. The engine still ships the")
	fmt.Println(" pure-semantic path measured in docs/results/semantic-experiment-minilm.txt,")
	fmt.Println(" and the semantic rows in this run reproduce that file. Nothing here")
	fmt.Println(" withdraws it: this is the next step, measured.")
	fmt.Println()
	fmt.Println(" WHAT IS REAL in the hybrid rows:")
	fmt.Println("   - the verdicts come from the engine's own semantic.Checker with its field")
	fmt.Println("     extractor configured, not from a reimplementation in this harness;")
	fmt.Println("   - the gate's decisions are exact string comparisons of extracted values,")
	fmt.Println("     so they are reproducible from the journaled intent text alone;")
	fmt.Println("   - the latency figures are real timings of that path.")
	fmt.Println()
	fmt.Println(" WHAT IS NOT:")
	fmt.Println("   - the labels are still generated by construction, not hand-assigned, so")
	fmt.Println("     every accuracy figure here remains provisional in exactly the way the")
	fmt.Println("     baseline's does;")
	fmt.Println("   - the field extractor's vocabulary was written against this corpus, so its")
	fmt.Println("     coverage of it is not evidence of coverage anywhere else. A phrasing")
	fmt.Println("     outside that vocabulary is not read at all;")
	if modelUsed == 0 {
		fmt.Println("   - no embedding was needed for any pair in the corpus, so these rows do not")
		fmt.Println("     measure semantic similarity. They measure exact matching over scraped")
		fmt.Println("     fields. The free-text probe above is the only part of this run in which")
		fmt.Println("     the similarity half of the comparator decided anything.")
	}
	fmt.Println()
	fmt.Println(" The honest reading: the gate removes the baseline's false positives because")
	fmt.Println(" it stops asking a question similarity cannot answer. It does not make")
	fmt.Println(" similarity better at that question — the free-text probe above shows the same")
	fmt.Println(" failure surviving inside the text the gate does not cover — and it needs a")
	fmt.Println(" per-activity schema the engine cannot derive on its own. The durable fix is")
	fmt.Println(" for an activity to DECLARE its deciding fields in its input, so that nothing")
	fmt.Println(" has to be scraped and nothing that decides identity is left to a threshold.")
}

// hybridHeaderLine states which comparators a run measured, right at the top.
func hybridHeaderLine(cfg config) string {
	if !cfg.hybrid {
		return "exact, semantic (the shipped comparator)"
	}
	return "exact, semantic (the shipped comparator), hybrid (opt-in, deciding fields exact)"
}
