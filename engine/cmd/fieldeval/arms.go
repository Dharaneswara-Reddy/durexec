package main

import "github.com/dharaneswar339/durexec/internal/idem/semantic"

// ---------------------------------------------------------------------------
// THE ARMS
// ---------------------------------------------------------------------------
//
// An arm here is a COMPARATOR, not a threshold. Thresholds are swept at
// scoring time from one stored similarity per pair, because a pair's
// similarity is a property of the pair and the model and does not move when a
// threshold moves; sweeping by re-running would multiply the model calls by
// thirty and measure nothing new. The same is true of the escalation policy,
// which only changes what is DONE with a verdict, so both policies are scored
// from the same rows.
//
// What cannot be derived, and therefore defines an arm, is: which extractor
// read the fields, which gate rule judged them, and whether the intent text
// still had its capitals.
//
// The two baselines are measured in the same loop, over the same pairs, with
// the engine's own code. That is the only way a difference between the tables
// can be attributed to the comparators rather than to the measurement.

type armKind string

const (
	kindExact    armKind = "exact"
	kindSemantic armKind = "semantic"
	kindHybrid   armKind = "hybrid"
)

// arm is one comparator under test.
type arm struct {
	Name string
	Kind armKind

	// Extractor is the label that goes in the CSV: "itinerary", "generic",
	// "llm", or "" for the two baselines.
	Extractor string

	// Fields is the extractor itself. Nil on the baselines.
	Fields semantic.FieldExtractor

	GateMode    semantic.GateMode
	MinAgreeing int

	// GateDecides lets the gate's answer stand, with a dissimilar remainder
	// only able to escalate. See semantic.Checker.GateDecides for why, and
	// the results file for the before and after: this arm was added AFTER a
	// first run showed the remainder threshold discarding almost every
	// duplicate the gate had correctly passed.
	GateDecides bool

	// CasePreserved renders the intent WITHOUT app/agent's lowercasing.
	//
	// It exists because the generic extractor's one heuristic rule reads
	// capitalisation, and app/agent's own intent renderer destroys it:
	// agent.CanonicalIntent lowercases every tool call. On the engine's real
	// path the proper-noun rule therefore contributes nothing, and an arm
	// that quietly kept the capitals would report a number the engine cannot
	// reach. Both are measured, and the case-preserved one is marked
	// everywhere it appears as NOT the engine's current path.
	CasePreserved bool

	// GateOnly removes the similarity half entirely. It is measured as its
	// own arm, through the engine, as well as being derivable as an
	// ablation: the recommendation of this run is a gate-only configuration,
	// and a recommendation has to come from the engine's code path rather
	// than from arithmetic over somebody else's rows.
	GateOnly bool

	// Render overrides the run's rendering for this one arm. Empty means the
	// run's -render setting. It exists for a single arm: the one that
	// reproduces the rendering the published semantic-experiment files used,
	// so the difference between this run and those can be read off one table
	// instead of argued about.
	Render string

	// Shipped marks the comparator the engine runs by default, so nothing in
	// the output can be mistaken for a claim about current behaviour.
	Shipped bool
}

