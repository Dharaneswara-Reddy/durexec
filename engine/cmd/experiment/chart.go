package main

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// The precision/recall curve, drawn as inline SVG.
// ---------------------------------------------------------------------------
//
// It is hand-built SVG rather than a charting library because the report has
// to be one file that opens from a filesystem with no network: a CDN script
// tag would make the figure disappear the first time someone opened it on a
// train, and a bundled library would be an order of magnitude more bytes than
// the picture it draws.
//
// Colours are CSS custom properties, not literal hex, so the same markup
// serves the light and dark stylesheets.

// Plot geometry, in the SVG user units of the viewBox.
const (
	plotW      = 760.0
	plotH      = 600.0
	plotLeft   = 72.0
	plotRight  = 24.0
	plotTop    = 26.0
	plotBottom = 70.0
)

var (
	plotInnerW = plotW - plotLeft - plotRight
	plotInnerH = plotH - plotTop - plotBottom
)

// curvePoint is one arm placed on the precision/recall plane.
type curvePoint struct {
	Tau       float64
	Recall    float64
	Precision float64
	F1        float64
	Label     string
	// Plotted is false for an arm that predicted no duplicates at all.
	//
	// Precision is undefined when nothing was predicted positive — there is no
	// set of predictions to be right or wrong about. This harness reports it
	// as 0 so a table never shows NaN, but drawing that 0 would put a point on
	// the floor of the chart and make the curve appear to collapse. It is
	// left off the line and named underneath instead.
	Plotted bool
	// Operating marks the thresholds the engine ships with.
	Operating bool
	// Landmark marks a point worth labelling directly.
	Landmark bool
}

// series is one line on the chart.
type series struct {
	Name   string
	Slot   int // which categorical colour slot, 1-based
	Points []curvePoint
}

// buildSeries turns the arm results into the chart's data.
func buildSeries(results []*armResult) (single, band series, exact *curvePoint) {
	single = series{Name: "Semantic, single threshold τ", Slot: 1}
	band = series{Name: "Semantic, escalation band [τ, τ+w)", Slot: 2}

	for _, r := range results {
		p := curvePoint{
			Tau:       r.Arm.Thresholds.TauLow,
			Recall:    r.Recall(),
			Precision: r.Precision(),
			F1:        r.F1(),
			Label:     r.Arm.Name,
			Plotted:   r.MadePositivePredictions(),
			Operating: r.Arm.OperatingPoint,
			Landmark:  r.Arm.Landmark,
		}
		switch r.Arm.Series {
		case seriesSingle:
			single.Points = append(single.Points, p)
		case seriesBand:
			band.Points = append(band.Points, p)
		case seriesExact:
			p.Label = "Exact match (baseline)"
			p.Landmark = true
			exact = &p
		}
	}
	return single, band, exact
}

// renderCurve writes the whole figure.
func renderCurve(results []*armResult) string {
	single, band, exact := buildSeries(results)

	// No xmlns attribute: SVG written inline in an HTML document takes its
	// namespace from the parser, and leaving the URL out keeps the report free
	// of every http:// string, which is what its offline test checks for.
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="curve" viewBox="0 0 %.0f %.0f" role="img" `+
		`aria-label="Precision against recall for every threshold in the sweep, `+
		`with the exact-match baseline marked.">`,
		plotW, plotH)

	writeIsoF1(&b)
	writeGrid(&b)
	writeAxes(&b)
	writeSeries(&b, band)
	writeSeries(&b, single)
	writeExact(&b, exact)
	writeDirectLabels(&b, single, band, exact)

	b.WriteString(`</svg>`)
	return b.String()
}

