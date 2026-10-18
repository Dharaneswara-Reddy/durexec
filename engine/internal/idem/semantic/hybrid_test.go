package semantic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---- fakes ----------------------------------------------------------------

// scriptedEmbedder returns a vector chosen by the text it is handed, so a test
// can set the free-text similarity to an exact value instead of hoping a hash
// lands where it needs to. Anything unscripted is an error, because a test
// that silently embeds the wrong string would pass for the wrong reason.
type scriptedEmbedder struct {
	vecs  map[string][]float32
	calls []string
}

func (s *scriptedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	s.calls = append(s.calls, text)
	if v, ok := s.vecs[text]; ok {
		return v, nil
	}
	return nil, &unscriptedError{text: text}
}

func (s *scriptedEmbedder) Model() string { return "all-MiniLM-L6-v2" }
func (s *scriptedEmbedder) Dim() int      { return 3 }

type unscriptedError struct{ text string }

func (e *unscriptedError) Error() string { return "no vector scripted for " + e.text }

// bookingFixture wires a hybrid checker with one prior BookFlight already
// journaled, the way a run looks when the planner is about to re-issue a
// booking after a crash.
func bookingFixture(t *testing.T, emb core.Embedder, priorArgs string) *Checker {
	t.Helper()
	store := &memIntents{}
	if priorArgs != "" {
		text, err := IntentText("BookFlight", bookingInput(priorArgs))
		if err != nil {
			t.Fatal(err)
		}
		store.rows = append(store.rows, core.Intent{
			ShardID:      3,
			WorkflowID:   "trip-7",
			RunID:        "run-1",
			ScheduledSeq: 11,
			ActivityType: "BookFlight",
			IntentText:   text,
			// The stored vector is of the WHOLE intent text, which is what the
			// engine writes. The hybrid path does not read it — it compares
			// remainders — and this fixture leaves it deliberately useless to
			// prove that.
			Embedding: []float32{1, 0, 0},
		})
	}
	c := NewChecker(emb, store)
	c.Fields = ItineraryFields
	return c
}

func bookingInput(args string) json.RawMessage {
	b, err := json.Marshal(map[string]string{"tool": "BookFlight", "args": args})
	if err != nil {
		panic(err)
	}
	return b
}

func bookingRequest(args string) CheckRequest {
	return CheckRequest{
		ShardID:      3,
		WorkflowID:   "trip-7",
		RunID:        "run-1",
		K:            2,
		ActivityType: "BookFlight",
		Input:        bookingInput(args),
		KBySeq:       map[core.Seq]int{11: 0},
	}
}

// ---- the two cases the whole comparator exists for -------------------------

// TestHybridSameRouteDifferentDate is the failure the pure-semantic path was
// measured making: two bookings that mean the same topic and charge two cards.
// One deciding field differs, so no similarity score may rescue it.
func TestHybridSameRouteDifferentDate(t *testing.T) {
	// The embedder is DOWN. If the gate works, it is never consulted, because
	// a candidate rejected on a deciding field cannot become a duplicate
	// however similar its wording is.
	c := bookingFixture(t, downEmbedder{}, "BLR to DEL Friday morning")

	ev, err := c.Check(context.Background(), bookingRequest("BLR to DEL Saturday morning"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("verdict = %s, want DISTINCT: Saturday is not Friday", ev.Verdict)
	}
	if ev.MatchedK != NoMatch {
		t.Errorf("MatchedK = %d, want %d: a rejected candidate is not a match", ev.MatchedK, NoMatch)
	}
	if IsFallback(ev) {
		t.Errorf("Model = %q, want no fallback marker: the gate needed no model", ev.Model)
	}
	// The verdict has to be explainable from the log alone, which means naming
	// the field that differed and both of its values.
	for _, want := range []string{FieldWhen, "friday", "saturday"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q", ev.Detail, want)
		}
	}
}

