package semantic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// THE HYBRID COMPARATOR: DECIDING FIELDS EXACTLY, FREE TEXT BY MEANING
// ---------------------------------------------------------------------------
//
// # Why this exists
//
// The pure-semantic path asks one question of a sentence encoder: "do these
// two requests mean the same thing?". The measured answer, in
// docs/results/semantic-experiment-minilm.txt, is that a general-purpose
// encoder answers a DIFFERENT question — "are these two sentences about the
// same topic?" — and answers it correctly. For
//
//	"BLR to DEL Friday morning"   vs   "BLR to DEL Saturday morning"
//
// the honest answer to the topic question is yes: same route, same traveller,
// same kind of request. But they are two different seats on two different
// days, so a mechanism that suppresses the second one loses a booking. At the
// shipped thresholds that run wrongly suppressed 108 of 115 genuinely distinct
// pairs, and all 91 of the lexically close ones.
//
// The diagnosis is not that the model is bad. It is that similarity is the
// wrong instrument for a field where ONE WORD changes the identity of the
// action. "Friday" to "Saturday" is a small edit in meaning-space and a total
// change in what gets charged to a card.
//
// # The split
//
// So this comparator splits one canonicalised intent into two parts:
//
//	DECIDING FIELDS  the values that constitute the identity of the action —
//	                 the route, the date, how many passengers. Two calls are
//	                 the same call only if every one of these matches EXACTLY.
//	                 No threshold, no model, no judgement.
//
//	FREE TEXT        whatever is left once those are removed. This is where
//	                 rewording is the real problem and where similarity is
//	                 the right instrument, because two phrasings of the same
//	                 request genuinely differ here and genuinely mean the same
//	                 thing.
//
// A repeat is therefore: ALL deciding fields equal AND the free text similar
// enough. The gate runs first, costs nothing but string comparison, and in
// practice rejects almost every candidate — which is also why the model is
// consulted far less often than on the pure-semantic path.
//
// # Where the fields come from, and what that costs
//
// They are EXTRACTED FROM TEXT, by hand-written rules, and that is a
// compromise forced by the shape of the activity input rather than a choice.
//
// The ideal is to read them off a structured input: BookFlight would take
// {"origin":"BLR","dest":"DEL","date":"2026-09-25","pax":1,"note":"window
// seat if possible"}, the engine would compare the four typed fields exactly
// and embed only the note, and nothing would ever have to guess. The demo
// agent does not have that shape. app/agent's BookFlight takes
// {"tool":"BookFlight","args":"BLR to DEL Friday morning"} — one prose string
// a planner wrote — because that is what a tool-calling agent actually emits
// when the tool schema is a free-text query. There are no structured fields to
// read, so they have to be recovered from the prose.
//
// The cost of that is real and has to be stated plainly:
//
//   - It is a domain schema. The extractor in itinerary.go knows airport
//     codes, weekdays and passenger counts. It works for flight booking and
//     for nothing else, and a second domain needs a second extractor.
//     Hand-written per-domain rules are exactly what the project's own
//     argument says does not scale — the whole appeal of embeddings was that
//     they need no such table. This comparator does not refute that argument;
//     it measures what the argument costs.
//   - It can fail silently. A phrasing that mentions no airport and no city
//     yields no fields at all. That case is handled explicitly — see
//     Fields.Recognised — by falling back to the pure-semantic comparison and
//     journaling that it did, because a gate that quietly passes everything
//     would be worse than no gate.
//   - An extraction mistake is a correctness bug with a real cost. Read a
//     date wrongly as equal and a booking is suppressed that should have
//     happened.
//
// The honest version of this fix is therefore a change to the ACTIVITY
// CONTRACT: an activity that wants semantic idempotency should declare which
// of its input fields decide identity, and pass them structurally. Extraction
// from prose is what the engine can do for an activity that has not done that
// yet.
//
// # Why it is off by default
//
// The verdict is journaled and replay reads it back, so two nodes running
// different comparators would reach different verdicts for the same history
// only on the live path — but a verdict is written once, and the one written
// is the one that governs the run forever. Turning the comparator on is
// therefore a deployment-wide decision (SEMANTIC_HYBRID, see
// internal/config), left off so that the default behaviour, and the published
// measurement of it, stay exactly as they were.

