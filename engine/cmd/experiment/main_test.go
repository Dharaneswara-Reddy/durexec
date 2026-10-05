package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/app/agent"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

// ---------------------------------------------------------------------------
// The corpus.
// ---------------------------------------------------------------------------

func TestCorpusIsWellFormed(t *testing.T) {
	pairs, err := Corpus()
	if err != nil {
		t.Fatalf("Corpus() failed: %v", err)
	}
	s := Summarize(pairs)
	t.Logf("corpus: %d pairs, %d duplicates, %d distincts, categories %v",
		s.Total, s.Duplicates, s.Distincts, s.ByCategory)

	if s.Total < 120 {
		t.Fatalf("corpus holds %d pairs, the brief calls for at least 120", s.Total)
	}
	// "Roughly balanced": neither label may be more than two thirds of the set,
	// or the headline accuracy figures would be dominated by one class.
	for _, c := range []struct {
		name string
		n    int
	}{{"duplicates", s.Duplicates}, {"distincts", s.Distincts}} {
		if share := float64(c.n) / float64(s.Total); share < 0.33 || share > 0.67 {
			t.Fatalf("%s are %.1f%% of the corpus, which is not roughly balanced", c.name, 100*share)
		}
	}
	if s.ByCategory["fixture"] != 1 {
		t.Fatalf("expected exactly one fixture pair, found %d", s.ByCategory["fixture"])
	}
	if s.ByCategory["paraphrase"] == 0 {
		t.Fatal("the corpus has no paraphrase pairs, so nothing in it needs semantic matching")
	}
}

