package main

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/dharaneswar339/durexec/app/agent"
)

// ---------------------------------------------------------------------------
// THE CORPUS, AND WHAT IT IS NOT
// ---------------------------------------------------------------------------
//
// READ THIS BEFORE QUOTING ANY NUMBER THIS HARNESS PRINTS.
//
// In the real evaluation described in the design document, the retry pairs are
// HAND-LABELLED. A person reads two phrasings of a booking request and decides
// whether they are the same request or two different ones. That judgement is
// the ground truth, because "do these mean the same thing?" is a question about
// human intent, and there is no algorithm that can settle it — if there were,
// the engine would just use that algorithm and the whole project would be
// unnecessary.
//
// The corpus below is NOT that. It is a set of pairs the author wrote by hand
// and labelled by construction: two phrasings of the same itinerary record are
// labelled duplicate, two phrasings of different itinerary records are labelled
// distinct. It exists so the harness can be wired up, run end to end, and smoke
// tested — so that the plumbing, the arms, the metrics and the CSV are all
// known to work before a single hour of human labelling is spent.
//
// Three specific limitations follow from that, and the report must state them:
//
//  1. The labels are assumed, not observed. A pair is called a duplicate
//     because it was generated from one itinerary record, not because anyone
//     confirmed a user would consider the two requests interchangeable. Where
//     the generator's assumption is wrong, the measured accuracy is wrong in
//     the same direction and by the same amount.
//
//  2. The paraphrases are one author's idea of how a language model rewords a
//     request. Real retry pairs come out of a real planner under real recovery,
//     and their distribution of rewordings is an empirical fact this corpus
//     does not have access to. The negatives in particular are probably easier
//     than real ones, because they were written to differ in a named attribute.
//
//  3. The corpus is small and narrow: one domain, flight booking, and a few
//     hundred pairs. Nothing here supports a claim about semantic idempotency
//     in general.
//
// So: numbers produced from this corpus are evidence that the HARNESS works.
// They are not evidence that the CONTRIBUTION works. The precision and recall
// figures in the report must come from the hand-labelled set, measured with
// this same harness, and must say how many pairs a human labelled and what the
// inter-annotator agreement was.
//
// ---------------------------------------------------------------------------

// Label is the ground truth for one pair.
type Label bool

const (
	// Distinct means the two requests are genuinely different work. Booking
	// both is correct.
	Distinct Label = false
	// Duplicate means the two requests are the same work worded differently.
	// Booking both charges the user twice, which is the bug.
	Duplicate Label = true
)

func (l Label) String() string {
	if bool(l) {
		return "DUPLICATE"
	}
	return "DISTINCT"
}

// Pair is one labelled retry pair: what was asked the first time, what came
// back after recovery, and whether a human would call them the same request.
type Pair struct {
	// ID is stable across runs so a row in the CSV can be traced back here.
	ID string

	// Category says how the pair was made, so results can be broken down by
	// how hard the pair is rather than reported as one undifferentiated number.
	//
	//	fixture    the pair from app/agent's DoubleBooking scenario
	//	surface    same words, different case or spacing
	//	paraphrase same itinerary, genuinely different wording
	//	contrast   different itineraries, differing in a named attribute
	//	near-miss  two requests that share nearly all their words and still
	//	           mean different work
	Category string

	// Hard marks a negative that is lexically close to its partner, so a
	// system matching on meaning has to work to keep the two apart.
	//
	// The distinction matters because it is the only thing that makes a
	// precision number worth reading. "Kolkata to Goa" and "Pune to Dubai"
	// share almost no words, so any method at all separates them, and a
	// corpus padded with pairs like that reports a precision close to 1.0
	// while proving nothing. "BLR to DEL Friday morning" and "BLR to DEL
	// Saturday morning" differ by one word out of five and mean two
	// different seats on two different days. Precision measured over those
	// is the number that has something to say.
	Hard bool

	// Note explains what makes this pair what it is: for a duplicate, the kind
	// of rewording; for a distinct pair, the attribute that differs.
	Note string

	// A is the phrasing recorded before the crash. B is the phrasing the
	// recovered planner produced.
	A, B string

	// Truth is the ground-truth label. See the warning at the top of this file
	// about where it comes from.
	Truth Label
}