// Field is one deciding field: a named value that two invocations must agree
// on exactly before they can possibly be the same invocation.
//
// Both halves are strings, deliberately. The comparison this type exists for
// is equality, and the values are canonical tokens an extractor produced
// ("blr>del", "friday", "2"), not quantities to do arithmetic on. A typed
// value would invite comparing dates with a tolerance, and a tolerance is
// precisely the thing that lost the booking in the first place.
type Field struct {
	Name  string
	Value string
}

// Fields is one intent split into the part that must match exactly and the
// part where rewording is expected.
type Fields struct {
	// Deciding are the identity-bearing values, sorted by Name so that two
	// extractions of the same intent produce the same signature and the same
	// journaled explanation.
	Deciding []Field

	// Remainder is the free text left after the deciding fields and the
	// activity's own boilerplate have been removed, normalised to lowercase
	// single-spaced words.
	//
	// An EMPTY remainder is a meaningful answer, not a missing one: it says
	// the request consisted of nothing but its deciding fields. Comparing two
	// such requests needs no model at all — if every deciding field matches
	// there is nothing left that could disagree. See freeTextSimilarity.
	Remainder string

	// Recognised reports whether the extractor understood this intent at all.
	//
	// False means no deciding field could be read, which is not the same as
	// "this request has no deciding fields". It means the gate has no
	// information, and a gate with no information must not be allowed to
	// decide anything — the caller falls back to the pure-semantic
	// comparison and says so in the journaled verdict.
	Recognised bool
}

// FieldExtractor splits a rendered intent into deciding fields and free text.
//
// It is handed the intent TEXT rather than the raw input, for two reasons.
// First, that text is what is journaled and what the intent store keeps, so a
// candidate's fields can be re-derived from the log years later with no access
// to the original arguments. Second, it is already canonical — the exact
// package sorted the keys and normalised the numbers — so two spellings of one
// call cannot extract differently.
//
// It must be pure. The same text has to produce the same fields on every node
// and in every process, because an explanation recovered from the log is only
// an explanation if it is the same one the engine reached.
type FieldExtractor func(activityType, intentText string) (Fields, error)

// Signature renders the deciding fields as one stable string, for the
// journaled verdict: "pax=1|route=blr>del|when=friday".
func (f Fields) Signature() string {
	if len(f.Deciding) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(f.Deciding))
	for _, d := range f.Deciding {
		parts = append(parts, d.Name+"="+d.Value)
	}
	return strings.Join(parts, "|")
}

// value looks one field up. A field that is absent and a field that is present
// but empty are reported differently, because "this phrasing said nothing
// about the date" and "the date is empty" are different facts.
func (f Fields) value(name string) (string, bool) {
	for _, d := range f.Deciding {
		if d.Name == name {
			return d.Value, true
		}
	}
	return "", false
}

// sortDeciding puts the fields in name order. Extractors call it so they do
// not each have to remember to.
func (f *Fields) sortDeciding() {
	sort.Slice(f.Deciding, func(i, j int) bool { return f.Deciding[i].Name < f.Deciding[j].Name })
}

// absentValue is what a missing field is reported as in an explanation. It is
// not a value an extractor can produce, so it cannot be confused with one.
const absentValue = "(absent)"