// buildArms lists every comparator to measure. The order is the order they
// appear in the report.
func buildArms(cfg config, haveLLM bool) []arm {
	arms := []arm{
		{
			Name: "exact",
			Kind: kindExact,
		},
		{
			Name:    "semantic",
			Kind:    kindSemantic,
			Shipped: true,
		},
		// The published rendering, kept as one arm for comparability.
		//
		// cmd/experiment — and therefore every figure in
		// docs/results/semantic-experiment-*.txt — embeds the whole
		// `Type({"args":"...","tool":"Type"})` envelope. That is the
		// checker's generic fallback, and it is NOT what the engine does for
		// the agent's tool calls: cmd/engined wires Checker.RenderIntent to
		// app/agent's renderer, which returns the bare sentence. The envelope
		// is mostly identical across every call of one activity type, so
		// embedding it inflates every cosine. This arm measures how much.
		{
			Name:   "semantic-envelope-rendering",
			Kind:   kindSemantic,
			Render: renderEnvelope,
		},
		// The hand-written baseline, under both gate rules. Strict is the
		// rule its published numbers were measured under; neutral is the
		// rule the live run's two artefact misses argued for.
		{
			Name:      "hybrid-itinerary-strict",
			Kind:      kindHybrid,
			Extractor: "itinerary",
			Fields:    semantic.ItineraryFields,
			GateMode:  semantic.GateStrict,
		},
		{
			Name:        "hybrid-itinerary-neutral",
			Kind:        kindHybrid,
			Extractor:   "itinerary",
			Fields:      semantic.ItineraryFields,
			GateMode:    semantic.GateNeutral,
			MinAgreeing: cfg.minAgree,
		},
		{
			Name:      "hybrid-generic-strict",
			Kind:      kindHybrid,
			Extractor: "generic",
			Fields:    semantic.GenericFields,
			GateMode:  semantic.GateStrict,
		},
		{
			Name:        "hybrid-generic-neutral",
			Kind:        kindHybrid,
			Extractor:   "generic",
			Fields:      semantic.GenericFields,
			GateMode:    semantic.GateNeutral,
			MinAgreeing: cfg.minAgree,
		},
		{
			Name:          "hybrid-generic-neutral-cased",
			Kind:          kindHybrid,
			Extractor:     "generic",
			Fields:        semantic.GenericFields,
			GateMode:      semantic.GateNeutral,
			MinAgreeing:   cfg.minAgree,
			CasePreserved: true,
		},
	}
	// The gate-only arms: the gate's answer, with the free text never
	// embedded and never consulted.
	arms = append(arms,
		arm{
			Name:        "hybrid-itinerary-neutral-gateonly",
			Kind:        kindHybrid,
			Extractor:   "itinerary",
			Fields:      semantic.ItineraryFields,
			GateMode:    semantic.GateNeutral,
			MinAgreeing: cfg.minAgree,
			GateOnly:    true,
		},
		arm{
			Name:        "hybrid-generic-neutral-gateonly",
			Kind:        kindHybrid,
			Extractor:   "generic",
			Fields:      semantic.GenericFields,
			GateMode:    semantic.GateNeutral,
			MinAgreeing: cfg.minAgree,
			GateOnly:    true,
		},
		arm{
			Name:          "hybrid-generic-neutral-gateonly-cased",
			Kind:          kindHybrid,
			Extractor:     "generic",
			Fields:        semantic.GenericFields,
			GateMode:      semantic.GateNeutral,
			MinAgreeing:   cfg.minAgree,
			GateOnly:      true,
			CasePreserved: true,
		},
	)

	// The gate-decides arms. They exist because of what the first run
	// measured, and the results file says so with both sets of numbers: the
	// gate on its own passed 90% of duplicates and the remainder threshold
	// then threw all but 4% of them away. These arms keep the gate's answer
	// and let the remainder escalate instead of veto.
	arms = append(arms,
		arm{
			Name:        "hybrid-itinerary-neutral-gatedecides",
			Kind:        kindHybrid,
			Extractor:   "itinerary",
			Fields:      semantic.ItineraryFields,
			GateMode:    semantic.GateNeutral,
			MinAgreeing: cfg.minAgree,
			GateDecides: true,
		},
		arm{
			Name:        "hybrid-generic-neutral-gatedecides",
			Kind:        kindHybrid,
			Extractor:   "generic",
			Fields:      semantic.GenericFields,
			GateMode:    semantic.GateNeutral,
			MinAgreeing: cfg.minAgree,
			GateDecides: true,
		},
	)

	if haveLLM {
		arms = append(arms,
			arm{
				Name:      "hybrid-llm-strict",
				Kind:      kindHybrid,
				Extractor: "llm",
				GateMode:  semantic.GateStrict,
			},
			arm{
				Name:        "hybrid-llm-neutral",
				Kind:        kindHybrid,
				Extractor:   "llm",
				GateMode:    semantic.GateNeutral,
				MinAgreeing: cfg.minAgree,
			},
			arm{
				Name:        "hybrid-llm-neutral-gatedecides",
				Kind:        kindHybrid,
				Extractor:   "llm",
				GateMode:    semantic.GateNeutral,
				MinAgreeing: cfg.minAgree,
				GateDecides: true,
			},
			arm{
				Name:        "hybrid-llm-neutral-gateonly",
				Kind:        kindHybrid,
				Extractor:   "llm",
				GateMode:    semantic.GateNeutral,
				MinAgreeing: cfg.minAgree,
				GateOnly:    true,
			},
		)
	}
	return arms
}