// Itinerary is one real-world booking request written down once, together with
// several phrasings a planner might plausibly produce for it.
//
// Every phrasing of one itinerary describes the same seat on the same flight
// for the same people, so any two of them form a duplicate pair. Two phrasings
// drawn from different itineraries form a distinct pair.
type Itinerary struct {
	ID         string
	Route      string
	Day        string
	TimeOfDay  string
	Passengers int
	Phrasings  []string
}

// itineraries are the distinct pieces of real work in the corpus. The first one
// is the DoubleBooking fixture's itinerary, and its first two phrasings are the
// fixture's own two strings — see mustMatchFixture.
var itineraries = []Itinerary{
	{
		ID: "blr-del-fri-am-1", Route: "BLR->DEL", Day: "Friday", TimeOfDay: "morning", Passengers: 1,
		Phrasings: []string{
			agent.IntentBeforeCrash, // "BLR to DEL Friday morning"
			agent.IntentAfterCrash,  // "Bangalore to Delhi, Friday AM"
			"book a Friday morning flight from Bangalore to Delhi",
			"one seat Bengaluru to New Delhi, Friday before noon",
		},
	},
	{
		ID: "blr-del-fri-pm-1", Route: "BLR->DEL", Day: "Friday", TimeOfDay: "evening", Passengers: 1,
		Phrasings: []string{
			"BLR to DEL Friday evening",
			"Bangalore to Delhi, Friday PM",
			"book a Friday evening flight from Bangalore to Delhi",
			"one seat Bengaluru to New Delhi, Friday after 6pm",
		},
	},
	{
		ID: "blr-bom-fri-am-1", Route: "BLR->BOM", Day: "Friday", TimeOfDay: "morning", Passengers: 1,
		Phrasings: []string{
			"BLR to BOM Friday morning",
			"Bangalore to Mumbai, Friday AM",
			"book a Friday morning flight from Bangalore to Mumbai",
			"one seat Bengaluru to Bombay, Friday before noon",
		},
	},
	{
		ID: "blr-del-sat-am-1", Route: "BLR->DEL", Day: "Saturday", TimeOfDay: "morning", Passengers: 1,
		Phrasings: []string{
			"BLR to DEL Saturday morning",
			"Bangalore to Delhi, Saturday AM",
			"book a Saturday morning flight from Bangalore to Delhi",
			"one seat Bengaluru to New Delhi, Saturday before noon",
		},
	},
	{
		ID: "blr-del-fri-am-2", Route: "BLR->DEL", Day: "Friday", TimeOfDay: "morning", Passengers: 2,
		Phrasings: []string{
			"BLR to DEL Friday morning for two",
			"Bangalore to Delhi, Friday AM, 2 passengers",
			"book two Friday morning seats from Bangalore to Delhi",
			"two seats Bengaluru to New Delhi, Friday before noon",
		},
	},
	{
		ID: "del-blr-fri-am-1", Route: "DEL->BLR", Day: "Friday", TimeOfDay: "morning", Passengers: 1,
		Phrasings: []string{
			"DEL to BLR Friday morning",
			"Delhi to Bangalore, Friday AM",
			"book a Friday morning flight from Delhi to Bangalore",
			"one seat New Delhi to Bengaluru, Friday before noon",
		},
	},
	{
		ID: "maa-hyd-mon-am-1", Route: "MAA->HYD", Day: "Monday", TimeOfDay: "morning", Passengers: 1,
		Phrasings: []string{
			"MAA to HYD Monday morning",
			"Chennai to Hyderabad, Monday AM",
			"book a Monday morning flight from Chennai to Hyderabad",
			"one seat Madras to Hyderabad, Monday before noon",
		},
	},
	{
		ID: "hyd-maa-mon-am-1", Route: "HYD->MAA", Day: "Monday", TimeOfDay: "morning", Passengers: 1,
		Phrasings: []string{
			"HYD to MAA Monday morning",
			"Hyderabad to Chennai, Monday AM",
			"book a Monday morning flight from Hyderabad to Chennai",
			"one seat Hyderabad to Madras, Monday before noon",
		},
	},
	{
		ID: "ccu-goi-sun-pm-2", Route: "CCU->GOI", Day: "Sunday", TimeOfDay: "evening", Passengers: 2,
		Phrasings: []string{
			"CCU to GOI Sunday evening for two",
			"Kolkata to Goa, Sunday PM, 2 passengers",
			"book two Sunday evening seats from Kolkata to Goa",
			"two seats Calcutta to Goa, Sunday after 6pm",
		},
	},
	{
		ID: "pnq-dxb-wed-am-1", Route: "PNQ->DXB", Day: "Wednesday", TimeOfDay: "morning", Passengers: 1,
		Phrasings: []string{
			"PNQ to DXB Wednesday morning",
			"Pune to Dubai, Wednesday AM",
			"book a Wednesday morning flight from Pune to Dubai",
			"one seat Pune to Dubai, Wednesday before noon",
		},
	},
	{
		ID: "bom-sin-thu-pm-3", Route: "BOM->SIN", Day: "Thursday", TimeOfDay: "night", Passengers: 3,
		Phrasings: []string{
			"BOM to SIN Thursday night for three",
			"Mumbai to Singapore, Thursday PM, 3 passengers",
			"book three Thursday night seats from Mumbai to Singapore",
			"three seats Bombay to Singapore, Thursday after 9pm",
		},
	},

	// The three itineraries below exist only to be confused with ones above
	// them. Each one is the trip a planner is most plausibly going to mix up
	// with the fixture's trip, which is the point: a negative is only worth
	// measuring if something could realistically have got it wrong.
	{
		// The return leg. In a real agent run this is the single most likely
		// thing to be mistaken for the outbound booking — same two cities,
		// same traveller, same trip, and it is a second seat that must be
		// bought.
		ID: "del-blr-sun-pm-1", Route: "DEL->BLR", Day: "Sunday", TimeOfDay: "evening", Passengers: 1,
		Phrasings: []string{
			"DEL to BLR Sunday evening",
			"Delhi to Bangalore, Sunday PM",
			"book a Sunday evening flight from Delhi to Bangalore",
			"one seat New Delhi to Bengaluru, Sunday after 6pm",
		},
	},
	{
		// The same route, on the same weekday, one week later. Every content
		// word except "next" is shared with the fixture's itinerary, and the
		// two are different flights on different dates.
		ID: "blr-del-nextfri-am-1", Route: "BLR->DEL", Day: "next Friday", TimeOfDay: "morning", Passengers: 1,
		Phrasings: []string{
			"BLR to DEL next Friday morning",
			"Bangalore to Delhi, the Friday after this one, AM",
			"book a flight from Bangalore to Delhi a week on Friday, morning",
			"one seat Bengaluru to New Delhi, next Friday before noon",
		},
	},
	{
		// The international route run backwards, so the reversed-direction
		// case is present at more than one route and is not an artefact of
		// one city pair.
		ID: "sin-bom-thu-pm-3", Route: "SIN->BOM", Day: "Thursday", TimeOfDay: "night", Passengers: 3,
		Phrasings: []string{
			"SIN to BOM Thursday night for three",
			"Singapore to Mumbai, Thursday PM, 3 passengers",
			"book three Thursday night seats from Singapore to Mumbai",
			"three seats Singapore to Bombay, Thursday after 9pm",
		},
	},
}