// FirstDifference reports the first deciding field on which two intents
// disagree, in name order.
//
// A field present on one side and absent on the other IS a difference. One
// phrasing naming Friday and the other naming no day at all are not
// confidently the same booking, and the direction to be wrong in here is to
// run the activity again.
func FirstDifference(a, b Fields) (name, av, bv string, differs bool) {
	names := make([]string, 0, len(a.Deciding)+len(b.Deciding))
	seen := make(map[string]bool, len(a.Deciding)+len(b.Deciding))
	for _, src := range [][]Field{a.Deciding, b.Deciding} {
		for _, d := range src {
			if !seen[d.Name] {
				seen[d.Name] = true
				names = append(names, d.Name)
			}
		}
	}
	sort.Strings(names)

	for _, n := range names {
		x, okX := a.value(n)
		y, okY := b.value(n)
		if okX && okY && x == y {
			continue
		}
		if !okX {
			x = absentValue
		}
		if !okY {
			y = absentValue
		}
		return n, x, y, true
	}
	return "", "", "", false
}

// ---------------------------------------------------------------------------
// The check.
// ---------------------------------------------------------------------------

// checkHybrid is Check's body when a FieldExtractor is configured.
//
// The order of operations is the whole design: gate on the deciding fields
// first, and ask the model only about the candidates that survived. A
// candidate the gate rejected cannot become a duplicate however similar its
// wording is, so embedding it would be paying a model call for an answer that
// cannot change the verdict.
func (c *Checker) checkHybrid(
	ctx context.Context, req CheckRequest, th core.Thresholds,
	text string, base core.DedupEvaluated, start time.Time,
) (core.DedupEvaluated, error) {
	qf, err := c.Fields(req.ActivityType, text)
	if err != nil || !qf.Recognised {
		// The extractor could not read this intent. Falling back to the
		// pure-semantic comparison keeps the engine's behaviour defined, and
		// stamping the reason into the verdict keeps the log honest about
		// which comparator actually decided: a reader must never have to
		// guess whether a given verdict was gated.
		ev, serr := c.checkSemantic(ctx, req, th, text, base, start)
		if serr != nil {
			return ev, serr
		}
		ev.Detail = hybridUngatedDetail(err)
		return ev, nil
	}

	cands, err := c.Intents.Candidates(ctx, req.ShardID, req.WorkflowID, req.RunID, req.ActivityType)
	if err != nil {
		return core.DedupEvaluated{}, fmt.Errorf("semantic: load candidates: %w", err)
	}
	if len(cands) == 0 {
		ev := base
		ev.Detail = fmt.Sprintf("hybrid: first %s in this run; fields %s",
			req.ActivityType, qf.Signature())
		c.observe(req.ActivityType, ev.Verdict, start)
		return ev, nil
	}

	// The gate, then the model, in that order. A candidate the gate rejected
	// cannot become a duplicate however similar its wording is, so embedding
	// it would be paying for an answer that cannot change the verdict.
	//
	// best is the survivor whose free text came closest; nearest is the
	// rejected candidate that came closest, kept so that a DISTINCT verdict
	// can name the one field that stopped it.
	var best, nearest *core.Intent
	var bestCmp, nearCmp Comparison
	rejected := 0

	for i := range cands {
		cmp, cerr := c.compareAgainst(ctx, req.ActivityType, qf, cands[i].IntentText)
		if cerr != nil && !errors.Is(cerr, core.ErrEmbeddingUnavailable) {
			// A malformed model response is a real error, exactly as it is on
			// the pure path: something is wrong that degrading would hide.
			return core.DedupEvaluated{}, fmt.Errorf("semantic: compare free text: %w", cerr)
		}
		if cerr != nil {
			// The model is unreachable and this comparison needed it. Degrade
			// the way the pure path does: DISTINCT, the activity runs again,
			// and the model name is marked so the log never reads as though a
			// score of zero had been measured.
			ev := base
			ev.Model = FallbackPrefix + base.Model
			ev.Detail = fmt.Sprintf(
				"hybrid: every deciding field matched a prior call, but its free text could not "+
					"be compared because the model was unavailable; fields %s", qf.Signature())
			c.observe(req.ActivityType, ev.Verdict, start)
			return ev, nil
		}
		if cmp.Gated {
			rejected++
			if nearest == nil {
				nearest, nearCmp = &cands[i], cmp
			}
			continue
		}
		if best == nil || cmp.Similarity > bestCmp.Similarity {
			best, bestCmp = &cands[i], cmp
		}
	}

	if best == nil {
		ev := base
		ev.Detail = fmt.Sprintf(
			"hybrid: %d prior call(s), none agreeing on every deciding field; the closest, k=%d, "+
				"differs on %s (this call %q, that one %q); this call's fields %s",
			rejected, req.matchedK(nearest.ScheduledSeq),
			nearCmp.DifferingField, nearCmp.MineValue, nearCmp.TheirsValue, qf.Signature())
		c.observe(req.ActivityType, ev.Verdict, start)
		return ev, nil
	}

	// The survivor is the same action by every field the engine can compare
	// exactly. What is left is whether the free text says something the
	// deciding fields did not.
	ev := base
	ev.Similarity = bestCmp.Similarity
	ev.Verdict = th.Classify(bestCmp.Similarity)
	matched := req.matchedK(best.ScheduledSeq)
	if ev.Verdict != core.VerdictDistinct {
		ev.MatchedK = matched
	}

	how := "over the free text that remained"
	if !bestCmp.ModelUsed {
		how = "without consulting the model: nothing but the deciding fields was left to compare, " +
			"so the free text cannot disagree"
	}
	ev.Detail = fmt.Sprintf(
		"hybrid: every deciding field matches k=%d (%s); similarity %.4f %s",
		matched, qf.Signature(), bestCmp.Similarity, how)

	c.observe(req.ActivityType, ev.Verdict, start)
	return ev, nil
}

