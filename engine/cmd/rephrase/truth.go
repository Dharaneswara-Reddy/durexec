package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// READING A SENTENCE BACK AGAINST THE GOAL IT CAME FROM
// ---------------------------------------------------------------------------
//
// A trial is only evidence about REPHRASING if both sentences still ask for
// the action the user asked for. A model that answers a Friday request with a
// Saturday sentence, or pays a different person, has not reworded anything —
// it has planned a different action, and counting that as a caught or missed
// duplicate would be counting a model error as a comparator result. So every
// sentence is read back against its goal here, and a trial where either side
// contradicts the goal is put in its own bucket and kept out of both the catch
// rate and the miss rate.
//
// WHY THIS IS NOT THE ENGINE'S EXTRACTOR. internal/idem/semantic/itinerary.go
// reads deciding fields too, and it would have been less code to reuse it. It
// would also have been circular: the question a trial has to answer is "are
// these the same action?", and answering it with the gate's own reading would
// make every pair the gate let through "the same action" by definition, so the
// hybrid comparator would score perfectly for free. This reader is anchored to
// the GOAL — what the user asked for — and not to the other sentence, and it
// is allowed to disagree with the gate. Where the two disagree is reported.
//
// THREE OUTCOMES PER FIELD, NOT TWO. The distinction that matters most is
// between contradicting the goal and merely not mentioning it:
//
//	agrees      the sentence states this field and states it correctly.
//	missing     the sentence is silent about it. It has not changed the action,
//	            but it has given a field-equality gate less to work with, and
//	            such a gate counts absent as different by design. Reported
//	            separately because that is a cost of the gate, not a model error.
//	contradicts the sentence states a value the goal did not ask for.
//
// Silence is read as agreement rather than as drift because the goal is the
// contract, and because a model that drops a detail has still asked for the
// same action as far as the user is concerned.

type verdictKind int

const (
	fieldAgrees verdictKind = iota
	fieldMissing
	fieldContradicts
)

func (k verdictKind) String() string {
	switch k {
	case fieldAgrees:
		return "agrees"
	case fieldMissing:
		return "missing"
	default:
		return "contradicts"
	}
}

// field is one deciding value of one goal, with the rule for finding it in a
// sentence. The rule is a closure rather than a table because the five domains
// have genuinely different shapes — an ordered pair of places, a monetary
// amount, an opaque identifier — and a single table would have to be bent to
// cover all of them.
type field struct {
	Name string
	Want string
	Read func(toks []string, flat string) (stated string, kind verdictKind)
}

// fieldReading is what one field turned out to say.
type fieldReading struct {
	Name   string
	Want   string
	Stated string
	Kind   verdictKind
}

// reading is one sentence measured against one goal, field by field.
type reading struct {
	Fields []fieldReading
}

// Drifted reports whether this sentence asks for a different action than the
// goal did.
func (r reading) Drifted() bool {
	for _, f := range r.Fields {
		if f.Kind == fieldContradicts {
			return true
		}
	}
	return false
}

// Contradictions words each disagreement for a person reading a CSV row.
func (r reading) Contradictions() []string {
	var out []string
	for _, f := range r.Fields {
		if f.Kind == fieldContradicts {
			out = append(out, fmt.Sprintf("%s: states %q, the goal said %q", f.Name, f.Stated, f.Want))
		}
	}
	return out
}

// Missing names the goal values the sentence did not mention.
func (r reading) Missing() []string {
	var out []string
	for _, f := range r.Fields {
		if f.Kind == fieldMissing {
			out = append(out, f.Name)
		}
	}
	return out
}

// Stated renders every field's finding, so a CSV row carries the whole reading
// and not only its conclusion.
func (r reading) Stated() []string {
	out := make([]string, 0, len(r.Fields))
	for _, f := range r.Fields {
		out = append(out, fmt.Sprintf("%s=%s(%s)", f.Name, f.Stated, f.Kind))
	}
	return out
}

// read measures one sentence against one goal.
func read(g goalSpec, sentence string) reading {
	toks := tokens(sentence)
	flat := strings.Join(toks, " ")
	var r reading
	for _, f := range g.Fields {
		stated, kind := f.Read(toks, flat)
		r.Fields = append(r.Fields, fieldReading{Name: f.Name, Want: f.Want, Stated: stated, Kind: kind})
	}
	return r
}

