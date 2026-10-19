package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/evalcorpus"
)

// ---------------------------------------------------------------------------
// THE REPORT
// ---------------------------------------------------------------------------
//
// Written for a tired student first and a reviewer second, which decides the
// order: the summary table is at the top and answers the question in one
// screen; everything that justifies it comes after. Every figure is computed
// from the CSV on disk, so a partial run produces a valid partial report.

const rule = "==========================================================================="

func printHeader(prov *provenance, pairs []evalPair) {
	fmt.Printf("%s\n WHERE SHOULD DECIDING FIELDS COME FROM — THREE EXTRACTORS, TEN DOMAINS\n%s\n", rule, rule)
	fmt.Printf(" pairs      : %d across %d corpora\n", len(pairs), len(prov.Corpora))
	fmt.Printf(" comparators: %s\n", strings.Join(prov.Arms, ", "))
	fmt.Printf(" embedder   : %s at %s\n", prov.EmbedModel, prov.Endpoint)
	if prov.LLMModel != "" {
		fmt.Printf(" extraction model: %s, temperature 0\n", prov.LLMModel)
	}
	fmt.Printf(" rows       : %s (flushed per row; re-running skips what is there)\n\n", prov.CSVPath)
}

// writeReport builds the whole report from the rows on disk and writes it to
// the text file and to the terminal.
func writeReport(cfg config, prov *provenance, rows []row) error {
	var b strings.Builder
	report(&b, cfg, prov, rows)
	out := b.String()

	if cfg.txtPath != "" {
		if err := os.WriteFile(cfg.txtPath, []byte(out), 0o644); err != nil {
			return err
		}
	}
	fmt.Print(out)
	if cfg.txtPath != "" {
		fmt.Printf("\nwrote the report to %s\n", cfg.txtPath)
	}
	return nil
}

func report(w io.Writer, cfg config, prov *provenance, rows []row) {
	th := core.Thresholds{TauLow: cfg.tauLow, TauHigh: cfg.tauHigh}
	comparators := comparatorsIn(rows, prov.Arms)
	corpora := corporaIn(rows)
	defaultPolicy, blockPolicy := policies[0], policies[1]

	writeRunHeader(w, cfg, prov, rows, corpora)
	writeCompleteness(w, rows, comparators, corpora)
	writeSummary(w, rows, comparators, th, defaultPolicy)
	writeVerificationNote(w, rows, th)
	writePolicySection(w, rows, comparators, th, defaultPolicy, blockPolicy)
	writePerCorpus(w, rows, comparators, corpora, th, defaultPolicy)
	writePerDomain(w, rows, comparators, th, defaultPolicy)
	writeRecognition(w, rows, comparators, th)
	writeModelConsulted(w, rows, comparators)
	writeMcNemar(w, rows, comparators, th, defaultPolicy)
	writeAblation(w, rows, comparators, th, defaultPolicy)
	writeSweep(w, cfg, rows, comparators, th, defaultPolicy)
	writeExtractionFailures(w, rows, th)
	writeCost(w, rows, comparators, prov)
	writeDisclosure(w)
	writeDiscipline(w, prov, rows)
}

// ---------------------------------------------------------------------------

func writeRunHeader(w io.Writer, cfg config, prov *provenance, rows []row, corpora []string) {
	fmt.Fprintf(w, "%s\n WHERE SHOULD DECIDING FIELDS COME FROM — THREE EXTRACTORS, TEN DOMAINS\n%s\n", rule, rule)
	fmt.Fprintf(w, " run at        : %s\n", prov.StartedAt.Format(time.RFC3339))
	if !prov.FinishedAt.IsZero() {
		fmt.Fprintf(w, " finished at   : %s (wall clock %s)\n",
			prov.FinishedAt.Format(time.RFC3339),
			prov.FinishedAt.Sub(prov.StartedAt).Round(time.Second))
	}
	fmt.Fprintf(w, " question      : the hybrid duplicate check needs the DECIDING FIELDS of an\n")
	fmt.Fprintf(w, "                 action. The engine had one way to find them — a hand-written\n")
	fmt.Fprintf(w, "                 flight vocabulary. This run scores three ways on the same pairs.\n")
	fmt.Fprintf(w, " embedder      : %s at %s (dim %d), real service, no fake embedder\n",
		prov.EmbedModel, prov.Endpoint, prov.Dim)
	if prov.LLMModel != "" {
		fmt.Fprintf(w, " extractor LLM : %s, temperature 0, max_tokens 400\n", prov.LLMModel)
	} else if prov.LLMSkipped {
		fmt.Fprintf(w, " extractor LLM : SKIPPED — GROQ_API_KEY was not set. Its arms are absent from\n")
		fmt.Fprintf(w, "                 every table below rather than reported as a zero.\n")
	} else {
		fmt.Fprintf(w, " extractor LLM : not requested (-llm was off)\n")
	}
	fmt.Fprintf(w, " thresholds    : tau_low %.2f / tau_high %.2f — THE SHIPPED VALUES, not tuned here\n",
		cfg.tauLow, cfg.tauHigh)
	fmt.Fprintf(w, " gate rules    : strict (absent counts as a difference — the published rule) and\n")
	fmt.Fprintf(w, "                 neutral (only two values both present and contradicting close the\n")
	fmt.Fprintf(w, "                 gate), the latter needing %d agreeing field(s) to pass a pair\n", prov.MinAgreeing)
	fmt.Fprintf(w, " a -cased arm  : the intent keeps its capitals. app/agent lowercases every tool\n")
	fmt.Fprintf(w, "                 call through agent.CanonicalIntent, which destroys the only signal\n")
	fmt.Fprintf(w, "                 the generic extractor's proper-noun rule has, so both are measured.\n")
	fmt.Fprintf(w, "                 A -cased arm is NOT the engine's current path, and it pays a second\n")
	fmt.Fprintf(w, "                 price: the flight corpus's \"surface\" pairs differ only in case and\n")
	fmt.Fprintf(w, "                 spacing, so without that canonicalisation they are no longer\n")
	fmt.Fprintf(w, "                 identical and a -cased arm can miss duplicates every other arm\n")
	fmt.Fprintf(w, "                 catches for free.\n")
	fmt.Fprintf(w, " policies      : both scored from the same rows — the engine's default\n")
	fmt.Fprintf(w, "                 (escalate runs the activity) and ESCALATION_BLOCKS=on\n")
	fmt.Fprintf(w, " repetitions   : %d timed checks per row; the fastest is recorded\n", prov.Repeat)
	fmt.Fprintf(w, " rows measured : %d in %s\n", len(rows), prov.CSVPath)

	fmt.Fprintf(w, "\n CORPORA\n")
	for _, cs := range prov.Corpora {
		fmt.Fprintf(w, "  %-16s %3d pairs, %3d duplicate, %3d distinct (%d lexically close), %d domain(s)\n",
			cs.Name, cs.Pairs, cs.Duplicates, cs.Distincts, cs.Hard, cs.Domains)
		fmt.Fprintf(w, "  %-16s %s\n", "", wrap(cs.Labels, 70, 19))
	}
	if prov.RephraseUsable > 0 {
		lo, hi := wilson(prov.RephraseReworded, prov.RephraseUsable)
		fmt.Fprintf(w, "\n THE PREMISE, RESTATED WITH AN INTERVAL\n")
		fmt.Fprintf(w, "  A real planner reworded the lost action on %d of %d usable trials: %.3f\n",
			prov.RephraseReworded, prov.RephraseUsable,
			float64(prov.RephraseReworded)/float64(prov.RephraseUsable))
		fmt.Fprintf(w, "  with a 95%% Wilson interval of [%.2f, %.2f]. If that rate were near zero,\n", lo, hi)
		fmt.Fprintf(w, "  an exact-text duplicate check would be sufficient and none of this would be\n")
		fmt.Fprintf(w, "  needed. The figure is a property of THAT model and THAT prompt, measured in\n")
		fmt.Fprintf(w, "  the run named below, not re-measured here.\n\n")
	}
	if prov.RephraseSource != "" {
		fmt.Fprintf(w, "  live source     : %s\n", prov.RephraseSource)
		fmt.Fprintf(w, "                    %s\n", wrap(prov.RephraseNote, 70, 20))
	} else if prov.RephraseNote != "" {
		fmt.Fprintf(w, "  live pairs      : %s\n", wrap(prov.RephraseNote, 70, 20))
	}

	if len(prov.DomainShape) > 0 {
		fmt.Fprintf(w, "\n THE TEN DOMAINS WERE CHOSEN FOR THE SHAPE OF THEIR DECIDING FIELDS\n")
		for _, d := range evalcorpus.DomainOrder {
			if s, ok := prov.DomainShape[d]; ok {
				fmt.Fprintf(w, "  %-13s %s\n", d, s)
			}
		}
	}

	fmt.Fprintf(w, "\n command       : %s\n", prov.CommandLine)
	fmt.Fprintf(w, " resume        : %s\n", prov.ResumeCommand)
	fmt.Fprintf(w, " report only   : go run ./cmd/fieldeval -report-only -csv=%s -txt=<where>\n", prov.CSVPath)
	fmt.Fprintf(w, "                 (recomputes every table from the CSV; needs no model and no embedder)\n")
}

