package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The HTML report.
// ---------------------------------------------------------------------------
//
// The file is written to be opened from a filesystem, on a machine with no
// network, years from now: no script tags, no stylesheet links, no fonts, no
// images, nothing fetched. Everything the page needs is between its own two
// tags. That is a requirement of the artefact, not a preference — a figure
// that stops rendering when a CDN moves is not evidence of anything.
//
// The one rule that drives the layout: A READER MUST NOT BE ABLE TO MISTAKE A
// SMOKE TEST FOR THE RESULT. The provenance verdict is therefore set at the
// same size as the headline figure, above it, and the headline figure itself
// is struck through with the word "provisional" whenever the run was not a
// real measurement. It is not a footnote, a superscript or a grey caption.

// ProvenanceMarker heads the provenance block at the top of every report, real
// or not. Tests look for it, so it is named rather than inlined.
const ProvenanceMarker = "PROVENANCE OF THESE NUMBERS"

func writeHTMLReport(path string, results []*armResult, rows []perPairRow, prov *provenance) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(renderHTML(results, rows, prov)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func renderHTML(results []*armResult, rows []perPairRow, prov *provenance) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	b.WriteString(`<meta charset="utf-8">` + "\n")
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">` + "\n")
	fmt.Fprintf(&b, "<title>Semantic Idempotency %s</title>\n", "Evaluation")
	fmt.Fprintf(&b, `<meta name="description" content="%s">`+"\n",
		html.EscapeString("Precision and recall of semantic duplicate detection against the exact-match baseline, swept over the similarity threshold."))
	b.WriteString("<style>\n" + reportCSS + "\n</style>\n")
	b.WriteString("</head>\n<body>\n")

	b.WriteString(`<main class="page">`)
	writeMasthead(&b, prov)
	writeProvenanceBand(&b, prov)
	writeHeadline(&b, results, prov)
	writeFigure(&b, results, prov)
	writeProvenanceTable(&b, prov)
	writeArmTable(&b, results, prov)
	writeConfusionGrid(&b, results)
	writeFixtureCard(&b, rows)
	writeLimitations(&b, prov)
	writeFooter(&b, prov)
	b.WriteString(`</main>`)

	b.WriteString("\n</body>\n</html>\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Sections.
// ---------------------------------------------------------------------------

func writeMasthead(b *strings.Builder, prov *provenance) {
	b.WriteString(`<header class="masthead">`)
	b.WriteString(`<p class="eyebrow">Durable execution engine &middot; evaluation harness</p>`)
	b.WriteString(`<h1>Semantic idempotency: does meaning catch the repeats that bytes miss?</h1>`)
	fmt.Fprintf(b, `<p class="standfirst">One activity is scheduled twice after a crash, worded differently `+
		`each time. The exact-match baseline compares the two arguments byte for byte. The semantic `+
		`arms compare what they mean. Every arm below ran over the same %d labelled pairs, through `+
		`the engine's own check code.</p>`, prov.Corpus.Total)
	b.WriteString(`</header>`)
}

// writeProvenanceBand is the block the whole page is arranged around.
func writeProvenanceBand(b *strings.Builder, prov *provenance) {
	tone := "tone-ok"
	if !prov.SemanticNumbersAreReal() {
		tone = "tone-critical"
	}
	fmt.Fprintf(b, `<section class="band %s" id="provenance">`, tone)
	fmt.Fprintf(b, `<p class="band-kicker">%s</p>`, ProvenanceMarker)
	fmt.Fprintf(b, `<p class="band-verdict">%s</p>`, html.EscapeString(prov.Verdict()))
	writeBandFacts(b, prov)

	b.WriteString(`<div class="band-body">`)
	if prov.Fake {
		b.WriteString(`<p><strong>The vectors in this run were not produced by a language model.</strong> ` +
			`They came from <code>FakeEmbedder</code>, which turns a sentence into numbers by hashing it. ` +
			`A hash has no idea what words mean, so two ways of saying the same thing come out as ` +
			`unrelated vectors and score near zero. Every semantic number on this page therefore ` +
			`measures the plumbing and nothing else.</p>`)
		b.WriteString(`<p>What it <em>does</em> establish: the corpus loads, the thresholds are applied, ` +
			`the escalation band produces its third verdict, the metrics add up, and a check costs ` +
			`what the latency column says it costs.</p>`)
	} else {
		fmt.Fprintf(b, `<p>The vectors were produced by <strong>%s</strong>, a real sentence encoder, `+
			`reached at <code>%s</code>.</p>`,
			html.EscapeString(prov.Model), html.EscapeString(prov.Endpoint))
	}
	if prov.Incomplete() {
		b.WriteString(`<p class="band-flag"><strong>Part of this run did not happen.</strong></p><ul>`)
		for _, r := range prov.IncompleteReasons() {
			fmt.Fprintf(b, `<li>%s</li>`, html.EscapeString(r))
		}
		b.WriteString(`</ul>`)
	}
	fmt.Fprintf(b, `<p class="band-labels"><strong>Labels:</strong> %s. `+
		`No human read these pairs and judged them. A published precision or recall figure `+
		`requires a hand-labelled set and a stated inter-annotator agreement. `+
		`<strong>This harness measures the mechanism, not the claim.</strong></p>`,
		html.EscapeString(prov.LabelSource()))
	b.WriteString(`</div></section>`)
}

// writeBandFacts is the four answers a reader needs before any number: which
// encoder, when, over how many pairs, and who decided the right answers. They
// are laid out as a short list inside the band, not left to the table further
// down, so they cannot be scrolled past on the way to the chart.
func writeBandFacts(b *strings.Builder, prov *provenance) {
	embedder := prov.EmbedderLabel
	if prov.Fake {
		embedder = "FakeEmbedder — a hash, not a language model"
	}
	labels := "generated by construction — not labelled by a person"
	if prov.LabelsHandAssigned() {
		labels = "hand-assigned by a person"
	}
	complete := "yes"
	if prov.Incomplete() {
		complete = fmt.Sprintf("NO — %d pair(s) never measured", len(prov.FailedPairs)+len(prov.FallbackPairs))
	}
	b.WriteString(`<dl class="band-facts">`)
	for _, f := range [][2]string{
		{"Embedder", embedder},
		{"Run at", prov.StartedAt.Format("2006-01-02 15:04 MST")},
		{"Pairs", fmt.Sprintf("%d", prov.Corpus.Total)},
		{"Labels", labels},
		{"Complete", complete},
	} {
		fmt.Fprintf(b, `<div><dt>%s</dt><dd>%s</dd></div>`, html.EscapeString(f[0]), html.EscapeString(f[1]))
	}
	b.WriteString(`</dl>`)
}

func writeHeadline(b *strings.Builder, results []*armResult, prov *provenance) {
	op := operatingResult(results)
	ex := seriesResult(results, seriesExact)
	if op == nil || ex == nil {
		return
	}

	b.WriteString(`<section class="headline">`)

	b.WriteString(`<div class="hero">`)
	b.WriteString(`<p class="hero-label">Duplicates caught at the shipped thresholds</p>`)
	cls := "hero-figure"
	switch {
	case prov.Incomplete():
		// A partial run publishes no headline at all. Part of the corpus was
		// never measured, so any single number here would describe a
		// different, smaller experiment than the one the page is about.
		b.WriteString(`<p class="hero-figure withheld">withheld</p>`)
		b.WriteString(`<p class="hero-strike">incomplete run &mdash; no headline figure is published</p>`)
	case !prov.SemanticNumbersAreReal():
		fmt.Fprintf(b, `<p class="%s provisional">%.1f%%</p>`, cls, 100*op.Recall())
		b.WriteString(`<p class="hero-strike">provisional &mdash; see the provenance above</p>`)
	default:
		fmt.Fprintf(b, `<p class="%s">%.1f%%</p>`, cls, 100*op.Recall())
	}
	fmt.Fprintf(b, `<p class="hero-sub">%s, against <strong>%.1f%%</strong> for exact byte matching `+
		`on the same pairs.</p>`,
		html.EscapeString(op.Arm.Name), 100*ex.Recall())
	b.WriteString(`</div>`)

	b.WriteString(`<div class="tiles">`)
	if op.PrecisionIsDefined() {
		tile(b, "Precision", fmt.Sprintf("%.3f", op.Precision()),
			"of the suppressions were right")
	} else {
		tile(b, "Precision", "undefined",
			"this arm never said DUPLICATE, so there were no suppressions to be right about")
	}
	tile(b, "False positives, hard negatives", fmt.Sprintf("%.3f", op.HardFalsePositiveRate()),
		fmt.Sprintf("of the %d lexically close distinct pairs were wrongly suppressed", op.HardDistincts))
	tile(b, "Sent to a human", fmt.Sprintf("%.3f", op.EscalationRate()),
		"of all pairs landed in the escalation band")
	tile(b, "Per check", fmtDur(op.P50()),
		fmt.Sprintf("median, p99 %s, embeddings served from cache", fmtDur(op.P99())))
	b.WriteString(`</div>`)

	b.WriteString(`</section>`)
}

func tile(b *strings.Builder, label, value, sub string) {
	b.WriteString(`<div class="tile">`)
	fmt.Fprintf(b, `<p class="tile-label">%s</p>`, html.EscapeString(label))
	fmt.Fprintf(b, `<p class="tile-value">%s</p>`, html.EscapeString(value))
	fmt.Fprintf(b, `<p class="tile-sub">%s</p>`, html.EscapeString(sub))
	b.WriteString(`</div>`)
}

func writeFigure(b *strings.Builder, results []*armResult, prov *provenance) {
	b.WriteString(`<section class="figure-block"><h2>The precision&ndash;recall curve</h2>`)
	fmt.Fprintf(b, `<p class="lede">Each dot is one threshold. Moving right means catching more of `+
		`the real repeats; moving up means being right more often about the ones it suppresses. `+
		`The sweep runs from &tau;=%.2f to &tau;=%.2f in steps of %.2f.</p>`,
		prov.TauMin, prov.TauMax, prov.TauStep)

	b.WriteString(`<figure class="figure">`)
	b.WriteString(`<div class="chart-frame">`)
	b.WriteString(renderCurve(results))
	b.WriteString(`</div>`)

	b.WriteString(`<div class="legend">`)
	legendItem(b, 1, "circle", "Semantic, single threshold τ")
	legendItem(b, 2, "circle", fmt.Sprintf("Semantic, escalation band [τ, τ+%.2f)", prov.BandWidth))
	legendItem(b, 3, "square", "Exact match (the baseline)")
	b.WriteString(`</div>`)

	b.WriteString(`<figcaption>`)
	if curveIsDegenerate(results) {
		b.WriteString(`<p class="caption-flag"><strong>The curve has collapsed to a single point.</strong> ` +
			`Every threshold scored identically, which is what happens when the embedder cannot ` +
			`tell any two pairs apart. With a hash embedder this is the expected shape, not a bug: ` +
			`only the pairs that are byte-identical after normalisation score 1.0, and everything ` +
			`else scores near zero, so no threshold between the two changes anything.</p>`)
	}
	if n := unplottedCount(results); n > 0 {
		fmt.Fprintf(b, `<p>%d arm(s) are missing from the line because they predicted no duplicates `+
			`at all. Precision is undefined when there are no predictions to be right about; `+
			`plotting it as zero would draw a collapse that did not happen. Those arms are still `+
			`in the table below, where their precision reads &ldquo;undefined&rdquo;.</p>`, n)
	}
	b.WriteString(`<p>The faint arcs are lines of constant F1. Hovering a dot names its arm and ` +
		`its three figures; every value is also in the table below, so nothing is reachable only ` +
		`by hovering.</p>`)
	b.WriteString(`</figcaption></figure></section>`)
}

func legendItem(b *strings.Builder, slot int, shape, label string) {
	fmt.Fprintf(b, `<span class="legend-item"><span class="swatch slot-%d shape-%s"></span>%s</span>`,
		slot, shape, html.EscapeString(label))
}

func writeProvenanceTable(b *strings.Builder, prov *provenance) {
	b.WriteString(`<section><h2>Where these numbers came from</h2>`)
	b.WriteString(`<table class="kv"><tbody>`)

	embedder := prov.EmbedderLabel
	if prov.Fake {
		embedder += "  — NOT A REAL ENCODER"
	}
	kv(b, "Embedder", embedder)
	kv(b, "Model name", prov.Model)
	if prov.Endpoint != "" {
		kv(b, "Endpoint", prov.Endpoint)
	}
	kv(b, "Vector width", fmt.Sprintf("%d", prov.Dim))
	kv(b, "Run started", prov.StartedAt.Format(time.RFC3339))
	kv(b, "Wall time", prov.Duration().Round(time.Millisecond).String())
	kv(b, "Pairs", fmt.Sprintf("%d (%d labelled DUPLICATE, %d labelled DISTINCT, %d of those hard)",
		prov.Corpus.Total, prov.Corpus.Duplicates, prov.Corpus.Distincts, prov.Corpus.HardDistincts))
	kv(b, "Pairs by category", categoryLine(prov.Corpus))
	kv(b, "Label source", prov.LabelSource())
	kv(b, "Arms", fmt.Sprintf("%d (τ from %.2f to %.2f step %.2f, band width %.2f)",
		prov.Arms, prov.TauMin, prov.TauMax, prov.TauStep, prov.BandWidth))
	kv(b, "Repetitions", fmt.Sprintf("%d per pair, for the latency percentiles", prov.Repeat))
	kv(b, "Distinct intent texts embedded", fmt.Sprintf("%d", prov.UniqueIntents))

	if prov.CacheDir != "" {
		c := prov.Cache
		kv(b, "Embedding cache", fmt.Sprintf("%s — %d hits, %d misses, %d written, %d unusable entries",
			prov.CacheDir, c.Hits, c.Misses, c.Writes, c.Errors))
	} else {
		kv(b, "Embedding cache", "disabled")
	}
	if n := len(prov.EmbedLatencies); n > 0 && prov.Fake {
		kv(b, "One model round-trip", fmt.Sprintf("not measured — the %d uncached call(s) went to "+
			"FakeEmbedder, a local hash (p50 %s), not a model", n, fmtDur(prov.EmbedP50())))
	} else if n > 0 {
		kv(b, "One model round-trip", fmt.Sprintf("p50 %s, p99 %s, over the %d call(s) the cache could not serve",
			fmtDur(prov.EmbedP50()), fmtDur(prov.EmbedP99()), n))
	} else {
		kv(b, "One model round-trip", "not measured — every embedding came from the cache")
	}
	kv(b, "Embedding retries spent", fmt.Sprintf("%d (up to %d attempts per call)", prov.EmbedRetries, prov.Retries))

	complete := "yes — every pair was embedded"
	if prov.Incomplete() {
		complete = fmt.Sprintf("NO — %d pair(s) failed outright, %d fell back to the degraded path",
			len(prov.FailedPairs), len(prov.FallbackPairs))
	}
	kv(b, "Run complete", complete)
	kv(b, "Command", prov.CommandLine)
	b.WriteString(`</tbody></table>`)

	if len(prov.FailedPairs) > 0 {
		b.WriteString(`<p class="note"><strong>Pairs that could not be embedded:</strong> <code>` +
			html.EscapeString(strings.Join(prov.FailedPairs, ", ")) + `</code></p>`)
	}
	b.WriteString(`</section>`)
}

func categoryLine(s CorpusSummary) string {
	keys := make([]string, 0, len(s.ByCategory))
	for k := range s.ByCategory {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, s.ByCategory[k]))
	}
	return strings.Join(parts, ", ")
}