// contrast names two itineraries that must never be confused, and says what
// separates them. These are the hard negatives: a system that deduplicates by
// meaning has to be sensitive enough to keep them apart, and the attribute that
// differs is often a single word.
type contrast struct {
	A, B   string
	Reason string

	// Easy marks a contrast between two trips that share almost no wording.
	// It is kept in the corpus because a real workload contains plenty of
	// them, but it is excluded from the hard-negative precision figure.
	Easy bool
}

var contrasts = []contrast{
	// Hard: one named attribute moves and everything else stays put.
	{A: "blr-del-fri-am-1", B: "blr-del-fri-pm-1", Reason: "same route and day, different time of day"},
	{A: "blr-del-fri-am-1", B: "blr-bom-fri-am-1", Reason: "same origin and date, different destination"},
	{A: "blr-del-fri-am-1", B: "blr-del-sat-am-1", Reason: "same route and time, different date"},
	{A: "blr-del-fri-am-1", B: "blr-del-fri-am-2", Reason: "same flight, different passenger count"},
	{A: "blr-del-fri-am-1", B: "del-blr-fri-am-1", Reason: "same cities and date, reversed direction"},
	{A: "blr-del-fri-pm-1", B: "blr-del-sat-am-1", Reason: "same route, different date and time of day"},
	{A: "blr-bom-fri-am-1", B: "blr-del-sat-am-1", Reason: "same origin, different destination and date"},
	{A: "blr-del-fri-am-2", B: "blr-del-sat-am-1", Reason: "same route, different passenger count and date"},
	{A: "blr-bom-fri-am-1", B: "blr-del-fri-pm-1", Reason: "same origin and date, different destination and time of day"},
	{A: "del-blr-fri-am-1", B: "blr-del-sat-am-1", Reason: "same cities, reversed direction and different date"},
	{A: "maa-hyd-mon-am-1", B: "hyd-maa-mon-am-1", Reason: "same cities and date, reversed direction"},

	// Hard, and added because the fixture's own trip needed neighbours it
	// could realistically be confused with.
	{A: "blr-del-fri-am-1", B: "del-blr-sun-pm-1", Reason: "the return leg of the same trip: reversed direction, different day"},
	{A: "blr-del-fri-am-1", B: "blr-del-nextfri-am-1", Reason: "same route, same weekday, same time — one week later"},
	{A: "blr-del-sat-am-1", B: "blr-del-nextfri-am-1", Reason: "same route and time, different date"},
	{A: "blr-del-fri-pm-1", B: "blr-del-nextfri-am-1", Reason: "same route, a week apart and a different time of day"},
	{A: "del-blr-fri-am-1", B: "del-blr-sun-pm-1", Reason: "same route and direction, different day and time of day"},
	{A: "bom-sin-thu-pm-3", B: "sin-bom-thu-pm-3", Reason: "same international route and date, reversed direction"},
	{A: "blr-bom-fri-am-1", B: "blr-del-fri-am-2", Reason: "same origin and date, different destination and passenger count"},

	// Easy: two trips with almost no wording in common. Kept because a real
	// workload is full of them, excluded from the hard-negative precision.
	{A: "maa-hyd-mon-am-1", B: "ccu-goi-sun-pm-2", Reason: "unrelated trip", Easy: true},
	{A: "ccu-goi-sun-pm-2", B: "bom-sin-thu-pm-3", Reason: "unrelated trip, different passenger count", Easy: true},
	{A: "pnq-dxb-wed-am-1", B: "bom-sin-thu-pm-3", Reason: "unrelated international trip", Easy: true},
	{A: "blr-del-fri-am-1", B: "maa-hyd-mon-am-1", Reason: "unrelated trip", Easy: true},
	{A: "blr-del-fri-am-1", B: "pnq-dxb-wed-am-1", Reason: "unrelated trip, domestic versus international", Easy: true},
	{A: "ccu-goi-sun-pm-2", B: "blr-del-fri-am-2", Reason: "unrelated trip, different passenger count", Easy: true},
}