// TestHybridSameBookingReworded is the fixture the project is built around:
// the planner reissues one booking in different words after a crash.
func TestHybridSameBookingReworded(t *testing.T) {
	c := bookingFixture(t, downEmbedder{}, "BLR to DEL Friday morning")

	ev, err := c.Check(context.Background(), bookingRequest("Bangalore to Delhi, Friday AM"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ev.Verdict != core.VerdictDuplicate {
		t.Fatalf("verdict = %s, want DUPLICATE: same route, day and daypart, reworded", ev.Verdict)
	}
	if ev.MatchedK != 0 {
		t.Errorf("MatchedK = %d, want 0: the booking it repeats", ev.MatchedK)
	}
	if ev.Similarity != 1 {
		t.Errorf("Similarity = %v, want 1: neither side had any free text left", ev.Similarity)
	}
	// Caught with the model unreachable. That is a property of this path, not
	// an accident: once the deciding fields match and nothing else is left,
	// there is nothing for a model to judge.
	if IsFallback(ev) {
		t.Errorf("Model = %q, want no fallback marker: no embedding was needed", ev.Model)
	}
	if !strings.Contains(ev.Detail, "without consulting the model") {
		t.Errorf("Detail = %q, want it to say no embedding was needed", ev.Detail)
	}
}

// ---- the free-text half ---------------------------------------------------

func TestHybridFreeTextDecidesWhenFieldsAgree(t *testing.T) {
	// Both sides book the same flight and then say something different about
	// it, so the remainder is where the answer has to come from.
	emb := &scriptedEmbedder{vecs: map[string][]float32{
		"window":           {1, 0, 0},
		"prefer by window": {1, 0.05, 0}, // nearly the same direction: similar
		"aisle":            {0, 1, 0},    // orthogonal: not similar
	}}

	tests := []struct {
		name string
		args string
		want core.DedupVerdict
	}{
		{"reworded preference", "BLR to DEL Friday morning, prefer by window", core.VerdictDuplicate},
		{"different preference", "BLR to DEL Friday morning, aisle", core.VerdictDistinct},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := bookingFixture(t, emb, "BLR to DEL Friday morning, window")
			ev, err := c.Check(context.Background(), bookingRequest(tc.args))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if ev.Verdict != tc.want {
				t.Fatalf("verdict = %s (similarity %.4f), want %s", ev.Verdict, ev.Similarity, tc.want)
			}
		})
	}
}