func TestCorpusIsDeterministic(t *testing.T) {
	a, err := Corpus()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Corpus()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("two calls returned %d and %d pairs", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("pair %d differs between calls: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestFixturePairIsTheRealScenario ties the corpus to app/agent. If the fixture
// constants change and the corpus does not, the harness must not quietly keep
// measuring the old scenario.
func TestFixturePairIsTheRealScenario(t *testing.T) {
	pairs, err := Corpus()
	if err != nil {
		t.Fatal(err)
	}
	var fixture *Pair
	for i := range pairs {
		if pairs[i].Category == "fixture" {
			fixture = &pairs[i]
			break
		}
	}
	if fixture == nil {
		t.Fatal("no fixture pair in the corpus")
	}
	if fixture.A != agent.IntentBeforeCrash || fixture.B != agent.IntentAfterCrash {
		t.Fatalf("the fixture pair is %q / %q, want the DoubleBooking constants %q / %q",
			fixture.A, fixture.B, agent.IntentBeforeCrash, agent.IntentAfterCrash)
	}
	if fixture.Truth != Duplicate {
		t.Fatal("the fixture pair must be labelled DUPLICATE; it is the double booking")
	}
	// The scenario is only interesting because normalisation cannot save it.
	if agent.CanonicalIntent(fixture.A) == agent.CanonicalIntent(fixture.B) {
		t.Fatal("the fixture's two sides are equal after canonicalisation, so the scenario " +
			"no longer needs an embedding at all")
	}
}

// TestSurfacePairsAreCaughtByEveryArm is the harness's own self-check. Surface
// pairs collapse to the same string under canonicalisation, so every arm must
// call them duplicates with a similarity of exactly 1.0. If this fails, the
// wiring is broken and no other number in the run means anything.
func TestSurfacePairsAreCaughtByEveryArm(t *testing.T) {
	pairs, err := Corpus()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{activityType: agent.ActivityBookFlight, repeat: 1, dim: 64}
	emb := semantic.NewFakeEmbedder(cfg.dim)
	arms := []arm{
		{Name: "exact", Kind: kindExact},
		{Name: "tau=0.95", Kind: kindSemantic, Thresholds: core.Thresholds{TauLow: 0.95, TauHigh: 0.95}},
		{Name: "escalate", Kind: kindSemantic, Thresholds: core.DefaultThresholds()},
	}

	for _, a := range arms {
		for _, p := range pairs {
			if p.Category != "surface" {
				continue
			}
			out, err := checkPair(context.Background(), cfg, a, p, emb)
			if err != nil {
				t.Fatalf("arm %s, pair %s: %v", a.Name, p.ID, err)
			}
			if out.Verdict != core.VerdictDuplicate {
				t.Fatalf("arm %s called surface pair %s %s; every arm must catch these",
					a.Name, p.ID, out.Verdict)
			}
			if a.Kind == kindSemantic && math.Abs(out.Similarity-1.0) > 1e-9 {
				t.Fatalf("arm %s scored surface pair %s at %.6f, want exactly 1.0",
					a.Name, p.ID, out.Similarity)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Threshold wiring.
// ---------------------------------------------------------------------------

// angleEmbedder returns one of two unit vectors in a plane, chosen so that the
// cosine between them is exactly a value the test names. It exists to drive the
// arms at controlled similarities, which a hash embedder cannot do.
type angleEmbedder struct {
	sim float64
}

func (e angleEmbedder) Model() string { return "test-angle" }
func (e angleEmbedder) Dim() int      { return 2 }

func (e angleEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	// The stored side is embedded first and gets the reference direction; every
	// other text is rotated away from it by the angle whose cosine is e.sim.
	if isReference(text) {
		return []float32{1, 0}, nil
	}
	theta := math.Acos(e.sim)
	return []float32{float32(math.Cos(theta)), float32(math.Sin(theta))}, nil
}

// reference is set by the test just before the stored side is embedded.
var referenceText string

func isReference(text string) bool { return text == referenceText }

var _ core.Embedder = angleEmbedder{}

// TestArmsApplyTheirThresholds proves each arm classifies by the thresholds it
// was built with, and in particular that the escalation band produces a third
// verdict where a single threshold produces only two.
func TestArmsApplyTheirThresholds(t *testing.T) {
	cfg := config{activityType: agent.ActivityBookFlight, repeat: 1}
	pair := Pair{ID: "synthetic", Category: "paraphrase", A: "alpha one", B: "beta two", Truth: Duplicate}

	// The stored side's intent text, so angleEmbedder can recognise it.
	inputA, err := toolCallJSON(cfg.activityType, pair.A)
	if err != nil {
		t.Fatal(err)
	}
	refText, err := semantic.IntentText(cfg.activityType, inputA)
	if err != nil {
		t.Fatal(err)
	}
	referenceText = refText

	tests := []struct {
		name string
		sim  float64
		arm  arm
		want core.DedupVerdict
	}{
		// Scores are kept a little away from the exact threshold values. The
		// controlled embedder builds its vectors from float32 components, so a
		// score aimed at exactly 0.90 lands a few ulps either side of it. The
		// behaviour AT a threshold is pinned separately, and exactly, in
		// TestClassifyBoundariesAreInclusiveAtTauHigh.
		{"tau=0.85, score above", 0.90, tauArm(0.85), core.VerdictDuplicate},
		{"tau=0.85, score just above", 0.8505, tauArm(0.85), core.VerdictDuplicate},
		{"tau=0.85, score below", 0.84, tauArm(0.85), core.VerdictDistinct},
		{"tau=0.90, score between the two taus", 0.87, tauArm(0.90), core.VerdictDistinct},
		{"tau=0.95, score below", 0.94, tauArm(0.95), core.VerdictDistinct},
		{"tau=0.95, score above", 0.96, tauArm(0.95), core.VerdictDuplicate},

		// The same 0.87 that a single threshold of 0.90 calls DISTINCT lands in
		// the band and is escalated instead. That difference is the whole point
		// of the third verdict.
		{"escalation band, inside", 0.87, escalateArm(0.85, 0.90), core.VerdictEscalate},
		{"escalation band, just above the lower edge", 0.8505, escalateArm(0.85, 0.90), core.VerdictEscalate},
		{"escalation band, just above the upper edge", 0.9005, escalateArm(0.85, 0.90), core.VerdictDuplicate},
		{"escalation band, below", 0.10, escalateArm(0.85, 0.90), core.VerdictDistinct},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := checkPair(context.Background(), cfg, tc.arm, pair, angleEmbedder{sim: tc.sim})
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(out.Similarity-tc.sim) > 1e-4 {
				t.Fatalf("the harness measured similarity %.6f, want %.6f — the controlled "+
					"embedder is not being used as intended", out.Similarity, tc.sim)
			}
			if out.Verdict != tc.want {
				t.Fatalf("similarity %.2f under %s gave %s, want %s",
					tc.sim, tc.arm.Name, out.Verdict, tc.want)
			}
		})
	}
}

// TestClassifyBoundariesAreInclusiveAtTauHigh pins the exact edge behaviour
// with no floating-point embedder in the way: a score equal to TauHigh is a
// duplicate, and a score equal to TauLow is escalated, not distinct.
func TestClassifyBoundariesAreInclusiveAtTauHigh(t *testing.T) {
	band := core.Thresholds{TauLow: 0.85, TauHigh: 0.90}
	single := core.Thresholds{TauLow: 0.90, TauHigh: 0.90}

	tests := []struct {
		name string
		th   core.Thresholds
		sim  float64
		want core.DedupVerdict
	}{
		{"band: exactly TauHigh", band, 0.90, core.VerdictDuplicate},
		{"band: exactly TauLow", band, 0.85, core.VerdictEscalate},
		{"band: a hair under TauLow", band, 0.85 - 1e-12, core.VerdictDistinct},
		{"single: exactly tau", single, 0.90, core.VerdictDuplicate},
		{"single: a hair under tau", single, 0.90 - 1e-12, core.VerdictDistinct},
		{"a perfect score", band, 1.0, core.VerdictDuplicate},
		{"an orthogonal score", band, 0.0, core.VerdictDistinct},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.th.Classify(tc.sim); got != tc.want {
				t.Fatalf("Classify(%.12f) with %+v = %s, want %s", tc.sim, tc.th, got, tc.want)
			}
		})
	}
}

func tauArm(tau float64) arm {
	return arm{
		Name:       "tau",
		Kind:       kindSemantic,
		Thresholds: core.Thresholds{TauLow: tau, TauHigh: tau},
	}
}

func escalateArm(lo, hi float64) arm {
	return arm{
		Name:       "escalate",
		Kind:       kindSemantic,
		Thresholds: core.Thresholds{TauLow: lo, TauHigh: hi},
	}
}

func TestSingleThresholdArmNeverEscalates(t *testing.T) {
	if escalateArm(0.85, 0.90).Escalating() != true {
		t.Fatal("an arm with a band should report that it escalates")
	}
	if tauArm(0.90).Escalating() != false {
		t.Fatal("a single-threshold arm must not report that it escalates")
	}
}

func TestBuildArmsFromFlags(t *testing.T) {
	tests := []struct {
		name      string
		cfg       config
		wantNames []string
		wantErr   bool
	}{
		{
			name: "a three-point sweep with the shipped band on the grid",
			cfg: config{tauMin: 0.85, tauMax: 0.95, tauStep: 0.05, bandWidth: 0.05,
				tauLow: 0.85, tauHigh: 0.90},
			wantNames: []string{
				"exact",
				"semantic tau=0.85", "semantic tau=0.90", "semantic tau=0.95",
				"semantic escalate [0.85,0.90)", "semantic escalate [0.90,0.95)",
				"semantic escalate [0.95,1.00)",
			},
		},
		{
			name: "the shipped band is appended when it is off the grid",
			cfg: config{tauMin: 0.90, tauMax: 0.90, tauStep: 0.01, bandWidth: 0.05,
				tauLow: 0.85, tauHigh: 0.90},
			wantNames: []string{
				"exact", "semantic tau=0.90", "semantic escalate [0.90,0.95)",
				"semantic escalate [0.85,0.90)",
			},
		},
		{name: "a zero step is rejected", cfg: config{tauMin: 0.7, tauMax: 0.99, bandWidth: 0.05}, wantErr: true},
		{name: "a tau above 1 is rejected", cfg: config{tauMin: 0.7, tauMax: 1.5, tauStep: 0.01, bandWidth: 0.05}, wantErr: true},
		{name: "an inverted sweep is rejected", cfg: config{tauMin: 0.9, tauMax: 0.8, tauStep: 0.01, bandWidth: 0.05}, wantErr: true},
		{name: "a zero band width is rejected", cfg: config{tauMin: 0.7, tauMax: 0.99, tauStep: 0.01}, wantErr: true},
		{
			name: "an inverted band is rejected",
			cfg: config{tauMin: 0.7, tauMax: 0.99, tauStep: 0.01, bandWidth: 0.05,
				tauLow: 0.95, tauHigh: 0.90},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arms, err := buildArms(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(arms) != len(tc.wantNames) {
				t.Fatalf("built %d arms, want %d: %v", len(arms), len(tc.wantNames), armNames(arms))
			}
			for i, want := range tc.wantNames {
				if arms[i].Name != want {
					t.Fatalf("arm %d is %q, want %q", i, arms[i].Name, want)
				}
			}
		})
	}
}

// TestDefaultSweepIsThirtyExactGridPoints pins the shape the report's chart is
// built from: 0.70 to 0.99 in steps of 0.01 is thirty thresholds, each exactly
// the value it is printed as, with one exact arm and one operating point.
func TestDefaultSweepIsThirtyExactGridPoints(t *testing.T) {
	taus, err := sweepTaus(0.70, 0.99, 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if len(taus) != 30 {
		t.Fatalf("the default sweep has %d points, want 30", len(taus))
	}
	for i, tau := range taus {
		want := float64(70+i) / 100
		if tau != want {
			t.Fatalf("sweep point %d is %.17g, want exactly %.2f", i, tau, want)
		}
	}

	d := core.DefaultThresholds()
	arms, err := buildArms(config{tauMin: 0.70, tauMax: 0.99, tauStep: 0.01, bandWidth: 0.05,
		tauLow: d.TauLow, tauHigh: d.TauHigh})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	operating := 0
	for _, a := range arms {
		counts[a.Series]++
		if a.OperatingPoint {
			operating++
			if a.Thresholds != d {
				t.Fatalf("the operating point is %+v, want the shipped %+v", a.Thresholds, d)
			}
		}
	}
	if counts[seriesExact] != 1 || counts[seriesSingle] != 30 || counts[seriesBand] != 30 {
		t.Fatalf("series sizes %v, want exact=1 single=30 band=30", counts)
	}
	if operating != 1 {
		t.Fatalf("%d arms are marked as the operating point, want exactly 1", operating)
	}
}

func armNames(arms []arm) []string {
	out := make([]string, len(arms))
	for i, a := range arms {
		out[i] = a.Name
	}
	return out
}

// ---------------------------------------------------------------------------
// The metrics themselves.
// ---------------------------------------------------------------------------

// TestMetricsArithmetic pins the formulas, including the decision that an
// ESCALATE counts as "not a duplicate" rather than as a false positive.
func TestMetricsArithmetic(t *testing.T) {
	type verdictCase struct {
		truth   Label
		verdict core.DedupVerdict
		n       int
	}
	tests := []struct {
		name          string
		cases         []verdictCase
		wantTP        int
		wantFP        int
		wantTN        int
		wantFN        int
		wantEscDup    int
		wantEscDist   int
		wantPrecision float64
		wantRecall    float64
		wantF1        float64
		wantFPR       float64
	}{
		{
			name: "a perfect arm",
			cases: []verdictCase{
				{Duplicate, core.VerdictDuplicate, 10},
				{Distinct, core.VerdictDistinct, 10},
			},
			wantTP: 10, wantTN: 10,
			wantPrecision: 1, wantRecall: 1, wantF1: 1, wantFPR: 0,
		},
		{
			name: "an arm that suppresses everything",
			cases: []verdictCase{
				{Duplicate, core.VerdictDuplicate, 10},
				{Distinct, core.VerdictDuplicate, 10},
			},
			wantTP: 10, wantFP: 10,
			wantPrecision: 0.5, wantRecall: 1, wantF1: 2.0 / 3.0, wantFPR: 1,
		},
		{
			name: "an arm that suppresses nothing",
			cases: []verdictCase{
				{Duplicate, core.VerdictDistinct, 10},
				{Distinct, core.VerdictDistinct, 10},
			},
			wantFN: 10, wantTN: 10,
			wantPrecision: 0, wantRecall: 0, wantF1: 0, wantFPR: 0,
		},
		{
			name: "escalations are misses, never false positives",
			cases: []verdictCase{
				{Duplicate, core.VerdictDuplicate, 6},
				{Duplicate, core.VerdictEscalate, 4},
				{Distinct, core.VerdictEscalate, 5},
				{Distinct, core.VerdictDistinct, 5},
			},
			wantTP: 6, wantFN: 4, wantTN: 10,
			wantEscDup: 4, wantEscDist: 5,
			// Precision stays perfect because nothing distinct was suppressed;
			// recall falls to 0.6 because four duplicates needed a human.
			wantPrecision: 1, wantRecall: 0.6, wantF1: 0.75, wantFPR: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			total := 0
			for _, c := range tc.cases {
				total += c.n
			}
			r := newArmResult(arm{Name: "t", Kind: kindSemantic}, total)
			for _, c := range tc.cases {
				for i := 0; i < c.n; i++ {
					r.record(Pair{Truth: c.truth}, outcome{Verdict: c.verdict})
				}
			}

			checkInt(t, "TP", r.TruePositives, tc.wantTP)
			checkInt(t, "FP", r.FalsePositives, tc.wantFP)
			checkInt(t, "TN", r.TrueNegatives, tc.wantTN)
			checkInt(t, "FN", r.FalseNegatives, tc.wantFN)
			checkInt(t, "escalated duplicates", r.EscalatedDuplicates, tc.wantEscDup)
			checkInt(t, "escalated distincts", r.EscalatedDistincts, tc.wantEscDist)

			checkFloat(t, "precision", r.Precision(), tc.wantPrecision)
			checkFloat(t, "recall", r.Recall(), tc.wantRecall)
			checkFloat(t, "F1", r.F1(), tc.wantF1)
			checkFloat(t, "false-positive rate", r.FalsePositiveRate(), tc.wantFPR)
			checkFloat(t, "duplicate-detection rate", r.DuplicateDetectionRate(), tc.wantRecall)

			// Every pair must land in exactly one confusion-matrix cell.
			if sum := r.TruePositives + r.FalsePositives + r.TrueNegatives + r.FalseNegatives; sum != total {
				t.Fatalf("the confusion matrix sums to %d, but %d pairs were recorded", sum, total)
			}
		})
	}
}

func TestMetricsOnAnEmptyArmAreZeroNotNaN(t *testing.T) {
	r := newArmResult(arm{Name: "empty"}, 0)
	for name, got := range map[string]float64{
		"precision":                r.Precision(),
		"recall":                   r.Recall(),
		"f1":                       r.F1(),
		"false-positive rate":      r.FalsePositiveRate(),
		"duplicate-detection rate": r.DuplicateDetectionRate(),
		"escalation rate":          r.EscalationRate(),
	} {
		if math.IsNaN(got) {
			t.Fatalf("%s is NaN on an empty arm; it must be 0 so an absent measurement "+
				"cannot be mistaken for one", name)
		}
		if got != 0 {
			t.Fatalf("%s = %v on an empty arm, want 0", name, got)
		}
	}
	if r.P50() != 0 || r.P99() != 0 || r.Mean() != 0 {
		t.Fatal("latency percentiles on an empty arm must be zero")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	tests := []struct {
		name string
		in   []time.Duration
		p    float64
		want time.Duration
	}{
		{"empty", nil, 0.5, 0},
		{"single sample", []time.Duration{7}, 0.99, 7},
		{"median of ten", durations(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 0.50, 5},
		// Nearest-rank: rank = ceil(0.99 * 100) = 99, and the 99th of the
		// sorted values 0..99 is 98.
		{"p99 of a hundred", rangeDurations(100), 0.99, 98},
		{"p100 is the maximum", durations(3, 1, 2), 1.0, 3},
		{"unsorted input is handled", durations(9, 1, 5, 3, 7), 0.50, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.in, tc.p); got != tc.want {
				t.Fatalf("percentile(%v, %.2f) = %v, want %v", tc.in, tc.p, got, tc.want)
			}
		})
	}
}

func durations(v ...int) []time.Duration {
	out := make([]time.Duration, len(v))
	for i, x := range v {
		out[i] = time.Duration(x)
	}
	return out
}

func rangeDurations(n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = time.Duration(i)
	}
	return out
}

func checkInt(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %d, want %d", name, got, want)
	}
}

func checkFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %.6f, want %.6f", name, got, want)
	}
}