// nearMisses are the corpus's hardest negatives, written out as explicit pairs
// rather than generated from the itinerary table.
//
// Every one of them is two requests that share nearly all their words and
// still mean two different seats. They are written by hand because the
// itinerary cross-product cannot produce them: crossing two itineraries also
// crosses two phrasings, so several words move at once and the pair gets
// easier. Here exactly one thing moves — a weekday, a direction, a digit, the
// word "next" — which is the shape a real planner's mistake takes.
//
// They cost nothing in duplicates. Adding another itinerary to get one more
// hard negative would also have added six more paraphrase duplicates that say
// nothing new, which is the padding the brief warns against.
var nearMisses = []contrast{
	{A: "BLR to DEL Friday morning", B: "BLR to DEL Friday afternoon", Reason: "same route and date, the time of day moves"},
	{A: "BLR to DEL Friday morning", B: "BLR to DEL Saturday morning", Reason: "same route and time, the date moves"},
	{A: "BLR to DEL Friday morning", B: "BLR to BOM Friday morning", Reason: "same date and time, the destination code moves"},
	{A: "BLR to DEL Friday morning", B: "DEL to BLR Friday morning", Reason: "identical word set, the direction is reversed"},
	{A: "Bangalore to Delhi, Friday AM", B: "Delhi to Bangalore, Friday AM", Reason: "identical word set, the direction is reversed"},
	{A: "BLR to DEL Friday morning", B: "BLR to DEL next Friday morning", Reason: "same route, weekday and time, one week apart"},
	{A: "BLR to DEL Friday morning", B: "BLR to DEL Friday morning returning Sunday", Reason: "one way versus a return trip"},
	{A: "BLR to DEL Saturday morning", B: "BLR to DEL Sunday morning", Reason: "same route, consecutive days"},
	{A: "one seat Bengaluru to New Delhi, Friday before noon", B: "two seats Bengaluru to New Delhi, Friday before noon", Reason: "same flight, the passenger count moves"},
	{A: "one seat Bengaluru to New Delhi, Friday before noon", B: "one seat Bengaluru to Mumbai, Friday before noon", Reason: "same date and time, the destination city moves"},
	{A: "book a Friday morning flight from Bangalore to Delhi", B: "book a Friday evening flight from Bangalore to Delhi", Reason: "same route and date, the time of day moves"},
	{A: "DEL to BLR Friday morning", B: "DEL to BLR Friday evening", Reason: "same route and date, the time of day moves"},
	{A: "MAA to HYD Monday morning", B: "MAA to HYD Tuesday morning", Reason: "same route, consecutive weekdays"},
	{A: "Chennai to Hyderabad, Monday AM", B: "Hyderabad to Chennai, Monday AM", Reason: "identical word set, the direction is reversed"},
	{A: "BOM to SIN Thursday night", B: "SIN to BOM Thursday night", Reason: "identical word set, the direction is reversed"},
	{A: "PNQ to DXB Wednesday morning", B: "PNQ to DXB Wednesday evening", Reason: "same route and date, the time of day moves"},
	{A: "CCU to GOI Sunday evening for two", B: "CCU to GOI Sunday evening for three", Reason: "same flight, one digit of the passenger count"},
	{A: "Kolkata to Goa, Sunday PM, 2 passengers", B: "Kolkata to Goa, Saturday PM, 2 passengers", Reason: "same route and party, the date moves"},
	{A: "Mumbai to Singapore, Thursday PM, 3 passengers", B: "Mumbai to Singapore, Thursday PM, 2 passengers", Reason: "same flight, one digit of the passenger count"},
}