// ---------------------------------------------------------------------------
// One comparison, with no threshold applied.
// ---------------------------------------------------------------------------

// Comparison is the hybrid comparator's answer about ONE pair of intents,
// before any threshold is applied to it.
//
// It is exported because the evaluation in cmd/experiment scores a
// thirty-point threshold sweep from one pass over the corpus, and the only
// honest way to do that is to take the measurement from the engine's own
// comparator and apply core.Thresholds.Classify to it — which is the identical
// call checkHybrid makes above. A harness that re-implemented the gate would
// be measuring itself.
type Comparison struct {
	// MineReadable and TheirsReadable say whether the extractor could read
	// each side. An unreadable side is not a similar side and not a different
	// side; it is an absence of information, and the two fields exist so that
	// absence cannot be silently counted as either.
	MineReadable   bool
	TheirsReadable bool

	// Gated is this comparator's distinguishing answer: a deciding field
	// differs, so no threshold can make these two the same action.
	Gated bool

	// DifferingField names the field that closed the gate, with both values.
	DifferingField string
	MineValue      string
	TheirsValue    string

	// Similarity is the cosine over the free text. It is meaningful only when
	// the gate passed; when Gated is true nothing was measured and it is zero.
	Similarity float64

	// ModelUsed reports whether answering actually needed an embedding.
	//
	// It is here because it is a finding and not a detail. If it is false for
	// most of a corpus, the similarity half of this comparator contributed
	// nothing to the result, and the result is really a measurement of the
	// field extractor. The evaluation counts it for exactly that reason.
	ModelUsed bool

	MineFields   Fields
	TheirsFields Fields
}

// UnreadableField is the field name reported when a candidate's intent could
// not be split into fields at all. It is not a name any extractor can
// produce, so it cannot be confused with a real field that differed.
const UnreadableField = "(that call's fields could not be read)"

// Compare answers the hybrid question about two rendered intent texts: is the
// second one a repeat of the first?
//
// It takes TEXT on both sides, not activity inputs, because that is what the
// engine has to work with — a candidate exists in the log as its journaled
// intent text and nothing else.
func (c *Checker) Compare(ctx context.Context, activityType, mine, theirs string) (Comparison, error) {
	mf, err := c.Fields(activityType, mine)
	if err != nil {
		return Comparison{}, err
	}
	if !mf.Recognised {
		// Nothing can be said. The engine's answer in this case is to fall
		// back to the pure-semantic comparison; the caller decides, because
		// the fallback needs a candidate set this function does not have.
		return Comparison{}, nil
	}
	return c.compareAgainst(ctx, activityType, mf, theirs)
}