// tokens splits a sentence into lowercase words, breaking on everything that
// is not a letter or a digit.
//
// Commas between digits are removed FIRST, so that "4,500" survives as one
// number. Without that, a payment amount would arrive as the two tokens "4"
// and "500" and every amount check would be meaningless.
func tokens(s string) []string {
	s = strings.ToLower(s)
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if (r == ',' || r == '.') && i > 0 && i+1 < len(runes) &&
			unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.FieldsFunc(b.String(), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// ---------------------------------------------------------------------------
// Field readers.
// ---------------------------------------------------------------------------

// vocabField is the common case: a closed set of values, each with the
// spellings that mean it. Finding one of the OTHER values is a contradiction,
// which is what makes this reader able to say "the model changed the day".
//
// The vocabulary is the whole field, not just the wanted value. A reader that
// only knew the right answer could never detect a wrong one.
func vocabField(name, want string, vocab map[string][]string) field {
	if _, ok := vocab[want]; !ok {
		// A goal declaring a value the vocabulary cannot spell would silently
		// make that field unreadable and inflate the "missing" count. Fail at
		// construction instead, where it is a typo and not a finding.
		panic("rephrase: field " + name + " wants " + want + ", which is not in its vocabulary")
	}
	return field{Name: name, Want: want, Read: func(toks []string, flat string) (string, verdictKind) {
		found := valuesIn(flat, vocab)
		return classify(found, want)
	}}
}

// looseField is for a value with no closed set of alternatives — the subject
// of an email, for instance. It can report agreement or silence and NEVER
// reports a contradiction, because there is no vocabulary of wrong answers to
// recognise one against.
//
// That is a real weakness and it is why the message domain's drift detection
// is weaker than the other four's. Saying so is better than inventing a
// vocabulary of everything an email could be about.
func looseField(name, want string, accept []string) field {
	return field{Name: name, Want: want, Read: func(toks []string, flat string) (string, verdictKind) {
		for _, a := range accept {
			if containsPhrase(flat, a) {
				return want, fieldAgrees
			}
		}
		return "", fieldMissing
	}}
}

// orderedPairField reads a route: two places in travel order.
//
// Order is the point. A sentence can contain every correct word and still be a
// different trip, so [to, from] is a contradiction and not a match. One
// endpoint alone is read as silence rather than as drift: the model dropped a
// detail, it did not choose a different destination.
func orderedPairField(name, from, to string, vocab map[string][]string) field {
	want := from + ">" + to
	return field{Name: name, Want: want, Read: func(toks []string, flat string) (string, verdictKind) {
		seq := sequenceIn(toks, vocab)
		stated := strings.Join(seq, ">")
		switch {
		case len(seq) == 0:
			return "", fieldMissing
		case stated == want:
			return stated, fieldAgrees
		}
		for _, v := range seq {
			if v != from && v != to {
				return stated, fieldContradicts
			}
		}
		if len(seq) == 1 {
			return stated, fieldMissing
		}
		// Both endpoints, wrong way round.
		return stated, fieldContradicts
	}}
}

// amountField reads a single monetary amount.
//
// Every integer in the sentence is a candidate, because a payment sentence has
// no other numbers in it. An integer below the floor is ignored — a model
// writing "payment 1 of 1" or an ordinal should not be read as an amount — and
// any other integer at or above the floor is a different amount and therefore
// a contradiction.
func amountField(name string, want int, floor int) field {
	return field{Name: name, Want: strconv.Itoa(want), Read: func(toks []string, flat string) (string, verdictKind) {
		var found []int
		for _, t := range toks {
			n, err := strconv.Atoi(t)
			if err != nil || n < floor {
				continue
			}
			if !containsInt(found, n) {
				found = append(found, n)
			}
		}
		switch {
		case len(found) == 0:
			return "", fieldMissing
		case len(found) == 1 && found[0] == want:
			return strconv.Itoa(want), fieldAgrees
		}
		parts := make([]string, 0, len(found))
		for _, n := range found {
			parts = append(parts, strconv.Itoa(n))
		}
		stated := strings.Join(parts, ",")
		if containsInt(found, want) {
			// The right amount AND another one. Two amounts in a payment
			// sentence is not a rewording, whichever one the payee would be
			// sent, so this is a contradiction rather than a match.
			return stated, fieldContradicts
		}
		return stated, fieldContradicts
	}}
}

// identifierField reads an opaque identifier such as an order number.
//
// An identifier is the one deciding field with nothing to be similar about: it
// either matches or it does not, and a near-miss is a different order. The
// comparison is over alphanumerics only, so "A-10492", "A10492" and "a 10492"
// are the same identifier and a reformatting is not read as drift.
//
// A DIFFERENT identifier-shaped run of digits is a contradiction. The shape
// test is four or more consecutive digits, which no other value in these goals
// has.
func identifierField(name, want string) field {
	wantFlat := alnum(want)
	return field{Name: name, Want: want, Read: func(toks []string, flat string) (string, verdictKind) {
		joined := alnum(flat)
		if strings.Contains(joined, wantFlat) {
			return want, fieldAgrees
		}
		for _, t := range toks {
			if digitRun(t) >= 4 && !strings.Contains(wantFlat, t) {
				return t, fieldContradicts
			}
		}
		return "", fieldMissing
	}}
}

// ---------------------------------------------------------------------------
// Shared matching.
// ---------------------------------------------------------------------------

// valuesIn reports which values of a vocabulary the sentence mentions.
func valuesIn(flat string, vocab map[string][]string) []string {
	var found []string
	for value, spellings := range vocab {
		for _, s := range spellings {
			if containsPhrase(flat, s) {
				found = append(found, value)
				break
			}
		}
	}
	return found
}

// sequenceIn reports the vocabulary values in order of first appearance, which
// valuesIn cannot do because it iterates a map.
func sequenceIn(toks []string, vocab map[string][]string) []string {
	type hit struct {
		at    int
		value string
	}
	var hits []hit
	for value, spellings := range vocab {
		best := -1
		for _, s := range spellings {
			if at := phraseAt(toks, s); at >= 0 && (best < 0 || at < best) {
				best = at
			}
		}
		if best >= 0 {
			hits = append(hits, hit{best, value})
		}
	}
	for i := range hits {
		for j := i + 1; j < len(hits); j++ {
			if hits[j].at < hits[i].at {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.value)
	}
	return out
}

// containsPhrase matches a whole word or whole phrase, never a substring.
// Substring matching would find "sat" inside "satisfied" and read a Saturday
// into a sentence that never mentioned one.
func containsPhrase(flat, phrase string) bool {
	padded := " " + flat + " "
	return strings.Contains(padded, " "+phrase+" ")
}

// phraseAt reports the token index where a phrase starts, or -1.
func phraseAt(toks []string, phrase string) int {
	want := strings.Fields(phrase)
	if len(want) == 0 {
		return -1
	}
	for i := 0; i+len(want) <= len(toks); i++ {
		ok := true
		for j, w := range want {
			if toks[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// classify turns "these are the values the sentence mentioned" into a verdict.
func classify(found []string, want string) (string, verdictKind) {
	if len(found) == 0 {
		return "", fieldMissing
	}
	sortStrings(found)
	stated := strings.Join(found, "+")
	if len(found) == 1 && found[0] == want {
		return stated, fieldAgrees
	}
	// Mentioning the right value AND another one is still a contradiction: a
	// sentence naming two payees or two days does not name one action.
	return stated, fieldContradicts
}

func sortStrings(xs []string) {
	for i := range xs {
		for j := i + 1; j < len(xs); j++ {
			if xs[j] < xs[i] {
				xs[i], xs[j] = xs[j], xs[i]
			}
		}
	}
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func alnum(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// digitRun is the longest run of consecutive digits in a token.
func digitRun(s string) int {
	best, cur := 0, 0
	for _, r := range s {
		if unicode.IsDigit(r) {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return best
}

// joinOr renders a list for a CSV cell, with a readable stand-in for empty so
// a reader cannot mistake "nothing was wrong" for "this column was not filled
// in".
func joinOr(xs []string, empty string) string {
	if len(xs) == 0 {
		return empty
	}
	return strings.Join(xs, "; ")
}