// surfaceVariant rewrites a phrasing so it means exactly the same thing but is
// spelled differently: different case, padded and doubled spaces.
//
// These pairs are the harness's own control. agent.CanonicalIntent collapses
// case and whitespace before anything is embedded, so both sides become the
// identical string and EVERY arm, including exact matching, must call them
// duplicates with a similarity of exactly 1.0. An arm that misses one of these
// is broken, not merely inaccurate.
func surfaceVariant(s string) string {
	return "  " + strings.ToUpper(strings.ReplaceAll(s, " ", "  ")) + " "
}

// Corpus builds the full labelled set. It is deterministic: the same pairs in
// the same order on every run, so two runs of the harness are comparable.
func Corpus() ([]Pair, error) {
	if err := mustMatchFixture(); err != nil {
		return nil, err
	}

	byID := make(map[string]Itinerary, len(itineraries))
	for _, it := range itineraries {
		byID[it.ID] = it
	}

	var pairs []Pair

	// 1. The fixture pair, first, because it is the scenario the whole
	//    evaluation rests on. It is also generated below as an ordinary
	//    paraphrase pair; it is lifted out here and skipped there so it can be
	//    reported on its own line.
	pairs = append(pairs, Pair{
		ID:       "fixture-double-booking",
		Category: "fixture",
		Note:     "app/agent DoubleBooking: the planner reworded the booking after a crash",
		A:        agent.IntentBeforeCrash,
		B:        agent.IntentAfterCrash,
		Truth:    Duplicate,
	})

	// 2. Surface-only duplicates: the control described on surfaceVariant.
	for _, it := range itineraries {
		pairs = append(pairs, Pair{
			ID:       "surface-" + it.ID,
			Category: "surface",
			Note:     "identical words, different case and spacing",
			A:        it.Phrasings[0],
			B:        surfaceVariant(it.Phrasings[0]),
			Truth:    Duplicate,
		})
	}

	// 3. Paraphrase duplicates: every unordered pair of phrasings within one
	//    itinerary. These are the pairs that need real meaning comparison —
	//    no amount of normalisation turns "BLR to DEL Friday morning" into
	//    "Bangalore to Delhi, Friday AM".
	for _, it := range itineraries {
		for i := 0; i < len(it.Phrasings); i++ {
			for j := i + 1; j < len(it.Phrasings); j++ {
				if it.Phrasings[i] == agent.IntentBeforeCrash && it.Phrasings[j] == agent.IntentAfterCrash {
					continue // already emitted as the fixture pair
				}
				pairs = append(pairs, Pair{
					ID:       fmt.Sprintf("para-%s-%d-%d", it.ID, i, j),
					Category: "paraphrase",
					Note:     "same itinerary (" + it.Route + " " + it.Day + " " + it.TimeOfDay + "), reworded",
					A:        it.Phrasings[i],
					B:        it.Phrasings[j],
					Truth:    Duplicate,
				})
			}
		}
	}

	// 4. Distinct pairs, taken phrasing-for-phrasing across a contrast so that
	//    the two sides are written in the same style. That is deliberate: it
	//    removes style as a confound and leaves the difference in meaning as
	//    the only thing telling them apart, which is the hardest case.
	for _, c := range contrasts {
		a, ok := byID[c.A]
		if !ok {
			return nil, fmt.Errorf("corpus: contrast references unknown itinerary %q", c.A)
		}
		b, ok := byID[c.B]
		if !ok {
			return nil, fmt.Errorf("corpus: contrast references unknown itinerary %q", c.B)
		}
		n := len(a.Phrasings)
		if len(b.Phrasings) < n {
			n = len(b.Phrasings)
		}
		for i := 0; i < n; i++ {
			pairs = append(pairs, Pair{
				ID:       fmt.Sprintf("contrast-%s-vs-%s-%d", a.ID, b.ID, i),
				Category: "contrast",
				Hard:     !c.Easy,
				Note:     c.Reason,
				A:        a.Phrasings[i],
				B:        b.Phrasings[i],
				Truth:    Distinct,
			})
		}
	}

	// 5. Near misses: the hardest negatives, written out as explicit pairs.
	//    These are what a precision number is worth reading for.
	for i, n := range nearMisses {
		pairs = append(pairs, Pair{
			ID:       fmt.Sprintf("nearmiss-%02d", i),
			Category: "near-miss",
			Hard:     true,
			Note:     n.Reason,
			A:        n.A,
			B:        n.B,
			Truth:    Distinct,
		})
	}

	if err := validate(pairs); err != nil {
		return nil, err
	}
	return pairs, nil
}