// ---------------------------------------------------------------------------
// The exact baseline.
// ---------------------------------------------------------------------------

// TestExactArmIsByteComparisonOnly records the baseline's defining weakness:
// it catches a repeat only when the arguments are identical after
// canonicalisation, which is precisely why the reworded retry slips past it.
func TestExactArmIsByteComparisonOnly(t *testing.T) {
	cfg := config{activityType: agent.ActivityBookFlight}
	tests := []struct {
		name string
		a, b string
		want core.DedupVerdict
	}{
		{"identical", "BLR to DEL Friday morning", "BLR to DEL Friday morning", core.VerdictDuplicate},
		{"different case and spacing", "BLR to DEL Friday morning", "blr  TO  del   friday morning", core.VerdictDuplicate},
		{"the fixture reword", agent.IntentBeforeCrash, agent.IntentAfterCrash, core.VerdictDistinct},
		{"genuinely different", "BLR to DEL Friday morning", "BLR to BOM Friday morning", core.VerdictDistinct},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := checkPair(context.Background(), cfg,
				arm{Name: "exact", Kind: kindExact},
				Pair{ID: "t", A: tc.a, B: tc.b}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if out.Verdict != tc.want {
				t.Fatalf("exact arm on %q vs %q gave %s, want %s", tc.a, tc.b, out.Verdict, tc.want)
			}
		})
	}
}