func px(recall float64) float64    { return plotLeft + clamp01(recall)*plotInnerW }
func py(precision float64) float64 { return plotTop + (1-clamp01(precision))*plotInnerH }

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// writeIsoF1 draws the curves along which F1 is constant.
//
// They are the standard reading aid for this plot: a point's distance from
// them says how good a trade it struck, which two separate axis readings do
// not. They are drawn first and in the gridline colour so they stay behind the
// data and recede from it.
func writeIsoF1(b *strings.Builder) {
	for _, f1 := range []float64{0.25, 0.50, 0.75} {
		var d strings.Builder
		started := false
		for i := 0; i <= 200; i++ {
			recall := float64(i) / 200
			// From F1 = 2PR/(P+R): precision is defined only where the
			// recall is high enough for this F1 to be reachable at all.
			denom := 2*recall - f1
			if denom <= 1e-9 {
				continue
			}
			precision := f1 * recall / denom
			if precision > 1.0 {
				continue
			}
			cmd := "L"
			if !started {
				cmd, started = "M", true
			}
			fmt.Fprintf(&d, "%s%.2f %.2f ", cmd, px(recall), py(precision))
		}
		if !started {
			continue
		}
		fmt.Fprintf(b, `<path class="iso" d="%s"/>`, strings.TrimSpace(d.String()))
		// One small label where the curve meets the right edge.
		if pr := f1 / (2 - f1); pr > 0 {
			fmt.Fprintf(b, `<text class="iso-label" x="%.2f" y="%.2f">F1 %.2f</text>`,
				px(1.0)-6, py(pr)-7, f1)
		}
	}
}

func writeGrid(b *strings.Builder) {
	for i := 0; i <= 5; i++ {
		v := float64(i) / 5
		fmt.Fprintf(b, `<line class="grid" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f"/>`,
			plotLeft, py(v), plotLeft+plotInnerW, py(v))
		fmt.Fprintf(b, `<line class="grid" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f"/>`,
			px(v), plotTop, px(v), plotTop+plotInnerH)
	}
}

func writeAxes(b *strings.Builder) {
	fmt.Fprintf(b, `<line class="axis" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f"/>`,
		plotLeft, plotTop+plotInnerH, plotLeft+plotInnerW, plotTop+plotInnerH)
	fmt.Fprintf(b, `<line class="axis" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f"/>`,
		plotLeft, plotTop, plotLeft, plotTop+plotInnerH)

	for i := 0; i <= 5; i++ {
		v := float64(i) / 5
		fmt.Fprintf(b, `<text class="tick tick-x" x="%.2f" y="%.2f">%s</text>`,
			px(v), plotTop+plotInnerH+22, tickLabel(v))
		fmt.Fprintf(b, `<text class="tick tick-y" x="%.2f" y="%.2f">%s</text>`,
			plotLeft-12, py(v)+4, tickLabel(v))
	}

	fmt.Fprintf(b, `<text class="axis-title axis-title-x" x="%.2f" y="%.2f">`+
		`Recall &#8212; share of real duplicates caught</text>`,
		plotLeft+plotInnerW/2, plotH-18)
	fmt.Fprintf(b, `<text class="axis-title axis-title-y" transform="translate(20 %.2f) rotate(-90)">`+
		`Precision &#8212; share of suppressions that were right</text>`,
		plotTop+plotInnerH/2)
}

func tickLabel(v float64) string {
	if v == 0 || v == 1 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func writeSeries(b *strings.Builder, s series) {
	plotted := make([]curvePoint, 0, len(s.Points))
	for _, p := range s.Points {
		if p.Plotted {
			plotted = append(plotted, p)
		}
	}
	if len(plotted) == 0 {
		return
	}

	// The line follows the sweep in threshold order, which is what makes it a
	// curve: each step along it is one click of tau. Sorting by recall instead
	// would draw a tidier line that no longer corresponds to anything.
	if len(plotted) > 1 {
		var d strings.Builder
		for i, p := range plotted {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&d, "%s%.2f %.2f ", cmd, px(p.Recall), py(p.Precision))
		}
		fmt.Fprintf(b, `<path class="line slot-%d" d="%s"/>`, s.Slot, strings.TrimSpace(d.String()))
	}

	for _, p := range plotted {
		r := 4.0
		cls := fmt.Sprintf("dot slot-%d", s.Slot)
		if p.Operating {
			r, cls = 7.0, cls+" operating"
		}
		fmt.Fprintf(b, `<circle class="%s" cx="%.2f" cy="%.2f" r="%.1f"><title>%s</title></circle>`,
			cls, px(p.Recall), py(p.Precision), r, html.EscapeString(pointTitle(p)))
	}
}

