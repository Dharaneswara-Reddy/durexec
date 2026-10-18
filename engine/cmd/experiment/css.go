package main

// ---------------------------------------------------------------------------
// The report's stylesheet.
// ---------------------------------------------------------------------------
//
// It is one Go string, inlined into a <style> tag by renderHTML, because the
// report has to be a single file that opens from a filesystem with no network.
// That constraint rules out a web font, so the typography is built from faces
// that ship with the machine: an old-style serif for the prose, which is what
// this document is — an argument with evidence in it — and the system
// grotesque for everything that is read as data rather than as a sentence.
//
// The colours are the validated three-slot categorical palette:
//
//	slot 1  blue   the single-threshold sweep
//	slot 2  orange the escalation band
//	slot 3  aqua   the exact-match baseline
//
// Those three were checked with the palette validator against both of this
// page's surfaces, for all three pairs rather than only the adjacent ones,
// because a scatter puts every series beside every other one. They clear the
// colour-blind separation floor (worst pair ΔE 9.2 light, 9.4 dark) and the
// normal-vision floor (24.0 light, 20.9 dark) in both modes.
//
// One result of that check is written into the markup rather than the CSS: the
// aqua slot sits below 3:1 contrast on the light surface, which obliges the
// figure to carry visible labels rather than leaning on colour. The baseline is
// therefore drawn as a square instead of a circle, is labelled "exact match" on
// the plot itself, and appears again in the table below — so nothing on this
// page is knowable by hue alone.
//
// Dark mode is a separate set of steps for the dark surface, not an inversion
// of the light ones, and is declared twice: once under the OS setting and once
// under an explicit data-theme, so a stamped theme wins either way.