// TestHybridOneSidedFreeText covers the asymmetric case: one phrasing carried
// free text and the other did not. There is no honest cosine between a
// sentence and nothing, and the extra words might be what makes this a
// different booking, so the pair must not be suppressed.
func TestHybridOneSidedFreeText(t *testing.T) {
	c := bookingFixture(t, &scriptedEmbedder{}, "BLR to DEL Friday morning")

	ev, err := c.Check(context.Background(), bookingRequest("BLR to DEL Friday morning, window"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("verdict = %s, want DISTINCT", ev.Verdict)
	}
	if ev.Similarity != 0 {
		t.Errorf("Similarity = %v, want 0", ev.Similarity)
	}
}

// TestHybridModelDownWithFreeText proves the degraded path still points the
// safe way: the activity runs again, and the log says the model was missing
// rather than claiming a measured score of zero.
func TestHybridModelDownWithFreeText(t *testing.T) {
	c := bookingFixture(t, downEmbedder{}, "BLR to DEL Friday morning, window")

	ev, err := c.Check(context.Background(), bookingRequest("BLR to DEL Friday morning, aisle"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("verdict = %s, want DISTINCT", ev.Verdict)
	}
	if !IsFallback(ev) {
		t.Errorf("Model = %q, want the fallback marker", ev.Model)
	}
	if !strings.Contains(ev.Detail, "unavailable") {
		t.Errorf("Detail = %q, want it to say the model was unavailable", ev.Detail)
	}
}

// TestHybridBrokenModelIsAnError keeps the distinction the pure path makes: a
// malformed response is a fault to surface, not a condition to degrade past.
func TestHybridBrokenModelIsAnError(t *testing.T) {
	c := bookingFixture(t, brokenEmbedder{}, "BLR to DEL Friday morning, window")

	if _, err := c.Check(context.Background(), bookingRequest("BLR to DEL Friday morning, aisle")); err == nil {
		t.Fatal("Check returned no error for a malformed model response")
	}
}

// ---- unreadable intents ---------------------------------------------------

// TestHybridFallsBackWhenNothingIsRecognised is the honest failure mode of a
// text scraper: a phrasing outside its vocabulary. The gate must contribute
// nothing rather than pass everything, and the verdict must say so.
func TestHybridFallsBackWhenNothingIsRecognised(t *testing.T) {
	c := bookingFixture(t, NewFakeEmbedder(0), "")
	c.Intents.(*memIntents).rows = nil

	ev, err := c.Check(context.Background(), bookingRequest("whatever the agent decided"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("verdict = %s, want DISTINCT", ev.Verdict)
	}
	if !strings.Contains(ev.Detail, "pure-semantic") {
		t.Errorf("Detail = %q, want it to say the verdict was not gated", ev.Detail)
	}
}

// TestHybridRejectsUnreadableCandidate covers a run whose history was written
// before the comparator was turned on: the stored intent cannot be split into
// fields, so it is not evidence that the two calls agree.
func TestHybridRejectsUnreadableCandidate(t *testing.T) {
	c := bookingFixture(t, downEmbedder{}, "whatever the agent decided")

	ev, err := c.Check(context.Background(), bookingRequest("BLR to DEL Friday morning"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ev.Verdict != core.VerdictDistinct {
		t.Fatalf("verdict = %s, want DISTINCT", ev.Verdict)
	}
	if !strings.Contains(ev.Detail, "could not be read") {
		t.Errorf("Detail = %q, want it to say the candidate was unreadable", ev.Detail)
	}
}

// ---- the default path is untouched ----------------------------------------

// TestPureSemanticPathLeavesDetailEmpty is the compatibility guard. The
// published measurement is of the path with no extractor configured, and a
// verdict it journals has to be byte-for-byte what it always was.
func TestPureSemanticPathLeavesDetailEmpty(t *testing.T) {
	c := bookingFixture(t, NewFakeEmbedder(0), "BLR to DEL Friday morning")
	c.Fields = nil

	ev, err := c.Check(context.Background(), bookingRequest("Bangalore to Delhi, Friday AM"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ev.Detail != "" {
		t.Errorf("Detail = %q, want empty on the pure-semantic path", ev.Detail)
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "detail") {
		t.Errorf("payload = %s, want no detail key at all", payload)
	}
}

// TestDedupEvaluatedDecodesWithoutDetail is the other half of that guarantee:
// an event journaled before the field existed still decodes.
func TestDedupEvaluatedDecodesWithoutDetail(t *testing.T) {
	const old = `{"k":2,"verdict":"DUPLICATE","matched_k":0,"similarity":0.93,` +
		`"tau_high":0.9,"tau_low":0.85,"model":"all-MiniLM-L6-v2","intent_text":"book BLR-DEL"}`
	var ev core.DedupEvaluated
	if err := json.Unmarshal([]byte(old), &ev); err != nil {
		t.Fatalf("decode a pre-change verdict: %v", err)
	}
	if ev.Verdict != core.VerdictDuplicate || ev.MatchedK != 0 || ev.Detail != "" {
		t.Fatalf("decoded = %+v, want the recorded verdict with an empty Detail", ev)
	}
}

// ---- the extractor --------------------------------------------------------

func TestItineraryFields(t *testing.T) {
	tests := []struct {
		name      string
		args      string
		signature string
		remainder string
	}{
		{"codes", "BLR to DEL Friday morning",
			"daypart=am|pax=1|route=blr>del|when=friday", ""},
		{"city names and AM", "Bangalore to Delhi, Friday AM",
			"daypart=am|pax=1|route=blr>del|when=friday", ""},
		{"prose phrasing", "book a Friday morning flight from Bangalore to Delhi",
			"daypart=am|pax=1|route=blr>del|when=friday", ""},
		{"spelled-out count and before noon", "one seat Bengaluru to New Delhi, Friday before noon",
			"daypart=am|pax=1|route=blr>del|when=friday", ""},
		{"two passengers", "Bangalore to Delhi, Friday AM, 2 passengers",
			"daypart=am|pax=2|route=blr>del|when=friday", ""},
		{"evening and night share a daypart", "BOM to SIN Thursday night for three",
			"daypart=pm|pax=3|route=bom>sin|when=thursday", ""},
		{"a clock hour is a daypart", "three seats Bombay to Singapore, Thursday after 9pm",
			"daypart=pm|pax=3|route=bom>sin|when=thursday", ""},
		{"next week is a different date", "BLR to DEL next Friday morning",
			"daypart=am|pax=1|route=blr>del|when=friday+1w", ""},
		{"and so is the long way of saying it", "Bangalore to Delhi, the Friday after this one, AM",
			"daypart=am|pax=1|route=blr>del|when=friday+1w", ""},
		{"and so is a week on Friday", "book a flight from Bangalore to Delhi a week on Friday, morning",
			"daypart=am|pax=1|route=blr>del|when=friday+1w", ""},
		{"direction is the order of the cities", "DEL to BLR Friday morning",
			"daypart=am|pax=1|route=del>blr|when=friday", ""},
		{"a return leg is a second day", "BLR to DEL Friday morning returning Sunday",
			"daypart=am|pax=1|route=blr>del|when=friday>sunday", "returning"},
		{"anything unrecognised survives as free text", "BLR to DEL Friday morning, window seat if possible",
			"daypart=am|pax=1|route=blr>del|when=friday", "window if possible"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, err := IntentText("BookFlight", bookingInput(tc.args))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ItineraryFields("BookFlight", text)
			if err != nil {
				t.Fatalf("ItineraryFields: %v", err)
			}
			if !got.Recognised {
				t.Fatalf("Recognised = false for %q", tc.args)
			}
			if sig := got.Signature(); sig != tc.signature {
				t.Errorf("Signature() = %q, want %q", sig, tc.signature)
			}
			if got.Remainder != tc.remainder {
				t.Errorf("Remainder = %q, want %q", got.Remainder, tc.remainder)
			}
		})
	}
}

func TestItineraryFieldsRecognisesNothingWithoutARoute(t *testing.T) {
	text, err := IntentText("BookFlight", bookingInput("sort out my trip on Friday morning please"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ItineraryFields("BookFlight", text)
	if err != nil {
		t.Fatalf("ItineraryFields: %v", err)
	}
	// A daypart and a weekday with no route would gate two unrelated trips
	// together, so the extractor reports that it understood nothing.
	if got.Recognised || len(got.Deciding) != 0 {
		t.Fatalf("got %+v, want nothing recognised without a route", got)
	}
}

func TestFirstDifference(t *testing.T) {
	a := Fields{Recognised: true, Deciding: []Field{
		{Name: FieldPax, Value: "1"}, {Name: FieldRoute, Value: "blr>del"},
		{Name: FieldWhen, Value: "friday"},
	}}

	tests := []struct {
		name    string
		b       Fields
		want    string
		differs bool
	}{
		{"identical", a, "", false},
		{"date moves", Fields{Recognised: true, Deciding: []Field{
			{Name: FieldPax, Value: "1"}, {Name: FieldRoute, Value: "blr>del"},
			{Name: FieldWhen, Value: "saturday"},
		}}, FieldWhen, true},
		{"count moves", Fields{Recognised: true, Deciding: []Field{
			{Name: FieldPax, Value: "2"}, {Name: FieldRoute, Value: "blr>del"},
			{Name: FieldWhen, Value: "friday"},
		}}, FieldPax, true},
		{"an absent field is a difference", Fields{Recognised: true, Deciding: []Field{
			{Name: FieldPax, Value: "1"}, {Name: FieldRoute, Value: "blr>del"},
		}}, FieldWhen, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, _, _, differs := FirstDifference(a, tc.b)
			if differs != tc.differs || name != tc.want {
				t.Fatalf("FirstDifference = (%q, %t), want (%q, %t)", name, differs, tc.want, tc.differs)
			}
		})
	}
}