func armNames(arms []arm) []string {
	out := make([]string, 0, len(arms))
	for _, a := range arms {
		out = append(out, a.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// Policies and ablations: what is derived from the rows rather than measured.
// ---------------------------------------------------------------------------

// policy is what the engine does with an uncertain verdict.
type policy struct {
	Name string

	// EscalateStops says whether an ESCALATE verdict prevents the repeat. It
	// is the whole difference between the two policies, and it is a real
	// engine setting: internal/config's ESCALATION_BLOCKS, which defaults to
	// off. Under the default an escalated activity RUNS, so an escalation is
	// not a catch; under the strict policy it is held for a person, so it is.
	EscalateStops bool

	// Note is the sentence the report prints under the table.
	Note string
}

var policies = []policy{
	{
		Name:          "escalate-executes",
		EscalateStops: false,
		Note: "the engine's DEFAULT: an uncertain verdict runs the activity anyway, so only " +
			"a DUPLICATE verdict prevents the repeat",
	},
	{
		Name:          "escalate-blocks",
		EscalateStops: true,
		Note: "ESCALATION_BLOCKS=on, a real engine setting that is off by default: an uncertain " +
			"verdict holds the activity for a person, so it prevents the repeat at the cost of " +
			"asking someone",
	},
}

// ablation names which half of the hybrid comparator is allowed to decide.
//
// The point is to attribute the result. A hybrid figure that is really the
// gate's figure, with the embedder never consulted, is the caveat the
// published hybrid run had to carry, and the only way to show how much each
// half contributed is to score each half alone over the same rows.
type ablation struct {
	Name string
	Note string
}

const (
	// ablationFull is the comparator as built: the gate vetoes, and
	// similarity decides what survives the veto.
	ablationFull = "gate+similarity"

	// ablationGate is the gate alone, with similarity removed: a pair that
	// passes the gate is called a duplicate whatever its free text says.
	//
	// It is the row that answers "was this really the extractor?". Where a
	// comparator's gate-only figures equal its full figures, similarity
	// contributed nothing and the result is the extractor's alone.
	ablationGate = "gate-only"
)

// The remaining two legs of the ablation need no derivation, because they are
// measured arms in their own right: "exact only" is the exact comparator's
// rows and "similarity only" is the pure-semantic comparator's rows, over the
// identical pairs. Deriving them from the hybrid rows instead would have been
// impossible anyway — the comparator does not compute a free-text similarity
// for a pair its gate rejected, because computing one would be paying for an
// answer that cannot change the verdict.
var ablations = []ablation{
	{"exact only", "the engine's byte key: no model, no gate"},
	{"similarity only", "the shipped pure-semantic path, with no gate at all"},
	{ablationGate, "the gate's veto with similarity removed"},
	{ablationFull, "the comparator as built"},
}

// renderWith reports the rendering this arm is measured under.
func (a arm) renderWith(cfg config) string {
	if a.Render != "" {
		return a.Render
	}
	return cfg.render
}