func kv(b *strings.Builder, k, v string) {
	fmt.Fprintf(b, `<tr><th scope="row">%s</th><td>%s</td></tr>`,
		html.EscapeString(k), html.EscapeString(v))
}

func writeArmTable(b *strings.Builder, results []*armResult, prov *provenance) {
	b.WriteString(`<section><h2>Every arm</h2>`)
	b.WriteString(`<p class="lede">The full sweep, including the confusion matrix for each arm. ` +
		`An <em>escalated</em> verdict is counted as "not caught" for recall and never as a false ` +
		`positive for precision, because the engine stops and asks a person rather than suppressing ` +
		`anything. The starred row is the threshold pair the engine ships with.</p>`)

	if !prov.SemanticNumbersAreReal() {
		b.WriteString(`<p class="inline-warning">Every semantic row in this table is provisional. ` +
			`See the provenance block at the top of the page.</p>`)
	}

	b.WriteString(`<div class="table-scroll"><table class="data"><thead><tr>`)
	for _, h := range []string{"arm", "series", "TP", "FP", "TN", "FN", "escalated",
		"precision", "recall", "F1", "FPR", "FPR (hard)", "p50", "p99"} {
		fmt.Fprintf(b, `<th scope="col">%s</th>`, html.EscapeString(h))
	}
	b.WriteString(`</tr></thead><tbody>`)

	for _, r := range results {
		cls := ""
		if r.Arm.OperatingPoint {
			cls = ` class="operating"`
		}
		name := r.Arm.Name
		if r.Arm.OperatingPoint {
			name = "★ " + name
		}
		fmt.Fprintf(b, `<tr%s><th scope="row">%s</th><td>%s</td>`,
			cls, html.EscapeString(name), html.EscapeString(r.Arm.Series))
		fmt.Fprintf(b, `<td class="num">%d</td><td class="num %s">%d</td><td class="num">%d</td><td class="num">%d</td><td class="num">%d</td>`,
			r.TruePositives, dangerClass(r.FalsePositives), r.FalsePositives,
			r.TrueNegatives, r.FalseNegatives,
			r.EscalatedDuplicates+r.EscalatedDistincts)
		b.WriteString(precisionCell(r))
		fmt.Fprintf(b, `<td class="num">%.3f</td><td class="num">%.3f</td>`, r.Recall(), r.F1())
		fmt.Fprintf(b, `<td class="num">%.3f</td><td class="num">%.3f</td>`,
			r.FalsePositiveRate(), r.HardFalsePositiveRate())
		fmt.Fprintf(b, `<td class="num">%s</td><td class="num">%s</td></tr>`,
			html.EscapeString(fmtDur(r.P50())), html.EscapeString(fmtDur(r.P99())))
	}
	b.WriteString(`</tbody></table></div></section>`)
}