// ---------------------------------------------------------------------------

// writeCompleteness says, before any figure, how much of the matrix was
// actually measured.
//
// It is first because this harness is resumable and therefore CAN be read
// part way through, and a table computed over a third of a comparator's pairs
// looks exactly like a table computed over all of them. An arm that is short
// of rows, or missing a whole corpus, has to say so above its own numbers
// rather than below them.
func writeCompleteness(w io.Writer, rows []row, comparators, corpora []string) {
	fmt.Fprintf(w, "\n%s\n HOW MUCH OF THE MATRIX THIS FILE COVERS\n%s\n", rule, rule)

	// The expected count per comparator is the number of distinct pairs any
	// comparator was measured on, which is the corpus size.
	pairsPer := map[string]map[string]bool{}
	for _, r := range rows {
		if pairsPer[r.Corpus] == nil {
			pairsPer[r.Corpus] = map[string]bool{}
		}
		pairsPer[r.Corpus][r.PairID] = true
	}
	expected := 0
	for _, c := range corpora {
		expected += len(pairsPer[c])
	}

	fmt.Fprintf(w, "\n %-38s", "comparator")
	for _, c := range corpora {
		fmt.Fprintf(w, " | %-16s", c)
	}
	fmt.Fprintf(w, " | %s\n", "errors")
	fmt.Fprintf(w, " %s\n", strings.Repeat("-", 38+19*len(corpora)+9))

	short := false
	for _, name := range comparators {
		counts := map[string]int{}
		errs := 0
		for _, r := range rows {
			if r.Comparator != name {
				continue
			}
			if strings.HasPrefix(r.Note, "ERROR") {
				errs++
				continue
			}
			counts[r.Corpus]++
		}
		fmt.Fprintf(w, " %-38s", name)
		total := 0
		for _, c := range corpora {
			want := len(pairsPer[c])
			got := counts[c]
			total += got
			mark := " "
			if got < want {
				mark, short = "!", true
			}
			fmt.Fprintf(w, " | %4d/%-4d %s      ", got, want, mark)
		}
		fmt.Fprintf(w, " | %d\n", errs)
		if total < expected {
			short = true
		}
	}
	if short {
		fmt.Fprintf(w, "\n  ! THIS ARM IS INCOMPLETE. Its figures below are computed over the rows it has\n")
		fmt.Fprintf(w, "    and nothing else. The corpora are interleaved by domain and by label, so a\n")
		fmt.Fprintf(w, "    prefix is a balanced sample rather than the first domain only — but a rate\n")
		fmt.Fprintf(w, "    over fifty pairs is not a rate over five hundred, and the intervals are the\n")
		fmt.Fprintf(w, "    only honest guide to the difference. Re-run the resume command to finish it.\n")
	} else {
		fmt.Fprintf(w, "\n  Every comparator was measured on every pair of every corpus listed.\n")
	}
	fmt.Fprintf(w, "  An ERROR row is one the harness could not measure — a model that did not\n")
	fmt.Fprintf(w, "  answer, for instance. ERROR rows are excluded from every figure and are\n")
	fmt.Fprintf(w, "  retried by a resume, so they are never silently counted as a result.\n")
}

// writeSummary is the one screen a tired reader should be able to stop at.
func writeSummary(w io.Writer, rows []row, comparators []string, th core.Thresholds, p policy) {
	fmt.Fprintf(w, "\n%s\n THE ANSWER IN ONE TABLE\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n On the balanced ten-domain corpus — the only one here with designed negatives,\n")
	fmt.Fprintf(w, " so the only one that can report a false-positive rate. Shipped thresholds,\n")
	fmt.Fprintf(w, " the engine's default escalation policy. 95%% Wilson intervals in brackets.\n\n")

	fmt.Fprintf(w, " %-28s | %-19s | %-19s | %-19s | %5s | %5s\n",
		"comparator", "caught of duplicates", "wrongly suppressed", "of the hard ones", "F1", "read")
	fmt.Fprintf(w, " %s\n", strings.Repeat("-", 118))
	for _, name := range comparators {
		c := score(rows, filter{Corpus: corpusMultiDomain, Comparator: name}, th, p, ablationFull)
		if c.Total == 0 {
			continue
		}
		writeSummaryRow(w, name, c)

		// The gate alone, immediately under the comparator it belongs to.
		//
		// It is in the HEADLINE table and not only in the ablation section
		// because on this corpus it is the strongest thing measured, and a
		// finding that only appears on page six is a finding a reader will
		// quote the wrong number instead of.
		if hasExtractor(rows, name) {
			g := score(rows, filter{Corpus: corpusMultiDomain, Comparator: name}, th, p, ablationGate)
			writeSummaryRow(w, "  \u21b3 its gate alone", g)
		}
	}

	fmt.Fprintf(w, "\n  its gate alone     the same comparator with similarity removed: a pair whose every\n")
	fmt.Fprintf(w, "                     deciding field agrees is called a duplicate, and the leftover\n")
	fmt.Fprintf(w, "                     prose is never consulted. Where this beats the row above it,\n")
	fmt.Fprintf(w, "                     the similarity half is DESTROYING the comparator, not helping it.\n")
	fmt.Fprintf(w, "\n  caught             how many genuine duplicates the comparator prevented. Higher is better.\n")
	fmt.Fprintf(w, "  wrongly suppressed how many genuinely different actions it stopped. LOWER IS BETTER;\n")
	fmt.Fprintf(w, "                     every one of these is a booking, payment or refund that would\n")
	fmt.Fprintf(w, "                     not have happened.\n")
	fmt.Fprintf(w, "  of the hard ones   the same, over the negatives that share most of their words with\n")
	fmt.Fprintf(w, "                     their partner. This is the number worth reading.\n")
	fmt.Fprintf(w, "  read               how often the field extractor could read BOTH sides. Where this is\n")
	fmt.Fprintf(w, "                     low the row is not the gate working, it is the gate falling back\n")
	fmt.Fprintf(w, "                     to the pure-semantic comparator and reporting its verdict.\n")

	// The live corpus, which can only speak to detection.
	live := score(rows, filter{Corpus: corpusRephrase}, th, p, ablationFull)
	if live.Total > 0 {
		fmt.Fprintf(w, "\n AND ON THE REAL WORDINGS A LIVE PLANNER PRODUCED (detection only — this corpus\n")
		fmt.Fprintf(w, " has no negative class, so it can say nothing about wrongly suppressed actions)\n\n")
		fmt.Fprintf(w, " %-28s | %-19s | %s\n", "comparator", "caught of rewordings", "extractor read both sides")
		fmt.Fprintf(w, " %s\n", strings.Repeat("-", 84))
		for _, name := range comparators {
			c := score(rows, filter{Corpus: corpusRephrase, Comparator: name}, th, p, ablationFull)
			if c.Total == 0 {
				continue
			}
			tp, nd := c.Detect()
			fmt.Fprintf(w, " %-28s | %s | %3d of %3d\n", name, rateCI(tp, nd), c.Recognised, c.Total)
		}
	}
}