func writeExact(b *strings.Builder, p *curvePoint) {
	if p == nil || !p.Plotted {
		return
	}
	// A square, not a circle. The baseline is one fixed policy rather than a
	// point on a swept curve, and the different shape says so without needing
	// the legend.
	const half = 6.0
	fmt.Fprintf(b, `<rect class="dot slot-3 baseline" x="%.2f" y="%.2f" width="%.1f" height="%.1f">`+
		`<title>%s</title></rect>`,
		px(p.Recall)-half, py(p.Precision)-half, 2*half, 2*half,
		html.EscapeString(fmt.Sprintf("Exact match (baseline) — precision %.3f, recall %.3f, F1 %.3f",
			p.Precision, p.Recall, p.F1)))
}

func pointTitle(p curvePoint) string {
	return fmt.Sprintf("%s — precision %.3f, recall %.3f, F1 %.3f", p.Label, p.Precision, p.Recall, p.F1)
}

// writeDirectLabels labels a handful of points and no more.
//
// A number beside all sixty of them would be unreadable, and the table below
// already carries every value. What is labelled here is only what the figure
// is about: the two ends of the swept curve, the shipped operating point, and
// the baseline the whole argument is against.
func writeDirectLabels(b *strings.Builder, single, band series, exact *curvePoint) {
	type placed struct {
		p          curvePoint
		text       string
		dx, dy     float64
		anchor     string
		slotAnchor int
	}
	var labels []placed

	if exact != nil && exact.Plotted {
		labels = append(labels, placed{
			p: *exact, text: "exact match", dx: 12, dy: 5, anchor: "start", slotAnchor: 3,
		})
	}
	for _, s := range []series{single, band} {
		lo, hi, op := edgePoints(s)
		if lo != nil {
			labels = append(labels, placed{p: *lo,
				text: fmt.Sprintf("τ=%.2f", lo.Tau), dx: 10, dy: 16, anchor: "start", slotAnchor: s.Slot})
		}
		if hi != nil {
			labels = append(labels, placed{p: *hi,
				text: fmt.Sprintf("τ=%.2f", hi.Tau), dx: -10, dy: -10, anchor: "end", slotAnchor: s.Slot})
		}
		if op != nil {
			labels = append(labels, placed{p: *op,
				text: "shipped default", dx: 14, dy: -10, anchor: "start", slotAnchor: s.Slot})
		}
	}

	for _, l := range labels {
		x, y := px(l.p.Recall)+l.dx, py(l.p.Precision)+l.dy
		fmt.Fprintf(b, `<text class="point-label" x="%.2f" y="%.2f" text-anchor="%s">%s</text>`,
			x, y, l.anchor, html.EscapeString(l.text))
	}
}

// edgePoints picks the lowest and highest plotted threshold on a series, plus
// the operating point if it is on this one.
func edgePoints(s series) (lo, hi, op *curvePoint) {
	for i := range s.Points {
		p := s.Points[i]
		if p.Operating && p.Plotted {
			op = &s.Points[i]
		}
		if !p.Plotted {
			continue
		}
		if lo == nil || p.Tau < lo.Tau {
			lo = &s.Points[i]
		}
		if hi == nil || p.Tau > hi.Tau {
			hi = &s.Points[i]
		}
	}
	// With one plotted point there is nothing to label at both ends.
	if lo != nil && hi != nil && lo.Tau == hi.Tau {
		hi = nil
	}
	// Do not print a threshold label on top of the "shipped default" one.
	if op != nil {
		if lo != nil && lo.Tau == op.Tau {
			lo = nil
		}
		if hi != nil && hi.Tau == op.Tau {
			hi = nil
		}
	}
	return lo, hi, op
}

// curveIsDegenerate reports whether every plotted point sits on top of every
// other one, which is what a sweep looks like when the embedder cannot tell
// the pairs apart. The caption says so rather than leaving a reader to wonder
// where the curve went.
func curveIsDegenerate(results []*armResult) bool {
	var first *armResult
	distinct := 0
	for _, r := range results {
		if r.Arm.Kind != kindSemantic || !r.MadePositivePredictions() {
			continue
		}
		if first == nil {
			first = r
			distinct = 1
			continue
		}
		if math.Abs(r.Recall()-first.Recall()) > 1e-9 || math.Abs(r.Precision()-first.Precision()) > 1e-9 {
			distinct++
		}
	}
	return distinct <= 1
}