// precisionCell renders precision, or an explicit "undefined" when the arm
// never predicted a duplicate. Printing 0.000 there would claim the arm was
// always wrong, when in fact it never answered.
func precisionCell(r *armResult) string {
	if !r.PrecisionIsDefined() {
		return `<td class="num undef" title="No positive predictions, so precision is 0/0">undefined</td>`
	}
	return fmt.Sprintf(`<td class="num">%.3f</td>`, r.Precision())
}

func dangerClass(fp int) string {
	if fp > 0 {
		return "danger"
	}
	return ""
}

// writeConfusionGrid draws one small matrix per arm.
//
// The table above holds the same four numbers, but a 2x2 laid out as a 2x2 is
// read differently: the eye goes straight to the off-diagonal, which is where
// the errors are, and the top-right cell is the one that costs a user money.
func writeConfusionGrid(b *strings.Builder, results []*armResult) {
	b.WriteString(`<section><h2>Confusion matrix, arm by arm</h2>`)
	b.WriteString(`<p class="lede">Rows are what the pair really was; columns are what the arm ` +
		`decided. The shaded cell is the expensive mistake: a genuinely new booking suppressed as ` +
		`though it were a repeat, which means a seat the user asked for was never bought.</p>`)
	b.WriteString(`<div class="cm-grid">`)
	for _, r := range results {
		cls := "cm"
		if r.Arm.OperatingPoint {
			cls += " operating"
		}
		fmt.Fprintf(b, `<figure class="%s">`, cls)
		fmt.Fprintf(b, `<figcaption>%s</figcaption>`, html.EscapeString(r.Arm.Name))
		b.WriteString(`<table><thead><tr><td></td>` +
			`<th scope="col">said dup.</th><th scope="col">did not</th></tr></thead><tbody>`)
		fmt.Fprintf(b, `<tr><th scope="row">was dup.</th><td class="num good">%d</td><td class="num">%d</td></tr>`,
			r.TruePositives, r.FalseNegatives)
		fmt.Fprintf(b, `<tr><th scope="row">was new</th><td class="num harm">%d</td><td class="num good">%d</td></tr>`,
			r.FalsePositives, r.TrueNegatives)
		b.WriteString(`</tbody></table>`)
		fmt.Fprintf(b, `<p class="cm-foot">%d escalated to a human`,
			r.EscalatedDuplicates+r.EscalatedDistincts)
		if !r.PrecisionIsDefined() {
			b.WriteString(` &middot; precision undefined`)
		}
		b.WriteString(`</p>`)
		b.WriteString(`</figure>`)
	}
	b.WriteString(`</div></section>`)
}