func writeSummaryRow(w io.Writer, name string, c confusion) {
	tp, nd := c.Detect()
	fp, nn := c.FPR()
	hfp, nh := c.HardFPR()
	fmt.Fprintf(w, " %-28s | %s | %s | %s | %5s | %4.0f%%\n",
		name, rateCI(tp, nd), rateCI(fp, nn), rateCI(hfp, nh),
		fmtF(c.F1(), 3), 100*float64(c.Recognised)/float64(max(1, c.Total)))
}

func writeVerificationNote(w io.Writer, rows []row, th core.Thresholds) {
	bad := verifyAgainstChecker(rows, th)
	fmt.Fprintf(w, "\n%s\n IS THE SCORING THE ENGINE'S OWN?\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n Every verdict above is derived from one stored similarity and the gate's answer,\n")
	fmt.Fprintf(w, " which is what lets one pass over the corpus score every threshold and both\n")
	fmt.Fprintf(w, " policies. That is only valid while the derivation agrees with semantic.Checker\n")
	fmt.Fprintf(w, " exactly, so every row's shipped-threshold verdict was re-derived and compared\n")
	fmt.Fprintf(w, " with the verdict the checker actually returned.\n\n")
	if len(bad) == 0 {
		fmt.Fprintf(w, "  AGREED on every row. The numbers in this file are the engine's own comparator's.\n")
		return
	}
	fmt.Fprintf(w, "  *** DISAGREED on %d row(s). EVERY FIGURE IN THIS FILE IS SUSPECT. ***\n", len(bad))
	for _, s := range bad {
		fmt.Fprintf(w, "    %s\n", s)
	}
}

// ---------------------------------------------------------------------------