// wordOverlap is the Jaccard overlap of two requests' word sets after
// canonicalisation and after punctuation is stripped.
//
// It measures how much of the surface the two sides share, which is exactly
// what "hard negative" means here. Punctuation is dropped because "Delhi," and
// "Delhi" are the same word to a reader and their difference is not the kind
// of difference the pair is about.
func wordOverlap(a, b string) float64 {
	words := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.Fields(agent.CanonicalIntent(s)) {
			w = strings.TrimFunc(w, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsDigit(r)
			})
			if w != "" {
				out[w] = true
			}
		}
		return out
	}
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	shared := 0
	for w := range wa {
		if wb[w] {
			shared++
		}
	}
	union := len(wa) + len(wb) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// MinNearMissOverlap is the share of words a near-miss pair must have in
// common. Below it the pair is not a near miss and does not belong in the
// category, however different its two meanings are.
const MinNearMissOverlap = 0.5

// mustMatchFixture keeps the corpus honest about its anchor. The first
// itinerary is supposed to BE the DoubleBooking scenario, so if those constants
// ever change in app/agent and this file does not, the harness must refuse to
// run rather than quietly measure a scenario that no longer exists.
func mustMatchFixture() error {
	first := itineraries[0]
	want := map[string]bool{agent.IntentBeforeCrash: false, agent.IntentAfterCrash: false}
	for _, p := range first.Phrasings {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for phrase, found := range want {
		if !found {
			return fmt.Errorf("corpus: itinerary %q no longer contains the fixture phrasing %q; "+
				"app/agent's DoubleBooking constants have changed and the corpus must be updated",
				first.ID, phrase)
		}
	}
	// The fixture is only interesting if exact matching genuinely fails on it.
	if agent.IntentBeforeCrash == agent.IntentAfterCrash {
		return fmt.Errorf("corpus: the fixture's two phrasings are identical, so it no longer " +
			"demonstrates anything an exact key would miss")
	}
	if agent.CanonicalIntent(agent.IntentBeforeCrash) == agent.CanonicalIntent(agent.IntentAfterCrash) {
		return fmt.Errorf("corpus: the fixture's two phrasings are equal after canonicalisation, " +
			"so cheap normalisation would catch the repeat and no embedding is needed")
	}
	return nil
}

// validate catches the mistakes that would silently corrupt the measurement:
// a duplicated id, an empty side, or a pair labelled duplicate whose two sides
// are the same string after canonicalisation while claiming to be a paraphrase.
func validate(pairs []Pair) error {
	seen := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		if seen[p.ID] {
			return fmt.Errorf("corpus: duplicate pair id %q", p.ID)
		}
		seen[p.ID] = true

		if strings.TrimSpace(p.A) == "" || strings.TrimSpace(p.B) == "" {
			return fmt.Errorf("corpus: pair %q has an empty side", p.ID)
		}
		if p.Truth == Distinct && agent.CanonicalIntent(p.A) == agent.CanonicalIntent(p.B) {
			return fmt.Errorf("corpus: pair %q is labelled DISTINCT but its two sides are "+
				"identical after canonicalisation", p.ID)
		}
		if p.Category == "paraphrase" && agent.CanonicalIntent(p.A) == agent.CanonicalIntent(p.B) {
			return fmt.Errorf("corpus: pair %q claims to be a paraphrase but normalisation "+
				"alone makes the two sides equal", p.ID)
		}
		// A near miss that does not actually share most of its words is just
		// an ordinary negative wearing the wrong label, and would quietly
		// make the hard-negative precision figure look better than it is.
		if p.Category == "near-miss" {
			if ov := wordOverlap(p.A, p.B); ov < MinNearMissOverlap {
				return fmt.Errorf("corpus: pair %q is filed as a near miss but its two sides "+
					"share only %.0f%% of their words (want at least %.0f%%); it is not a "+
					"hard negative", p.ID, 100*ov, 100*MinNearMissOverlap)
			}
		}
		if p.Hard && p.Truth == Duplicate {
			return fmt.Errorf("corpus: pair %q is marked Hard but labelled DUPLICATE; "+
				"Hard describes a negative that is difficult to keep apart", p.ID)
		}
	}
	return nil
}

// CorpusSummary counts the corpus by label and category, for the run header.
type CorpusSummary struct {
	Total      int
	Duplicates int
	Distincts  int
	// HardDistincts is how many of the negatives are lexically close to their
	// partner. It is reported on its own because precision over the easy ones
	// is close to free.
	HardDistincts int
	ByCategory    map[string]int
}

func Summarize(pairs []Pair) CorpusSummary {
	s := CorpusSummary{ByCategory: map[string]int{}}
	for _, p := range pairs {
		s.Total++
		if bool(p.Truth) {
			s.Duplicates++
		} else {
			s.Distincts++
			if p.Hard {
				s.HardDistincts++
			}
		}
		s.ByCategory[p.Category]++
	}
	return s
}