// writeFixtureCard singles out the one scenario the project is built around.
func writeFixtureCard(b *strings.Builder, rows []perPairRow) {
	var a, bSide string
	type verdictRow struct{ arm, verdict, mark string }
	var vs []verdictRow
	for _, r := range rows {
		if r.Pair.Category != "fixture" || !r.Arm.Landmark {
			continue
		}
		a, bSide = r.Pair.A, r.Pair.B
		mark := "missed — a second seat would be booked"
		switch r.Outcome.Verdict {
		case "DUPLICATE":
			mark = "caught — the repeat is suppressed"
		case "ESCALATE":
			mark = "escalated — a person is asked first"
		}
		vs = append(vs, verdictRow{r.ArmName, string(r.Outcome.Verdict), mark})
	}
	if len(vs) == 0 {
		return
	}

	b.WriteString(`<section><h2>The double-booking fixture</h2>`)
	b.WriteString(`<p class="lede">This is the scenario the whole engine is built around. A ` +
		`workflow books a flight, the process is killed, and the recovered planner asks for the ` +
		`same flight again in different words. Byte comparison sees two different requests and ` +
		`books a second seat.</p>`)
	b.WriteString(`<div class="quote-pair">`)
	fmt.Fprintf(b, `<blockquote><p class="q-label">before the crash</p><p>%s</p></blockquote>`,
		html.EscapeString(a))
	fmt.Fprintf(b, `<blockquote><p class="q-label">after recovery</p><p>%s</p></blockquote>`,
		html.EscapeString(bSide))
	b.WriteString(`</div>`)
	b.WriteString(`<table class="data"><thead><tr><th scope="col">arm</th>` +
		`<th scope="col">verdict</th><th scope="col">what that means</th></tr></thead><tbody>`)
	for _, v := range vs {
		fmt.Fprintf(b, `<tr><th scope="row">%s</th><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(v.arm), html.EscapeString(v.verdict), html.EscapeString(v.mark))
	}
	b.WriteString(`</tbody></table>`)
	b.WriteString(`<p class="note">Only the landmark thresholds are listed here; the full sweep is ` +
		`in the per-pair CSV.</p></section>`)
}

func writeLimitations(b *strings.Builder, prov *provenance) {
	b.WriteString(`<section class="limits"><h2>What this page does not show</h2>`)
	b.WriteString(`<ol>`)

	fmt.Fprintf(b, `<li><strong>Nobody labelled these pairs.</strong> They were %s: two phrasings `+
		`written from one itinerary record are called the same request, two phrasings from `+
		`different records are called different requests. Where that assumption is wrong, the `+
		`accuracy figures are wrong by the same amount and in the same direction. A result fit to `+
		`publish needs a person to read real retry pairs, decide each one, and a second person to `+
		`do it independently so the agreement between them can be quoted.</li>`,
		html.EscapeString(prov.LabelSource()))

	if prov.Fake {
		b.WriteString(`<li><strong>No language model was involved.</strong> The vectors were hashes. ` +
			`Re-run against a real sentence encoder before reading any semantic figure as a ` +
			`measurement.</li>`)
	}
	if prov.Incomplete() {
		b.WriteString(`<li><strong>Part of the run did not complete.</strong> Some embeddings never ` +
			`arrived, and a similarity that was never computed is not a similarity of zero.</li>`)
	}

	b.WriteString(`<li><strong>The paraphrases are one author's guess.</strong> Real retry pairs come ` +
		`out of a real planner recovering from a real crash. How such a planner actually rewords a ` +
		`request is an empirical fact this corpus does not have.</li>`)

	fmt.Fprintf(b, `<li><strong>One domain, %d pairs.</strong> Flight booking, and nothing else. `+
		`Nothing here supports a claim about semantic idempotency in general.</li>`, prov.Corpus.Total)

	b.WriteString(`<li><strong>Picking the best threshold off this chart is cheating.</strong> ` +
		`Choosing &tau; by the F1 it scored on the very pairs it is being judged on is selection on ` +
		`the test set. A threshold to ship has to be chosen on a held-out split.</li>`)

	b.WriteString(`<li><strong>The latency column excludes the model.</strong> Embeddings are cached ` +
		`on disk, so after the first pass a check reads a vector from a local file instead of ` +
		`calling the encoder. The cost of one real encoder call is reported separately above.</li>`)

	b.WriteString(`</ol></section>`)
}

func writeFooter(b *strings.Builder, prov *provenance) {
	b.WriteString(`<footer>`)
	fmt.Fprintf(b, `<p>Generated by <code>cmd/experiment</code> at %s.</p>`,
		html.EscapeString(prov.FinishedAt.Format(time.RFC3339)))
	fmt.Fprintf(b, `<p class="cmd"><code>%s</code></p>`, html.EscapeString(prov.CommandLine))
	fmt.Fprintf(b, `<p>%s</p>`, html.EscapeString(prov.Verdict()))
	b.WriteString(`</footer>`)
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

func operatingResult(results []*armResult) *armResult {
	for _, r := range results {
		if r.Arm.OperatingPoint {
			return r
		}
	}
	return nil
}

func seriesResult(results []*armResult, s string) *armResult {
	for _, r := range results {
		if r.Arm.Series == s {
			return r
		}
	}
	return nil
}

func unplottedCount(results []*armResult) int {
	n := 0
	for _, r := range results {
		if !r.MadePositivePredictions() {
			n++
		}
	}
	return n
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