func writePolicySection(w io.Writer, rows []row, comparators []string, th core.Thresholds, def, block policy) {
	fmt.Fprintf(w, "\n%s\n THE ESCALATION POLICY, WHICH IS NOT AN EXTRACTOR AT ALL\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n The engine already has a setting for this: ESCALATION_BLOCKS, in internal/config,\n")
	fmt.Fprintf(w, " off by default. With it off an uncertain verdict RUNS the activity, so only a\n")
	fmt.Fprintf(w, " DUPLICATE prevents a repeat. With it on, an uncertain verdict holds the activity\n")
	fmt.Fprintf(w, " for a person. It is one line of configuration and no extraction at all, so it is\n")
	fmt.Fprintf(w, " the cheapest thing in this file and has to be measured against the rest.\n\n")

	for _, corpus := range []string{corpusMultiDomain, corpusRephrase} {
		if score(rows, filter{Corpus: corpus}, th, def, ablationFull).Total == 0 {
			continue
		}
		fmt.Fprintf(w, " %s\n", corpus)
		fmt.Fprintf(w, " %-28s | %-13s | %-13s | %-13s | %-13s | %s\n",
			"comparator", "caught (def)", "caught (block)", "wrong (def)", "wrong (block)", "escalations")
		fmt.Fprintf(w, " %s\n", strings.Repeat("-", 116))
		for _, name := range comparators {
			cd := score(rows, filter{Corpus: corpus, Comparator: name}, th, def, ablationFull)
			cb := score(rows, filter{Corpus: corpus, Comparator: name}, th, block, ablationFull)
			if cd.Total == 0 {
				continue
			}
			tpd, nd := cd.Detect()
			tpb, _ := cb.Detect()
			fpd, nn := cd.FPR()
			fpb, _ := cb.FPR()
			fmt.Fprintf(w, " %-28s | %4d/%-8d | %4d/%-8d | %4d/%-8d | %4d/%-8d | %d\n",
				name, tpd, nd, tpb, nd, fpd, nn, fpb, nn, cb.Escalated)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  def   %s\n", wrap(def.Note, 68, 8))
	fmt.Fprintf(w, "  block %s\n", wrap(block.Note, 68, 8))
	fmt.Fprintf(w, "\n  An escalation is a question put to a person. A policy that catches more by\n")
	fmt.Fprintf(w, "  escalating more has not improved the comparator; it has moved work to a human,\n")
	fmt.Fprintf(w, "  and the escalation column is what that costs. Read the two together.\n")
}

// ---------------------------------------------------------------------------

func writePerCorpus(w io.Writer, rows []row, comparators, corpora []string, th core.Thresholds, p policy) {
	fmt.Fprintf(w, "\n%s\n FULL CONFUSION MATRIX, PER CORPUS\n%s\n", rule, rule)
	for _, corpus := range corpora {
		fmt.Fprintf(w, "\n %s\n", corpus)
		fmt.Fprintf(w, " %-28s | %4s %4s %4s %4s %4s | %-13s | %-13s | %6s %6s %6s\n",
			"comparator", "TP", "FP", "TN", "FN", "esc", "recall", "FPR", "prec", "F1", "acc")
		fmt.Fprintf(w, " %s\n", strings.Repeat("-", 118))
		for _, name := range comparators {
			c := score(rows, filter{Corpus: corpus, Comparator: name}, th, p, ablationFull)
			if c.Total == 0 {
				continue
			}
			tp, nd := c.Detect()
			fp, nn := c.FPR()
			fmt.Fprintf(w, " %-28s | %4d %4d %4d %4d %4d | %s | %s | %6s %6s %6s\n",
				name, c.TP, c.FP, c.TN, c.FN, c.Escalated,
				rateCI(tp, nd), rateCI(fp, nn),
				fmtF(c.Precision(), 3), fmtF(c.F1(), 3), fmtF(c.Accuracy(), 3))
		}
		if corpus == corpusRephrase {
			fmt.Fprintf(w, "  Every pair in this corpus is a duplicate, so FP, TN and FPR are structurally\n")
			fmt.Fprintf(w, "  zero and precision is 1.000 by construction. They are printed for completeness\n")
			fmt.Fprintf(w, "  and MUST NOT be quoted: this corpus can only measure detection.\n")
		}
	}
}

// ---------------------------------------------------------------------------

func writePerDomain(w io.Writer, rows []row, comparators []string, th core.Thresholds, p policy) {
	fmt.Fprintf(w, "\n%s\n PER DOMAIN — HOW FAR EACH WAY OF FINDING FIELDS TRAVELS\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n This is the question the run exists to answer. Fifteen duplicates and fifteen\n")
	fmt.Fprintf(w, " designed negatives per domain, so every cell has the same denominator and the\n")
	fmt.Fprintf(w, " intervals are honest about how small that is.\n")

	domains := domainsIn(rows, corpusMultiDomain)
	for _, name := range comparators {
		if score(rows, filter{Corpus: corpusMultiDomain, Comparator: name}, th, p, ablationFull).Total == 0 {
			continue
		}
		fmt.Fprintf(w, "\n %s\n", name)
		fmt.Fprintf(w, " %-13s | %-19s | %-19s | %-19s | %5s\n",
			"domain", "caught", "wrongly suppressed", "hard ones wrongly", "read")
		fmt.Fprintf(w, " %s\n", strings.Repeat("-", 92))
		gateOnly := hasExtractor(rows, name)
		for _, d := range domains {
			f := filter{Corpus: corpusMultiDomain, Comparator: name, Domain: d}
			c := score(rows, f, th, p, ablationFull)
			if c.Total == 0 {
				continue
			}
			tp, nd := c.Detect()
			fp, nn := c.FPR()
			hfp, nh := c.HardFPR()
			fmt.Fprintf(w, " %-13s | %s | %s | %s | %4.0f%%\n",
				d, rateCI(tp, nd), rateCI(fp, nn), rateCI(hfp, nh),
				100*float64(c.Recognised)/float64(max(1, c.Total)))
			if gateOnly {
				g := score(rows, f, th, p, ablationGate)
				gtp, gnd := g.Detect()
				gfp, gnn := g.FPR()
				ghfp, gnh := g.HardFPR()
				fmt.Fprintf(w, " %-13s | %s | %s | %s |\n",
					"  \u21b3 gate", rateCI(gtp, gnd), rateCI(gfp, gnn), rateCI(ghfp, gnh))
			}
		}
	}

	// The live corpus per domain, detection only.
	if score(rows, filter{Corpus: corpusRephrase}, th, p, ablationFull).Total > 0 {
		fmt.Fprintf(w, "\n AND ON THE LIVE REWORDINGS, PER DOMAIN (detection only)\n")
		liveDomains := domainsIn(rows, corpusRephrase)
		fmt.Fprintf(w, " %-28s", "comparator")
		for _, d := range liveDomains {
			fmt.Fprintf(w, " | %-11s", d)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, " %s\n", strings.Repeat("-", 28+14*len(liveDomains)))
		for _, name := range comparators {
			if score(rows, filter{Corpus: corpusRephrase, Comparator: name}, th, p, ablationFull).Total == 0 {
				continue
			}
			fmt.Fprintf(w, " %-28s", name)
			for _, d := range liveDomains {
				c := score(rows, filter{Corpus: corpusRephrase, Comparator: name, Domain: d}, th, p, ablationFull)
				tp, nd := c.Detect()
				if nd == 0 {
					fmt.Fprintf(w, " | %-11s", "n/a")
					continue
				}
				fmt.Fprintf(w, " | %2d/%-2d %4.0f%%", tp, nd, 100*float64(tp)/float64(nd))
			}
			fmt.Fprintln(w)
		}
	}
}

// ---------------------------------------------------------------------------

func writeRecognition(w io.Writer, rows []row, comparators []string, th core.Thresholds) {
	fmt.Fprintf(w, "\n%s\n RECOGNITION: COULD THE EXTRACTOR READ THE INTENT AT ALL?\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n A pair the extractor cannot read is not gated: the comparator falls back to the\n")
	fmt.Fprintf(w, " pure-semantic path and journals that it did. So a hybrid row with a low\n")
	fmt.Fprintf(w, " recognition rate is reporting the pure-semantic comparator's verdict under a\n")
	fmt.Fprintf(w, " hybrid name, and reading it as the gate's work would be wrong.\n\n")

	for _, corpus := range []string{corpusMultiDomain, corpusFlight, corpusRephrase} {
		domains := domainsIn(rows, corpus)
		if len(domains) == 0 {
			continue
		}
		fmt.Fprintf(w, " %s\n", corpus)
		fmt.Fprintf(w, " %-28s", "comparator")
		for _, d := range domains {
			fmt.Fprintf(w, " | %-8s", trunc(d, 8))
		}
		fmt.Fprintf(w, " | overall\n")
		fmt.Fprintf(w, " %s\n", strings.Repeat("-", 28+11*len(domains)+10))
		for _, name := range comparators {
			all := score(rows, filter{Corpus: corpus, Comparator: name}, th, policies[0], ablationFull)
			if all.Total == 0 || !hasExtractor(rows, name) {
				continue
			}
			fmt.Fprintf(w, " %-28s", name)
			for _, d := range domains {
				c := score(rows, filter{Corpus: corpus, Comparator: name, Domain: d}, th, policies[0], ablationFull)
				if c.Total == 0 {
					fmt.Fprintf(w, " | %-8s", "n/a")
					continue
				}
				fmt.Fprintf(w, " | %6.0f%%", 100*float64(c.Recognised)/float64(c.Total))
			}
			fmt.Fprintf(w, " | %6.0f%%\n", 100*float64(all.Recognised)/float64(all.Total))
		}
		fmt.Fprintln(w)
	}
}

func hasExtractor(rows []row, comparator string) bool {
	for _, r := range rows {
		if r.Comparator == comparator {
			return r.Extractor != ""
		}
	}
	return false
}

// ---------------------------------------------------------------------------

func writeModelConsulted(w io.Writer, rows []row, comparators []string) {
	fmt.Fprintf(w, "\n%s\n WAS THE EMBEDDING MODEL ACTUALLY CONSULTED?\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n This section exists because of what the published hybrid run had to record:\n")
	fmt.Fprintf(w, " it scored 1.000 on 213 pairs and THE MODEL WAS NEVER CONSULTED ON ANY OF THEM,\n")
	fmt.Fprintf(w, " because once the deciding fields were removed no free text was left. A figure\n")
	fmt.Fprintf(w, " reached that way measures exact matching over scraped values, not semantic\n")
	fmt.Fprintf(w, " similarity. The same question is asked here of every arm.\n\n")

	fmt.Fprintf(w, " %-28s | %-16s | %-16s | %-16s | %s\n",
		"comparator", "corpus", "model consulted", "gate decided", "fell back")
	fmt.Fprintf(w, " %s\n", strings.Repeat("-", 104))
	for _, name := range comparators {
		for _, corpus := range []string{corpusMultiDomain, corpusFlight, corpusRephrase} {
			var total, used, gated, fell int
			for _, r := range rows {
				if r.Comparator != name || r.Corpus != corpus {
					continue
				}
				if strings.HasPrefix(r.Note, "ERROR") {
					continue
				}
				total++
				if r.ModelUsed {
					used++
				}
				if r.Gated {
					gated++
				}
				if !r.RecognisedA || !r.RecognisedB {
					fell++
				}
			}
			if total == 0 {
				continue
			}
			fmt.Fprintf(w, " %-28s | %-16s | %4d %5.1f%%     | %4d %5.1f%%     | %4d %5.1f%%\n",
				name, corpus, used, 100*float64(used)/float64(total),
				gated, 100*float64(gated)/float64(total),
				fell, 100*float64(fell)/float64(total))
		}
	}
	fmt.Fprintf(w, "\n  model consulted  an embedding was needed to answer. Where this is 0%%, THE ARM IS\n")
	fmt.Fprintf(w, "                   MEASURING EXACT MATCHING AND NOT THE CONTRIBUTION OF SIMILARITY.\n")
	fmt.Fprintf(w, "  gate decided     a deciding field differed, so no threshold could have mattered.\n")
	fmt.Fprintf(w, "  fell back        the extractor could not read a side, so the verdict is the\n")
	fmt.Fprintf(w, "                   pure-semantic one.\n")
}

// ---------------------------------------------------------------------------

func writeMcNemar(w io.Writer, rows []row, comparators []string, th core.Thresholds, p policy) {
	fmt.Fprintf(w, "\n%s\n IS THE DIFFERENCE REAL? McNEMAR'S EXACT TEST ON THE PAIRED ARMS\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n Two comparators scored on one corpus are not two independent samples: they saw\n")
	fmt.Fprintf(w, " the same pairs. McNemar's test looks only at the pairs they DISAGREED on and\n")
	fmt.Fprintf(w, " asks whether those fall more on one side than chance explains. The p-value is\n")
	fmt.Fprintf(w, " the exact two-sided binomial one, not the chi-squared approximation, because\n")
	fmt.Fprintf(w, " the discordant counts here are small.\n")
	fmt.Fprintf(w, "\n Correct means the comparator's verdict matched the label: it prevented a\n")
	fmt.Fprintf(w, " duplicate, or it let a genuinely different action through.\n")

	baseline := "semantic"
	for _, corpus := range []string{corpusMultiDomain, corpusFlight, corpusRephrase} {
		if score(rows, filter{Corpus: corpus, Comparator: baseline}, th, p, ablationFull).Total == 0 {
			continue
		}
		fmt.Fprintf(w, "\n %s, each comparator against %q\n", corpus, baseline)
		fmt.Fprintf(w, " %-28s | %7s | %7s | %10s | %9s | %s\n",
			"comparator", "only it", "only base", "discordant", "p (exact)", "verdict")
		fmt.Fprintf(w, " %s\n", strings.Repeat("-", 104))
		for _, name := range comparators {
			if name == baseline {
				continue
			}
			if score(rows, filter{Corpus: corpus, Comparator: name}, th, p, ablationFull).Total == 0 {
				continue
			}
			m := mcnemar(rows, corpus, name, baseline, th, p)
			verdict := "no evidence of a difference"
			switch {
			case m.Discordant() == 0:
				verdict = "identical on every pair"
			case math.IsNaN(m.P):
				verdict = "not testable"
			case m.P < 0.05 && m.OnlyA > m.OnlyB:
				verdict = "BETTER than the baseline (p<0.05)"
			case m.P < 0.05 && m.OnlyB > m.OnlyA:
				verdict = "WORSE than the baseline (p<0.05)"
			}
			fmt.Fprintf(w, " %-28s | %7d | %9d | %10d | %9s | %s\n",
				name, m.OnlyA, m.OnlyB, m.Discordant(), fmtP(m.P), verdict)
		}
	}
}

// ---------------------------------------------------------------------------

func writeAblation(w io.Writer, rows []row, comparators []string, th core.Thresholds, p policy) {
	fmt.Fprintf(w, "\n%s\n ABLATION: WHICH HALF OF THE COMPARATOR IS DOING THE WORK?\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n Four rows per comparator, on the balanced corpus, at the shipped thresholds:\n")
	fmt.Fprintf(w, "   exact only       the engine's byte key, no model, no gate\n")
	fmt.Fprintf(w, "   similarity only  the shipped pure-semantic path: no gate at all\n")
	fmt.Fprintf(w, "   gate only        the gate's veto with similarity removed — a pair that passes\n")
	fmt.Fprintf(w, "                    the gate is called a duplicate whatever its free text says\n")
	fmt.Fprintf(w, "   gate+similarity  the comparator as built\n\n")

	fmt.Fprintf(w, " %-28s | %-16s | %-19s | %-19s | %s\n",
		"comparator", "ablation", "caught", "wrongly suppressed", "F1")
	fmt.Fprintf(w, " %s\n", strings.Repeat("-", 110))

	ex := score(rows, filter{Corpus: corpusMultiDomain, Comparator: "exact"}, th, p, ablationFull)
	if ex.Total > 0 {
		writeAblationRow(w, "exact", "exact only", ex)
	}
	sem := score(rows, filter{Corpus: corpusMultiDomain, Comparator: "semantic"}, th, p, ablationFull)
	if sem.Total > 0 {
		writeAblationRow(w, "semantic", "similarity only", sem)
	}
	for _, name := range comparators {
		if !hasExtractor(rows, name) {
			continue
		}
		g := score(rows, filter{Corpus: corpusMultiDomain, Comparator: name}, th, p, ablationGate)
		f := score(rows, filter{Corpus: corpusMultiDomain, Comparator: name}, th, p, ablationFull)
		if f.Total == 0 {
			continue
		}
		writeAblationRow(w, name, "gate only", g)
		writeAblationRow(w, name, "gate+similarity", f)
	}
	fmt.Fprintf(w, "\n  Where a comparator's gate-only row equals its gate+similarity row, SIMILARITY\n")
	fmt.Fprintf(w, "  CONTRIBUTED NOTHING on this corpus and the figure is the extractor's alone.\n")
}

func writeAblationRow(w io.Writer, name, ab string, c confusion) {
	tp, nd := c.Detect()
	fp, nn := c.FPR()
	fmt.Fprintf(w, " %-28s | %-16s | %s | %s | %s\n",
		name, ab, rateCI(tp, nd), rateCI(fp, nn), fmtF(c.F1(), 3))
}

// ---------------------------------------------------------------------------

func writeSweep(w io.Writer, cfg config, rows []row, comparators []string, th core.Thresholds, p policy) {
	fmt.Fprintf(w, "\n%s\n THRESHOLD SWEEP — PRECISION AND RECALL AS THE THRESHOLD MOVES\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n On the balanced corpus. The shipped tau_high is marked with a *. IT WAS NOT\n")
	fmt.Fprintf(w, " CHOSEN FROM THIS TABLE: reading off the best row and quoting it is selection on\n")
	fmt.Fprintf(w, " the test set, and this sweep is a sensitivity check, not a tuning result.\n")

	for _, name := range comparators {
		if name == "exact" {
			continue // no threshold to sweep
		}
		pts := sweep(rows, filter{Corpus: corpusMultiDomain, Comparator: name}, p, ablationFull,
			cfg.tauMin, cfg.tauMax, cfg.tauStep, th.TauLow, th.TauHigh)
		if len(pts) == 0 || pts[0].Conf.Total == 0 {
			continue
		}
		fmt.Fprintf(w, "\n %s\n", name)
		fmt.Fprintf(w, "  %6s | %5s %5s %5s %5s | %6s %6s %6s %6s\n",
			"tau", "TP", "FP", "TN", "FN", "recall", "FPR", "prec", "F1")
		fmt.Fprintf(w, "  %s\n", strings.Repeat("-", 64))
		for _, pt := range pts {
			mark := " "
			if pt.IsShipped {
				mark = "*"
			}
			c := pt.Conf
			fmt.Fprintf(w, " %s%5.2f | %5d %5d %5d %5d | %6s %6s %6s %6s\n",
				mark, pt.Tau, c.TP, c.FP, c.TN, c.FN,
				fmtF(c.Recall(), 3), fmtF(ratio(c.FPR()), 3),
				fmtF(c.Precision(), 3), fmtF(c.F1(), 3))
		}
	}
}

func ratio(k, n int) float64 {
	if n == 0 {
		return math.NaN()
	}
	return float64(k) / float64(n)
}

// ---------------------------------------------------------------------------

// writeExtractionFailures is the honesty section the brief demands by name:
// where each extractor over-extracted and where it under-extracted, with real
// examples from this run.
func writeExtractionFailures(w io.Writer, rows []row, th core.Thresholds) {
	fmt.Fprintf(w, "\n%s\n WHERE THE EXTRACTORS WENT WRONG, WITH EXAMPLES FROM THIS RUN\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n Two failure modes, and both are inherent rather than bugs to be patched:\n")
	fmt.Fprintf(w, "   OVER-EXTRACTION  a value that decides nothing becomes a deciding field, so the\n")
	fmt.Fprintf(w, "                    gate closes on two phrasings of one action. Shows up as a\n")
	fmt.Fprintf(w, "                    DUPLICATE the gate rejected.\n")
	fmt.Fprintf(w, "   UNDER-EXTRACTION a value carried by prose alone is invisible, so the intent is\n")
	fmt.Fprintf(w, "                    unreadable or the gate has nothing to hold. Shows up as a\n")
	fmt.Fprintf(w, "                    fall-back, or as a DISTINCT pair the gate waved through.\n")

	byComparator := map[string][]row{}
	for _, r := range rows {
		if r.Extractor == "" || strings.HasPrefix(r.Note, "ERROR") {
			continue
		}
		byComparator[r.Comparator] = append(byComparator[r.Comparator], r)
	}
	names := make([]string, 0, len(byComparator))
	for n := range byComparator {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		rs := byComparator[name]
		fmt.Fprintf(w, "\n %s\n", name)

		// Over-extraction: duplicates the gate rejected, grouped by field.
		byField := map[string]int{}
		var examples []row
		for _, r := range rs {
			if r.Truth && r.Gated {
				byField[r.GateField]++
				if len(examples) < 3 {
					examples = append(examples, r)
				}
			}
		}
		lost := 0
		for _, n := range byField {
			lost += n
		}
		fmt.Fprintf(w, "  OVER-EXTRACTION: %d duplicate pair(s) the gate rejected", lost)
		if lost == 0 {
			fmt.Fprintf(w, " — none\n")
		} else {
			fmt.Fprintf(w, ", by the field that closed it:\n")
			for _, f := range sortedKeys(byField) {
				fmt.Fprintf(w, "    %-44s %d\n", trunc(f, 44), byField[f])
			}
			for _, r := range examples {
				fmt.Fprintf(w, "    e.g. %s (%s)\n", r.PairID, r.Domain)
				fmt.Fprintf(w, "         before: %s\n", trunc(r.SignatureA, 100))
				fmt.Fprintf(w, "         after : %s\n", trunc(r.SignatureB, 100))
			}
		}

		// Under-extraction: unreadable intents, and passed negatives.
		unread := map[string]int{}
		var unreadEx []row
		for _, r := range rs {
			if !r.RecognisedA || !r.RecognisedB {
				unread[r.Domain]++
				if len(unreadEx) < 3 {
					unreadEx = append(unreadEx, r)
				}
			}
		}
		total := 0
		for _, n := range unread {
			total += n
		}
		fmt.Fprintf(w, "  UNDER-EXTRACTION: %d pair(s) where a side could not be read at all", total)
		if total == 0 {
			fmt.Fprintf(w, " — none\n")
		} else {
			fmt.Fprintf(w, ", by domain:\n")
			for _, d := range sortedKeys(unread) {
				fmt.Fprintf(w, "    %-20s %d\n", d, unread[d])
			}
			for _, r := range unreadEx {
				fmt.Fprintf(w, "    e.g. %s (%s): %s\n", r.PairID, r.Domain, trunc(r.SignatureB, 90))
			}
		}

		// Negatives the gate let through: the dangerous direction.
		passed, hardPassed := 0, 0
		var passedEx []row
		for _, r := range rs {
			if !r.Truth && !r.Gated && r.RecognisedA && r.RecognisedB {
				passed++
				if r.Hard {
					hardPassed++
					if len(passedEx) < 3 {
						passedEx = append(passedEx, r)
					}
				}
			}
		}
		fmt.Fprintf(w, "  GATE PASSED A GENUINELY DIFFERENT ACTION: %d pair(s), %d of them lexically close\n",
			passed, hardPassed)
		for _, r := range passedEx {
			fmt.Fprintf(w, "    e.g. %s (%s), %d agreeing field(s), similarity %.4f -> %s\n",
				r.PairID, r.Domain, r.AgreeingFields, r.Similarity,
				deriveVerdict(r, th, ablationFull))
			fmt.Fprintf(w, "         both read as: %s\n", trunc(r.SignatureB, 100))
		}
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// ---------------------------------------------------------------------------

func writeCost(w io.Writer, rows []row, comparators []string, prov *provenance) {
	fmt.Fprintf(w, "\n%s\n COST\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n Check latency, over every row of every corpus. The embeddings were served from\n")
	fmt.Fprintf(w, " the local cache during the timed section, so these are the engine's check path\n")
	fmt.Fprintf(w, " WITHOUT the model round-trip; the round-trip is on its own line below.\n\n")
	fmt.Fprintf(w, " %-28s | %10s | %10s | %s\n", "comparator", "p50", "p99", "rows")
	fmt.Fprintf(w, " %s\n", strings.Repeat("-", 64))
	for _, name := range comparators {
		var c confusion
		for _, r := range rows {
			if strings.HasPrefix(r.Note, "ERROR") {
				continue
			}
			if r.Comparator == name && r.LatencyUS > 0 {
				c.Latencies = append(c.Latencies, r.LatencyUS)
			}
		}
		if len(c.Latencies) == 0 {
			continue
		}
		fmt.Fprintf(w, " %-28s | %8.1fus | %8.1fus | %d\n",
			name, c.Percentile(0.50), c.Percentile(0.99), len(c.Latencies))
	}

	fmt.Fprintf(w, "\n Embedding round-trips actually made this run: %d", prov.EmbedCalls)
	if prov.EmbedCalls > 0 {
		fmt.Fprintf(w, " (p50 %s, p99 %s)",
			prov.EmbedP50.Round(time.Microsecond), prov.EmbedP99.Round(time.Microsecond))
	}
	fmt.Fprintf(w, "\n  Anything not counted there was served from the shared cache in %s,\n", prov.CacheDir)
	fmt.Fprintf(w, "  which cmd/experiment also writes, so a vector is bit-identical across harnesses.\n")

	if prov.LLMModel != "" {
		fmt.Fprintf(w, "\n THE LLM EXTRACTOR'S OWN COST\n")
		fmt.Fprintf(w, "  model                      %s at temperature 0\n", prov.LLMModel)
		fmt.Fprintf(w, "  extraction calls made      %d (counted, not estimated)\n", prov.LLMCallsMade)
		fmt.Fprintf(w, "  calls that failed          %d\n", prov.LLMFailures)
		if prov.LLMCallsMade > 0 {
			per := time.Duration(prov.LLMNanos / prov.LLMCallsMade)
			fmt.Fprintf(w, "  mean wall clock per call   %s\n", per.Round(time.Millisecond))
			fmt.Fprintf(w, "  total spent extracting     %s\n",
				time.Duration(prov.LLMNanos).Round(time.Second))
		}
		fmt.Fprintf(w, "  Every call is ONE per intent text, cached on disk, so the figures above are\n")
		fmt.Fprintf(w, "  the cost of a cold run. A warm re-run makes none. In the engine the cost would\n")
		fmt.Fprintf(w, "  be paid once per activity scheduled, on the live path, before the verdict is\n")
		fmt.Fprintf(w, "  journaled — a round-trip of this size added to every semantic check.\n")
	}
}

// ---------------------------------------------------------------------------

// writeDisclosure is the section the project's own rules require: anything
// changed after seeing scores has to be declared, with both sets of numbers
// available.
//
// It names the changes and points at the earlier run's own result file rather
// than quoting its figures here. A generated report that hard-coded numbers
// from a different run would be the exact thing this project forbids, and a
// reader who wants the comparison can read two files that were each produced
// by the run they describe.
func writeDisclosure(w io.Writer) {
	fmt.Fprintf(w, "\n%s\n WHAT WAS CHANGED AFTER SEEING SCORES, AND WHY\n%s\n", rule, rule)
	fmt.Fprintf(w, "\n An earlier run of this harness, on this corpus, with this embedder, is kept at\n")
	fmt.Fprintf(w, " docs/results/field-extractors-before-changes.txt and .csv. It was measured the\n")
	fmt.Fprintf(w, " same way; nothing was deleted. Four things changed between it and this file,\n")
	fmt.Fprintf(w, " and none of them was a special case added for a pair that had failed.\n")

	fmt.Fprintf(w, "\n 1. A BUG, found by reading the extracted signatures rather than the scores.\n")
	fmt.Fprintf(w, "    The engine's intent text is `ActivityType({\"args\":\"...\",\"tool\":\"ActivityType\"})`,\n")
	fmt.Fprintf(w, "    so the activity's own name appears inside the JSON as well as in front of it.\n")
	fmt.Fprintf(w, "    The copy inside the JSON is not sentence-initial, so the generic extractor's\n")
	fmt.Fprintf(w, "    proper-noun rule read it as a deciding value on EVERY call of that activity.\n")
	fmt.Fprintf(w, "    Every intent was therefore \"recognised\" on the strength of its own type name —\n")
	fmt.Fprintf(w, "    the one token incapable of telling two calls of it apart. Extracted values are\n")
	fmt.Fprintf(w, "    now filtered against the activity name and the envelope's keys.\n")

	fmt.Fprintf(w, "\n 2. VALUES OF ONE FIELD ARE NOW SORTED instead of kept in the order they were\n")
	fmt.Fprintf(w, "    said. \"A P1 ticket on TCK-219\" and \"TCK-219 as P1\" are one ticket, and order of\n")
	fmt.Fprintf(w, "    appearance is a property of the sentence rather than of the action. The price\n")
	fmt.Fprintf(w, "    is stated rather than hidden: with the values sorted this extractor cannot tell\n")
	fmt.Fprintf(w, "    an outbound booking from its return leg, and the flight domain's\n")
	fmt.Fprintf(w, "    reversed-direction negatives below are what that costs.\n")

	fmt.Fprintf(w, "\n 3. THE NEUTRAL GATE TOLERATES A SUBSET. The same reasoning that makes an absent\n")
	fmt.Fprintf(w, "    FIELD neutral applies one level down: if one side's values for a field are\n")
	fmt.Fprintf(w, "    contained in the other's, that side said less, not something else. Two\n")
	fmt.Fprintf(w, "    disjoint value sets still close the gate. The STRICT rule is untouched, because\n")
	fmt.Fprintf(w, "    it is the rule every published hybrid figure was measured under.\n")

	fmt.Fprintf(w, "\n 4. TWO NEW DECISION MODES, both added because the first run showed the leftover\n")
	fmt.Fprintf(w, "    free text destroying the comparator rather than helping it. The gate passed\n")
	fmt.Fprintf(w, "    the great majority of genuine duplicates and the remainder threshold then\n")
	fmt.Fprintf(w, "    rejected almost all of them, for a reason that is obvious in hindsight: a\n")
	fmt.Fprintf(w, "    remainder is what is LEFT once the values are removed, so it is short, and a\n")
	fmt.Fprintf(w, "    threshold calibrated on whole sentences is wrong for it.\n")
	fmt.Fprintf(w, "      gatedecides  the remainder may only downgrade to ESCALATE, never to DISTINCT\n")
	fmt.Fprintf(w, "      gateonly     the remainder is never embedded and never consulted\n")
	fmt.Fprintf(w, "    Both are off by default in the engine, and both appear as their own arms above\n")
	fmt.Fprintf(w, "    rather than replacing anything.\n")

	fmt.Fprintf(w, "\n 5. THE INTENT TEXT BEING EMBEDDED WAS WRONG, and this is the change that moved\n")
	fmt.Fprintf(w, "    the most. The engine has two ways of turning an activity's input into the\n")
	fmt.Fprintf(w, "    text it embeds and hands to an extractor: the activity's OWN renderer, which\n")
	fmt.Fprintf(w, "    app/agent registers and cmd/engined wires into Checker.RenderIntent, giving the\n")
	fmt.Fprintf(w, "    bare sentence \"blr to del friday morning\"; and the checker's generic fallback,\n")
	fmt.Fprintf(w, "    semantic.IntentText, giving the whole envelope. The first run of this harness\n")
	fmt.Fprintf(w, "    used the FALLBACK — and so does cmd/experiment, and so does every figure in\n")
	fmt.Fprintf(w, "    docs/results/semantic-experiment-minilm.txt and -hybrid-minilm.txt. The\n")
	fmt.Fprintf(w, "    envelope is nearly identical across every call of one activity type, so most\n")
	fmt.Fprintf(w, "    of what gets embedded is constant and every cosine is pulled upwards. That run\n")
	fmt.Fprintf(w, "    is kept at docs/results/field-extractors-envelope-rendering.txt, and the arm\n")
	fmt.Fprintf(w, "    \"semantic-envelope-rendering\" above reproduces it here so the two renderings\n")
	fmt.Fprintf(w, "    sit in one table instead of in two incomparable files.\n")

	fmt.Fprintf(w, "\n 6. THE LLM ARM WAS RE-RUN FASTER, ACROSS THREE ACCOUNTS' QUOTAS. The provider's\n")
	fmt.Fprintf(w, "    free tier limits TOKENS per minute — 8,000, against a measured 634 tokens per\n")
	fmt.Fprintf(w, "    extraction call, so about twelve calls a minute per key. Paced at twenty-two\n")
	fmt.Fprintf(w, "    the arm spent its time in rate-limit backoff and would have needed thirty\n")
	fmt.Fprintf(w, "    hours. app/agent/llm/keyring.go now rotates every GROQ_API_KEY* in the\n")
	fmt.Fprintf(w, "    environment, moving to the next key the moment one is refused and sleeping\n")
	fmt.Fprintf(w, "    only when all of them are, and the run was paced at twenty-eight calls a\n")
	fmt.Fprintf(w, "    minute — about three quarters of what three keys' token budgets allow.\n")
	fmt.Fprintf(w, "      THIS CHANGES NOTHING THAT IS MEASURED. The model, the prompt, the\n")
	fmt.Fprintf(w, "      temperature and the text are identical whichever key carries the request,\n")
	fmt.Fprintf(w, "      and an extraction does not depend on whose quota paid for it. Rate is a\n")
	fmt.Fprintf(w, "      property of the harness, not a variable under test. It is disclosed because\n")
	fmt.Fprintf(w, "      the first attempt's rate DID corrupt data — a rate-limited call was cached\n")
	fmt.Fprintf(w, "      as an empty reply and became a row claiming the extractor could not read\n")
	fmt.Fprintf(w, "      the intent. Those rows are at\n")
	fmt.Fprintf(w, "      docs/results/field-extractors-llm-ratelimited-rows.csv, kept as evidence of\n")
	fmt.Fprintf(w, "      the bug and part of no figure. An unanswered call is now never cached: it\n")
	fmt.Fprintf(w, "      becomes an ERROR row, excluded from every figure and retried on a resume.\n")

	fmt.Fprintf(w, "\n WHAT WAS NOT DONE. No per-pair special case, no vocabulary entry, no threshold\n")
	fmt.Fprintf(w, " chosen from the sweep in this file. The hand-written extractor was not touched\n")
	fmt.Fprintf(w, " at all: it is the baseline, and editing it would have destroyed the comparison.\n")

	fmt.Fprintf(w, "\n%s\n", rule)
}

func writeDiscipline(w io.Writer, prov *provenance, rows []row) {
	fmt.Fprintf(w, "\n%s\n WHAT IS MEASURED, WHAT IS ASSUMED, WHAT IS NOT MEASURED\n%s\n", rule, rule)

	fmt.Fprintf(w, "\n MEASURED\n")
	fmt.Fprintf(w, "  - Every verdict came from semantic.Checker.Check, the function the engine calls\n")
	fmt.Fprintf(w, "    on the live path, with the arm's extractor and gate rule set on it. The exact\n")
	fmt.Fprintf(w, "    baseline is exact.Key. Nothing was reimplemented, and the derivation used for\n")
	fmt.Fprintf(w, "    the sweep was checked row by row against the checker's own answer.\n")
	fmt.Fprintf(w, "  - Every similarity is a cosine between two 384-dimensional vectors from the\n")
	fmt.Fprintf(w, "    running all-MiniLM-L6-v2 service. No hash embedder anywhere in this file.\n")
	fmt.Fprintf(w, "  - The recognition, gate and model-consulted counts are per-row facts recorded\n")
	fmt.Fprintf(w, "    by the comparator, not inferred from the verdicts.\n")
	fmt.Fprintf(w, "  - The latency figures are wall-clock timings of the check path.\n")
	if prov.LLMCallsMade > 0 {
		fmt.Fprintf(w, "  - The LLM extractor's call count and wall clock are counted by the client.\n")
	}

	fmt.Fprintf(w, "\n ASSUMED\n")
	fmt.Fprintf(w, "  - THE LABELS ON TWO OF THE THREE CORPORA ARE GENERATED BY CONSTRUCTION. A pair\n")
	fmt.Fprintf(w, "    is called a duplicate because it was written as two phrasings of one record,\n")
	fmt.Fprintf(w, "    not because a person confirmed a user would consider them interchangeable.\n")
	fmt.Fprintf(w, "    Where that assumption is wrong, the accuracy here is wrong by the same amount.\n")
	fmt.Fprintf(w, "    No inter-annotator agreement exists because no annotation exists.\n")
	fmt.Fprintf(w, "  - That the designed negatives resemble the mistakes a real planner makes. They\n")
	fmt.Fprintf(w, "    were written to move one deciding field at a time, which is a modelling\n")
	fmt.Fprintf(w, "    choice; the live corpus has the real wordings but no negatives at all.\n")
	fmt.Fprintf(w, "  - That the hand-written extractor's figures on the flight corpus are inflated.\n")
	fmt.Fprintf(w, "    Its vocabulary was written by reading that corpus, which is selection on the\n")
	fmt.Fprintf(w, "    test set and cannot be undone by measuring more carefully.\n")

	fmt.Fprintf(w, "\n NOT MEASURED, AND NOT TO BE INFERRED FROM THIS FILE\n")
	fmt.Fprintf(w, "  - Whether a person agrees with any ESCALATE verdict. The blocking policy's\n")
	fmt.Fprintf(w, "    catch rate assumes a human adjudicates correctly, and nothing here asked one.\n")
	fmt.Fprintf(w, "  - End-to-end engine behaviour. The comparators are the engine's, but these\n")
	fmt.Fprintf(w, "    pairs were scored in a harness with an in-memory candidate store, not against\n")
	fmt.Fprintf(w, "    PostgreSQL on the cluster.\n")
	fmt.Fprintf(w, "  - Any other embedding model, and for the LLM arm any other extraction model,\n")
	fmt.Fprintf(w, "    temperature or prompt. One of each.\n")
	fmt.Fprintf(w, "  - The determinism of the LLM arm across runs. It is a network call to a sampled\n")
	fmt.Fprintf(w, "    model at temperature 0, cached here so the run is reproducible from the cache;\n")
	fmt.Fprintf(w, "    that is NOT evidence that two cold runs would extract identically. What makes\n")
	fmt.Fprintf(w, "    it survivable in the engine is that the VERDICT is journaled once and replay\n")
	fmt.Fprintf(w, "    reads it back (internal/workflow/history.go indexes EvDedupEvaluated;\n")
	fmt.Fprintf(w, "    internal/runtime/dedup.go refuses to re-run a check that has a recorded\n")
	fmt.Fprintf(w, "    answer), so two nodes cannot reach two different verdicts for one history.\n")
	fmt.Fprintf(w, "    The journaled EXPLANATION is then a snapshot that cannot be re-derived, which\n")
	fmt.Fprintf(w, "    the deterministic extractors' explanations can.\n")

	fmt.Fprintf(w, "\n%s\n %d rows, recomputed from %s. Nothing in this file was carried over from\n",
		rule, len(rows), prov.CSVPath)
	fmt.Fprintf(w, " another run.\n%s\n", rule)
}

// ---------------------------------------------------------------------------
// Formatting helpers.
// ---------------------------------------------------------------------------

func fmtF(v float64, prec int) string {
	if math.IsNaN(v) {
		return "undef"
	}
	return fmt.Sprintf("%.*f", prec, v)
}

// fmtP prints a p-value, never as 0.000: an exact test's p is small, not zero,
// and printing zero invites a claim nobody measured.
func fmtP(p float64) string {
	switch {
	case math.IsNaN(p):
		return "n/a"
	case p < 0.0001:
		return "<0.0001"
	default:
		return fmt.Sprintf("%.4f", p)
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// wrap breaks a sentence across lines with a hanging indent, so a long note
// stays inside eighty columns.
func wrap(s string, width, indent int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, word := range words {
		if line > 0 && line+1+len(word) > width {
			b.WriteString("\n" + strings.Repeat(" ", indent))
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