// compareAgainst is Compare with the first side already extracted, so a check
// against twenty candidates extracts the query's fields once instead of
// twenty times.
func (c *Checker) compareAgainst(
	ctx context.Context, activityType string, mf Fields, theirs string,
) (Comparison, error) {
	cmp := Comparison{MineReadable: true, MineFields: mf}

	tf, err := c.Fields(activityType, theirs)
	if err != nil || !tf.Recognised {
		// A candidate this extractor cannot read is not evidence that the two
		// calls agree, so it is rejected rather than waved through. This is
		// the mixed-history case: the row may have been written before the
		// comparator was turned on, or in a phrasing the rules do not cover.
		cmp.Gated = true
		cmp.DifferingField = UnreadableField
		cmp.MineValue, cmp.TheirsValue = mf.Signature(), clip(theirs, 120)
		return cmp, nil
	}
	cmp.TheirsReadable, cmp.TheirsFields = true, tf

	if name, mine, theirsValue, differs := FirstDifference(mf, tf); differs {
		cmp.Gated = true
		cmp.DifferingField, cmp.MineValue, cmp.TheirsValue = name, mine, theirsValue
		return cmp, nil
	}

	sim, used, err := c.freeTextSimilarity(ctx, mf.Remainder, tf.Remainder)
	if err != nil {
		cmp.ModelUsed = true
		return cmp, err
	}
	cmp.Similarity, cmp.ModelUsed = sim, used
	return cmp, nil
}

// hybridUngatedDetail words the "no gate was applied" explanation.
func hybridUngatedDetail(err error) string {
	if err != nil {
		return "hybrid: the field extractor failed on this intent (" + err.Error() +
			"), so this verdict is the pure-semantic one and no deciding field was compared"
	}
	return "hybrid: no deciding field could be read from this intent, so this verdict is the " +
		"pure-semantic one and no deciding field was compared"
}

// clip bounds a quoted string inside an explanation. A journaled verdict is
// read by people and stored forever; an intent text of any length does not
// get to decide how big an event row is.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// freeTextSimilarity scores the two remainders, and reports whether the model
// was needed to do it.
//
// Three cases never reach the model, and each is a decision worth stating:
//
//   - BOTH REMAINDERS EMPTY — 1.0. The two requests consist of nothing but
//     their deciding fields, and the caller has already established that
//     every one of those matches. There is nothing left that could make them
//     different actions, so this is not a guess at similarity, it is the
//     absence of anything to be similar about. This is also the common case
//     in practice, and it is why the hybrid path is cheap.
//   - IDENTICAL REMAINDERS — 1.0, for the same reason, reached by string
//     comparison instead of by two embeddings and a cosine that would return
//     1.0 anyway.
//   - ONE EMPTY, ONE NOT — 0. One phrasing carried free text the other did
//     not, and there is no honest cosine between a sentence and nothing. A
//     vector for the empty string is not an answer; it is a degenerate input
//     that Cosine deliberately scores 0. Scoring 0 here routes the pair to
//     DISTINCT, which runs the activity again — the safe direction, since the
//     extra words might be the part that makes this a different booking.
func (c *Checker) freeTextSimilarity(ctx context.Context, a, b string) (sim float64, usedModel bool, err error) {
	if a == b {
		return 1, false, nil
	}
	if a == "" || b == "" {
		return 0, false, nil
	}
	if c.Embedder == nil {
		return 0, true, core.ErrEmbeddingUnavailable
	}
	va, err := c.Embedder.Embed(ctx, a)
	if err != nil {
		return 0, true, err
	}
	vb, err := c.Embedder.Embed(ctx, b)
	if err != nil {
		return 0, true, err
	}
	return Cosine(va, vb), true, nil
}
