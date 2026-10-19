package semantic

import (
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// WHAT CLOSES THE GATE: TWO RULES, AND WHY THERE ARE TWO
// ---------------------------------------------------------------------------
//
// The hybrid comparator's gate asks one question of two field sets: is there
// a deciding field on which these two calls disagree? Until now there was one
// answer, FirstDifference, and it treated a field PRESENT ON ONE SIDE AND
// ABSENT ON THE OTHER as a disagreement.
//
// The live crash-and-rephrase run measured what that costs. On flights — the
// only domain whose intents the hand-written extractor could read at all —
// the hybrid comparator caught 2 of 5 genuine rewordings where the plain
// semantic path caught 3, and BOTH of its extra misses were artefacts of the
// absent-is-conflicting rule rather than two different actions:
//
//	daypart "am>am"  vs  "am"         the same value, rendered twice
//	daypart "am"     vs  "(absent)"   the second sentence did not restate it
//
// Neither pair is two different bookings. A planner that reworded "Friday
// morning" as "Friday" has not changed the day; it has said less. Reading
// silence as contradiction turns every omission into a second seat.
//
// So there are now two gate rules, and which one a run used is part of the
// result:
//
//	GateStrict   absent counts as a difference. This is the original rule and
//	             remains the DEFAULT, because it is what produced every
//	             published hybrid number, and a default that quietly changed
//	             would make two result files incomparable without saying so.
//	GateNeutral  only two fields that are BOTH PRESENT and genuinely
//	             contradict close the gate. Absence is treated as no
//	             information, which is what it is.
//
// GateNeutral is not free, and the direction of its cost is the dangerous
// one. Under GateStrict a mistake suppresses nothing: an invented difference
// runs the activity again, which is wasteful but safe. Under GateNeutral an
// intent that states almost nothing can pass the gate on one agreeing field
// and be suppressed on similarity alone. MinAgreeing exists for exactly that
// and is why the harness measures GateNeutral at more than one setting.

// GateMode selects how the gate reads a field that only one side stated.
type GateMode int

const (
	// GateStrict treats a field present on one side and absent on the other
	// as a difference. The default, and the rule every published hybrid
	// figure was measured under.
	GateStrict GateMode = iota

	// GateNeutral closes the gate only on two values that are both present
	// and differ.
	GateNeutral
)

func (m GateMode) String() string {
	if m == GateNeutral {
		return "neutral"
	}
	return "strict"
}

// FirstConflict reports the first deciding field that BOTH sides state and
// state differently, in name order.
//
// It is the GateNeutral counterpart of FirstDifference, kept as a separate
// exported function rather than a flag on that one so that nothing which
// already calls FirstDifference can change behaviour by accident.
func FirstConflict(a, b Fields) (name, av, bv string, differs bool) {
	for _, n := range fieldNames(a, b) {
		x, okX := a.value(n)
		y, okY := b.value(n)
		if !okX || !okY {
			// One side said nothing about this field. Silence is not a
			// contradiction; see the header.
			continue
		}
		if valuesConflict(x, y) {
			return n, x, y, true
		}
	}
	return "", "", "", false
}

// valuesConflict decides whether two values of ONE field contradict.
//
// A field can hold several values — two places, two numbers, an id and a
// priority — joined with ">". Under the strict rule those joined strings are
// compared whole, so one side mentioning an extra value is a difference.
// Under the neutral rule the same reasoning that makes an ABSENT FIELD
// neutral applies one level down: if one side's values are a SUBSET of the
// other's, the shorter side has said less, not something else.
//
//	"10>30" vs "30"            not a conflict: one sentence named a time the
//	                           other did not
//	"4500"  vs "5400"          a conflict: two different amounts
//	"p1>tck219" vs "p3>tck219" a conflict: neither contains the other
//
// The danger of subset tolerance is real and is the reason MinAgreeing
// exists: a nearly empty intent is a subset of almost everything. It is
// measured at more than one MinAgreeing setting for that reason.
func valuesConflict(x, y string) bool {
	if x == y {
		return false
	}
	xs := strings.Split(x, ">")
	ys := strings.Split(y, ">")
	return !subset(xs, ys) && !subset(ys, xs)
}

// subset reports whether every value in a is also in b.
func subset(a, b []string) bool {
	for _, v := range a {
		found := false
		for _, w := range b {
			if v == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// CountAgreeing is how many deciding fields both sides state without
// contradicting each other.
//
// It is the only positive evidence a gate ever has. Under GateNeutral a pair
// with zero agreeing fields has passed the gate without the gate having
// learned anything, and a comparator that called such a pair a duplicate
// would be deciding on similarity alone while reporting that its fields
// matched. MinAgreeing turns that into a refusal instead.
func CountAgreeing(a, b Fields) int {
	n := 0
	for _, d := range a.Deciding {
		if v, ok := b.value(d.Name); ok && !valuesConflict(v, d.Value) {
			n++
		}
	}
	return n
}

// fieldNames is the sorted union of both sides' field names. Sorted, because
// a journaled explanation that named a different field depending on map
// iteration order would not be an explanation.
func fieldNames(a, b Fields) []string {
	seen := make(map[string]bool, len(a.Deciding)+len(b.Deciding))
	names := make([]string, 0, len(a.Deciding)+len(b.Deciding))
	for _, src := range [][]Field{a.Deciding, b.Deciding} {
		for _, d := range src {
			if !seen[d.Name] {
				seen[d.Name] = true
				names = append(names, d.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// TooFewAgreeingField is the field name reported when a pair was rejected for
// having no field evidence rather than for a conflict. It is not a name any
// extractor can produce, so it cannot be mistaken for a real field.
const TooFewAgreeingField = "(too few agreeing deciding fields)"

// gateFailure applies this checker's gate rule to one pair of field sets.
//
// It is the single place the two rules are chosen between, so no caller can
// implement half of one.
func (c *Checker) gateFailure(a, b Fields) (name, av, bv string, closed bool) {
	switch c.GateMode {
	case GateNeutral:
		if n, x, y, differs := FirstConflict(a, b); differs {
			return n, x, y, true
		}
		if agree := CountAgreeing(a, b); agree < c.MinAgreeing {
			return TooFewAgreeingField, itoa(agree), itoa(c.MinAgreeing), true
		}
		return "", "", "", false
	default:
		return FirstDifference(a, b)
	}
}

// itoa avoids pulling strconv in for two call sites that only ever format a
// small non-negative count.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 && i > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