const reportCSS = `
/* ---------- tokens ---------- */

:root {
  color-scheme: light;

  --paper:        #f7f5f0;   /* the page plane */
  --surface:      #fdfcfa;   /* cards, tables, the chart */
  --surface-sunk: #f1eee7;
  --ink:          #16150f;
  --ink-2:        #4b4940;
  --ink-muted:    #837f73;
  --rule:         #e0dcd0;
  --rule-strong:  #c6c1b2;

  --series-1: #2a78d6;
  --series-2: #eb6834;
  --series-3: #1baf7a;
  --series-4: #8257cc;

  --critical: #d03b3b;
  --critical-wash: #fbeceb;
  --good:     #0ca30c;
  --good-wash: #ecf6ec;
  --harm-wash: #f8e4e2;

  --serif: "Iowan Old Style", "Charter", "Palatino Linotype", Palatino,
           "Book Antiqua", Georgia, "Times New Roman", serif;
  --sans:  system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", sans-serif;
  --mono:  ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas,
           "Liberation Mono", monospace;

  --measure: 34rem;   /* the prose column */
  --gutter:  16px;
}

@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    color-scheme: dark;

    --paper:        #0f1013;
    --surface:      #16171b;
    --surface-sunk: #1d1f24;
    --ink:          #f4f2ec;
    --ink-2:        #bcb9b0;
    --ink-muted:    #8d8a82;
    --rule:         #2b2d33;
    --rule-strong:  #3d4049;

    --series-1: #3987e5;
    --series-2: #d95926;
    --series-3: #199e70;
    --series-4: #9a6fe0;

    --critical-wash: #2a1719;
    --good-wash:     #13251a;
    --harm-wash:     #32191b;
  }
}

:root[data-theme="dark"] {
  color-scheme: dark;

  --paper:        #0f1013;
  --surface:      #16171b;
  --surface-sunk: #1d1f24;
  --ink:          #f4f2ec;
  --ink-2:        #bcb9b0;
  --ink-muted:    #8d8a82;
  --rule:         #2b2d33;
  --rule-strong:  #3d4049;

  --series-1: #3987e5;
  --series-2: #d95926;
  --series-3: #199e70;
  --series-4: #9a6fe0;

  --critical-wash: #2a1719;
  --good-wash:     #13251a;
  --harm-wash:     #32191b;
}

/* ---------- page ---------- */

* { box-sizing: border-box; }

html { -webkit-text-size-adjust: 100%; }

body {
  margin: 0;
  background: var(--paper);
  color: var(--ink);
  font-family: var(--serif);
  font-size: 17px;
  line-height: 1.62;
  text-rendering: optimizeLegibility;
}

.page {
  max-width: 62rem;
  margin: 0 auto;
  padding: 56px var(--gutter) 96px;
}

.page > section { margin: 0 0 56px; }

p { margin: 0 0 1em; max-width: var(--measure); }

h1, h2 { font-family: var(--serif); font-weight: 600; letter-spacing: -0.012em; }

h2 {
  font-size: 1.5rem;
  line-height: 1.25;
  margin: 0 0 12px;
  padding-bottom: 10px;
  border-bottom: 1px solid var(--rule);
}

code {
  font-family: var(--mono);
  font-size: 0.86em;
  background: var(--surface-sunk);
  padding: 0.12em 0.36em;
  border-radius: 3px;
  word-break: break-word;
}

a { color: var(--series-1); }

/* ---------- masthead ---------- */

.masthead { margin-bottom: 36px; }

.eyebrow {
  font-family: var(--sans);
  font-size: 0.72rem;
  font-weight: 600;
  letter-spacing: 0.13em;
  text-transform: uppercase;
  color: var(--ink-muted);
  margin: 0 0 14px;
}

.masthead h1 {
  font-size: clamp(1.9rem, 1.3rem + 2.4vw, 3rem);
  line-height: 1.08;
  margin: 0 0 18px;
  max-width: 22ch;
  text-wrap: balance;
}

.standfirst {
  font-size: 1.12rem;
  line-height: 1.55;
  color: var(--ink-2);
  max-width: var(--measure);
  margin: 0;
}

/* ---------- the provenance band ----------

   This block is the reason the page is laid out the way it is. It sits above
   the headline figure, it is the widest thing on the page, and its verdict
   line is set at the same size as the headline number itself. A reader who
   takes in nothing but the two largest pieces of type on this page still
   learns what kind of run produced the number.                              */

.band {
  border: 1px solid var(--rule-strong);
  border-left-width: 6px;
  border-radius: 4px;
  padding: 24px 28px;
  background: var(--surface);
  margin-bottom: 40px;
}

.band.tone-critical {
  border-color: var(--critical);
  background: var(--critical-wash);
}

.band.tone-ok {
  border-color: var(--good);
  background: var(--good-wash);
}

.band-kicker {
  font-family: var(--sans);
  font-size: 0.72rem;
  font-weight: 700;
  letter-spacing: 0.14em;
  text-transform: uppercase;
  color: var(--ink-2);
  margin: 0 0 6px;
  max-width: none;
}

.band-verdict {
  font-family: var(--sans);
  font-size: clamp(1.5rem, 1.1rem + 1.9vw, 2.4rem);
  font-weight: 700;
  line-height: 1.12;
  letter-spacing: -0.015em;
  margin: 0 0 16px;
  max-width: none;
  text-wrap: balance;
}

/* When the run is not a real result, the verdict takes the headline's own type
   size, so the warning and the number carry exactly the same visual weight. */
.tone-critical .band-verdict {
  color: var(--critical);
  font-size: clamp(3rem, 2rem + 6vw, 5.2rem);
  line-height: 1;
  letter-spacing: -0.035em;
}
.tone-ok .band-verdict       { color: var(--good); }

.band-body { max-width: var(--measure); }
.band-body p:last-child { margin-bottom: 0; }
.band-body ul { margin: 0 0 1em; padding-left: 1.2em; }
.band-body li { margin-bottom: 0.4em; }

.band-flag { margin-bottom: 0.4em; }

/* The four facts: encoder, time, size, who labelled. */
.band-facts {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 12px 24px;
  margin: 0 0 20px;
  padding: 14px 0;
  border-top: 1px solid var(--rule-strong);
  border-bottom: 1px solid var(--rule-strong);
}
.band-facts dt {
  font-family: var(--sans);
  font-size: 0.68rem;
  font-weight: 700;
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--ink-muted);
  margin: 0 0 2px;
}
.band-facts dd {
  font-family: var(--sans);
  font-size: 0.95rem;
  font-weight: 600;
  margin: 0;
  overflow-wrap: anywhere;
}
.tone-critical .band-facts dd { color: var(--ink); }

.band-labels {
  border-top: 1px solid var(--rule-strong);
  padding-top: 14px;
  margin-top: 16px;
}

/* ---------- headline ---------- */

.headline {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 32px;
}

@media (min-width: 62rem) {
  .headline { grid-template-columns: minmax(0, 0.9fr) minmax(0, 1.1fr); align-items: start; }
}

.hero-label {
  font-family: var(--sans);
  font-size: 0.72rem;
  font-weight: 600;
  letter-spacing: 0.13em;
  text-transform: uppercase;
  color: var(--ink-muted);
  margin: 0 0 4px;
}

.hero-figure {
  font-family: var(--sans);
  font-size: clamp(3rem, 2rem + 6vw, 5.2rem);
  font-weight: 700;
  line-height: 1;
  letter-spacing: -0.035em;
  margin: 0 0 8px;
  color: var(--ink);
}

/* A provisional figure is struck through, not footnoted. */
.hero-figure.provisional {
  color: var(--ink-muted);
  text-decoration: line-through;
  text-decoration-thickness: 3px;
  text-decoration-color: var(--critical);
}

.hero-strike {
  font-family: var(--sans);
  font-size: 0.82rem;
  font-weight: 700;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--critical);
  margin: 0 0 10px;
}

.hero-sub { color: var(--ink-2); margin: 0; }

.hero-figure.withheld {
  color: var(--critical);
  font-size: clamp(2.2rem, 1.6rem + 4vw, 3.8rem);
  letter-spacing: -0.02em;
}

/* An undefined precision is an absence, not a score: set it apart from the
   numbers so it is never read as one. */
.num.undef {
  font-style: italic;
  color: var(--ink-muted);
}

.tiles {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
  gap: 1px;
  background: var(--rule);
  border: 1px solid var(--rule);
  border-radius: 4px;
  overflow: hidden;
}

.tile { background: var(--surface); padding: 16px 18px; }

.tile-label {
  font-family: var(--sans);
  font-size: 0.7rem;
  font-weight: 600;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--ink-muted);
  margin: 0 0 6px;
}

.tile-value {
  font-family: var(--sans);
  font-variant-numeric: tabular-nums;
  font-size: 1.6rem;
  font-weight: 700;
  line-height: 1.05;
  letter-spacing: -0.02em;
  margin: 0 0 6px;
}

.tile-sub {
  font-size: 0.84rem;
  line-height: 1.38;
  color: var(--ink-2);
  margin: 0;
}

/* ---------- the figure ---------- */

.lede { color: var(--ink-2); }

.figure { margin: 0; }

.chart-frame {
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: 4px;
  padding: 12px 8px 4px;
  margin-bottom: 12px;
}

.curve { display: block; width: 100%; height: auto; }

.legend {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 22px;
  font-family: var(--sans);
  font-size: 0.82rem;
  color: var(--ink-2);
  margin-bottom: 14px;
}

.legend-item { display: inline-flex; align-items: center; gap: 8px; }

.swatch {
  width: 12px;
  height: 12px;
  display: inline-block;
  flex: none;
  background: var(--ink-muted);
}

.shape-circle { border-radius: 50%; }
.shape-square { border-radius: 2px; }

.swatch.slot-1 { background: var(--series-1); }
.swatch.slot-2 { background: var(--series-2); }
.swatch.slot-3 { background: var(--series-3); }
.swatch.slot-4 { background: var(--series-4); }

figcaption {
  font-size: 0.9rem;
  line-height: 1.5;
  color: var(--ink-2);
  border-top: 1px solid var(--rule);
  padding-top: 12px;
}

figcaption p { max-width: var(--measure); }
figcaption p:last-child { margin-bottom: 0; }

.caption-flag { color: var(--ink); }

/* ---------- SVG chart internals ----------

   The marks are thin and the chrome recedes: hairline grid, a slightly
   stronger axis, and iso-F1 arcs drawn dashed and behind everything so they
   read as a reading aid rather than as data. Every dot carries a 2px ring in
   the surface colour, which is what keeps two overlapping thresholds legible
   as two marks instead of one blob.                                        */

.grid { stroke: var(--rule); stroke-width: 1; }
.axis { stroke: var(--rule-strong); stroke-width: 1; }

.iso {
  fill: none;
  stroke: var(--rule-strong);
  stroke-width: 1;
  stroke-dasharray: 2 4;
}

.iso-label, .tick, .axis-title, .point-label {
  font-family: var(--sans);
  fill: var(--ink-muted);
}

.iso-label { font-size: 11px; text-anchor: end; }

.tick { font-size: 12px; font-variant-numeric: tabular-nums; }
.tick-x { text-anchor: middle; }
.tick-y { text-anchor: end; }

.axis-title {
  font-size: 12.5px;
  font-weight: 600;
  fill: var(--ink-2);
  text-anchor: middle;
}

.point-label {
  font-size: 12px;
  font-weight: 600;
  fill: var(--ink-2);
  paint-order: stroke;
  stroke: var(--surface);
  stroke-width: 3px;
  stroke-linejoin: round;
}

.line {
  fill: none;
  stroke-width: 2;
  stroke-linejoin: round;
  stroke-linecap: round;
}

.dot { stroke: var(--surface); stroke-width: 2; }

.line.slot-1 { stroke: var(--series-1); }
.line.slot-2 { stroke: var(--series-2); }
.line.slot-3 { stroke: var(--series-3); }
.line.slot-4 { stroke: var(--series-4); }

.dot.slot-1 { fill: var(--series-1); }
.dot.slot-2 { fill: var(--series-2); }
.dot.slot-3 { fill: var(--series-3); }
.dot.slot-4 { fill: var(--series-4); }

/* The shipped operating point is ringed in ink as well as enlarged, so it is
   the one mark that is identifiable with the colour taken away. */
.dot.operating { stroke: var(--ink); stroke-width: 2.5; }

/* ---------- key/value table ---------- */

.kv {
  width: 100%;
  border-collapse: collapse;
  font-family: var(--sans);
  font-size: 0.88rem;
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: 4px;
}

.kv th, .kv td {
  text-align: left;
  vertical-align: top;
  padding: 9px 14px;
  border-bottom: 1px solid var(--rule);
}

.kv tr:last-child th, .kv tr:last-child td { border-bottom: 0; }

.kv th {
  font-weight: 600;
  color: var(--ink-2);
  white-space: nowrap;
  width: 15.5rem;
}

.kv td { color: var(--ink); word-break: break-word; }

.note {
  font-size: 0.88rem;
  color: var(--ink-2);
  margin-top: 12px;
}

.inline-warning {
  font-family: var(--sans);
  font-size: 0.88rem;
  font-weight: 600;
  color: var(--critical);
  background: var(--critical-wash);
  border-left: 4px solid var(--critical);
  padding: 10px 14px;
  border-radius: 0 3px 3px 0;
  max-width: none;
}

/* ---------- the data table ---------- */

.table-scroll {
  overflow-x: auto;
  border: 1px solid var(--rule);
  border-radius: 4px;
  background: var(--surface);
}

table.data {
  width: 100%;
  border-collapse: collapse;
  font-family: var(--sans);
  font-size: 0.83rem;
  font-variant-numeric: tabular-nums;
}

table.data thead th {
  position: sticky;
  top: 0;
  background: var(--surface-sunk);
  text-align: left;
  font-weight: 600;
  font-size: 0.72rem;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--ink-2);
  padding: 9px 12px;
  border-bottom: 1px solid var(--rule-strong);
  white-space: nowrap;
}

table.data tbody th, table.data tbody td {
  padding: 7px 12px;
  border-bottom: 1px solid var(--rule);
  white-space: nowrap;
}

table.data tbody th {
  text-align: left;
  font-weight: 500;
  color: var(--ink);
}

table.data tbody tr:last-child th,
table.data tbody tr:last-child td { border-bottom: 0; }

table.data .num { text-align: right; }

table.data .danger { color: var(--critical); font-weight: 700; }

table.data tr.operating {
  background: var(--surface-sunk);
  box-shadow: inset 3px 0 0 var(--ink);
}

table.data tr.operating th { font-weight: 700; }

/* ---------- confusion matrices ---------- */

.cm-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(215px, 1fr));
  gap: 14px;
}

.cm {
  margin: 0;
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: 4px;
  padding: 12px 14px 10px;
}

.cm.operating { border-color: var(--ink); border-width: 2px; }

.cm figcaption {
  font-family: var(--sans);
  font-size: 0.76rem;
  font-weight: 600;
  color: var(--ink);
  border: 0;
  padding: 0 0 8px;
}

.cm table {
  width: 100%;
  border-collapse: collapse;
  font-family: var(--sans);
  font-size: 0.8rem;
  font-variant-numeric: tabular-nums;
}

.cm th {
  font-weight: 500;
  font-size: 0.68rem;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: var(--ink-muted);
  text-align: left;
  padding: 3px 6px;
  white-space: nowrap;
}

.cm thead th { text-align: right; }

.cm td {
  padding: 5px 6px;
  text-align: right;
  border: 1px solid var(--rule);
  background: var(--surface-sunk);
}

.cm .good { background: var(--good-wash); }

/* The expensive mistake: a genuinely new booking suppressed as a repeat. */
.cm .harm {
  background: var(--harm-wash);
  color: var(--critical);
  font-weight: 700;
}

.cm-foot {
  font-family: var(--sans);
  font-size: 0.72rem;
  color: var(--ink-muted);
  margin: 8px 0 0;
}

/* ---------- the fixture ---------- */

.quote-pair {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 14px;
  margin: 0 0 20px;
}

.quote-pair blockquote {
  margin: 0;
  padding: 14px 18px;
  background: var(--surface);
  border: 1px solid var(--rule);
  border-left: 4px solid var(--series-1);
  border-radius: 0 4px 4px 0;
  font-size: 1.05rem;
}

.quote-pair blockquote:last-child { border-left-color: var(--series-2); }

.quote-pair blockquote p { margin: 0; max-width: none; }

.q-label {
  font-family: var(--sans);
  font-size: 0.68rem;
  font-weight: 700;
  letter-spacing: 0.1em;
  text-transform: uppercase;
  color: var(--ink-muted);
  margin: 0 0 6px !important;
}

/* ---------- limitations ---------- */

.limits ol {
  max-width: var(--measure);
  padding-left: 1.3em;
  margin: 0;
}

.limits li { margin-bottom: 0.85em; }

/* ---------- footer ---------- */

footer {
  border-top: 1px solid var(--rule-strong);
  padding-top: 18px;
  font-family: var(--sans);
  font-size: 0.8rem;
  color: var(--ink-muted);
}

footer p { margin: 0 0 6px; max-width: none; }

.cmd code { word-break: break-all; }

/* ---------- print ----------

   The report gets printed and stapled into a supervision meeting, so the
   provenance band has to survive a black-and-white printer that drops
   background colours. It keeps a heavy rule instead.                        */

@media print {
  body { background: #fff; color: #000; font-size: 11pt; }
  .page { padding: 0; max-width: none; }
  .band { border: 3px solid #000 !important; background: #fff !important; }
  .band-verdict { color: #000 !important; }
  .table-scroll { overflow: visible; }
  table.data thead th { position: static; }
  .cm-grid { grid-template-columns: repeat(3, 1fr); }
  section { break-inside: avoid; }
}

@media (prefers-reduced-motion: reduce) {
  * { animation: none !important; transition: none !important; }
}
`
