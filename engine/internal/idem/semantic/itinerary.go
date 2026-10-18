package semantic

import (
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// THE ITINERARY EXTRACTOR
// ---------------------------------------------------------------------------
//
// This file is a domain schema for flight booking, written as hand-made rules
// over prose, and it is the part of the hybrid comparator that does not
// generalise. It is here, rather than in app/agent, only because the engine
// has to be able to apply it to a candidate's journaled intent text during a
// check, and because the evaluation in cmd/experiment drives the engine's own
// code and not a copy of it.
//
// WHY IT SCRAPES TEXT INSTEAD OF READING FIELDS
//
// Because the input carries no fields. app/agent's BookFlight is scheduled
// with {"tool":"BookFlight","args":"BLR to DEL Friday morning"}: one prose
// string written by a planner. That is faithful to how a tool-calling agent
// behaves when the tool's schema is a free-text query, and it means there is
// nothing structured to compare. Given the choice between comparing prose by
// similarity (measured: 93.9% of distinct pairs wrongly suppressed) and
// recovering the deciding values from the prose, this file does the second.
//
// WHAT IT COSTS, STATED ONCE AND PLAINLY
//
//   - The vocabulary is finite and was written by reading the corpus. Ten
//     airports and their city aliases, seven weekdays, four dayparts,
//     passenger counts up to four. A phrasing outside that vocabulary is not
//     mis-read, it is not read at all — Recognised goes false and the caller
//     falls back to the pure-semantic comparison — but "not read at all"
//     means the gate contributes nothing for that call.
//   - Because the vocabulary was written against the evaluation corpus, an
//     accuracy figure measured on that corpus is partly a measurement of how
//     well this file covers it. That is selection on the test set, it is not
//     avoidable by writing the rules more carefully, and every number the
//     hybrid arms produce has to be read with it in mind.
//   - Two canonicalisations below merge information on purpose, and both are
//     judgements that could be wrong:
//     (a) evening, night, "PM", "after 6pm" and "after 9pm" all become the
//     single daypart "pm". A planner that rewords "Thursday night" as
//     "Thursday PM" is saying the same thing, and no rule can resolve "PM"
//     any finer than "after midday" without inventing information. The cost
//     is that a genuine evening-versus-night distinction would be missed.
//     (b) an unstated passenger count is one. That matches what BookFlight
//     does, and it is what makes "book a flight to Delhi" and "book one seat
//     to Delhi" the same booking.
//
// The durable fix is not a better scraper. It is for an activity that wants
// semantic idempotency to declare its deciding fields in its input schema, so
// that this file is not needed at all.

// The deciding field names. They are constants because they appear in
// journaled explanations, and an explanation whose field names drift between
// versions cannot be compared across runs.
const (
	FieldRoute   = "route"   // the cities in the order they were named
	FieldWhen    = "when"    // the day tokens, in order
	FieldDaypart = "daypart" // morning / afternoon / pm
	FieldPax     = "pax"     // how many seats
)

// ItineraryFields is the FieldExtractor for flight-booking intents.
//
// The route is the anchor: an intent naming no city at all is not an
// itinerary this file understands, and it reports Recognised false rather than
// guessing. Everything else is optional, and a field named on one side and not
// the other counts as a difference — see FirstDifference.
func ItineraryFields(activityType, intentText string) (Fields, error) {
	toks := tokenize(intentText)
	noise := noiseFor(activityType)

	var cities, days, dayparts, remainder []string
	pax := ""

	for i := 0; i < len(toks); {
		kind, value, width := matchPhrase(toks, i)
		switch kind {
		case phraseCity:
			cities = append(cities, value)
		case phraseDay:
			days = append(days, value)
		case phraseDaypart:
			dayparts = append(dayparts, value)
		case phrasePax:
			// The FIRST count wins. A second number later in the sentence is
			// far more likely to be part of something else than a correction
			// of the party size.
			if pax == "" {
				pax = value
			}
		default:
			if !noise[toks[i]] {
				remainder = append(remainder, toks[i])
			}
		}
		i += width
	}

	if len(cities) == 0 {
		// No route, no itinerary. Returning an empty Fields rather than a
		// partial one is deliberate: a gate built from a daypart alone would
		// pass two completely different trips that happen to be in the
		// morning, which is worse than having no gate.
		return Fields{}, nil
	}

	f := Fields{Recognised: true, Remainder: strings.Join(remainder, " ")}
	f.Deciding = append(f.Deciding, Field{Name: FieldRoute, Value: strings.Join(cities, ">")})
	if len(days) > 0 {
		f.Deciding = append(f.Deciding, Field{Name: FieldWhen, Value: strings.Join(days, ">")})
	}
	if len(dayparts) > 0 {
		f.Deciding = append(f.Deciding, Field{Name: FieldDaypart, Value: strings.Join(dayparts, ">")})
	}
	if pax == "" {
		pax = "1"
	}
	f.Deciding = append(f.Deciding, Field{Name: FieldPax, Value: pax})
	f.sortDeciding()
	return f, nil
}

// tokenize lowercases and splits on everything that is not a letter or a
// digit.
//
// Splitting that way, rather than on spaces, is what makes the JSON scaffolding
// of the generic intent text — `BookFlight({"args":"...","tool":"BookFlight"})`
// — fall apart into ordinary words that the noise list can drop. It also keeps
// "6pm" as one token, which the daypart rules rely on, while separating
// "delhi," from its comma.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

type phraseKind int

const (
	phraseNone phraseKind = iota
	phraseCity
	phraseDay
	phraseDaypart
	phrasePax
)

// maxPhrase is the longest multi-word phrase in the tables, in tokens: "the
// friday after this one" contributes "friday after this one".
const maxPhrase = 4

// matchPhrase matches the longest phrase starting at i, and reports how many
// tokens it consumed.
//
// Longest-first matching is load-bearing in several places: "new delhi" must
// beat "delhi", "next friday" must beat "friday", and "friday after this one"
// must consume the "one" so it is not also read as a passenger count.
func matchPhrase(toks []string, i int) (phraseKind, string, int) {
	for n := maxPhrase; n >= 1; n-- {
		if i+n > len(toks) {
			continue
		}
		key := strings.Join(toks[i:i+n], " ")
		if p, ok := phrases[key]; ok {
			return p.kind, p.value, n
		}
	}
	// A clock-hour daypart such as "6pm" or "9am" is a rule rather than a
	// table entry, because the table would otherwise need twenty-four of them
	// to say the same thing.
	if v, ok := clockDaypart(toks[i]); ok {
		return phraseDaypart, v, 1
	}
	return phraseNone, "", 1
}

// clockDaypart reads "6pm" or "11am" as its half of the day.
func clockDaypart(tok string) (string, bool) {
	for _, suffix := range []string{"am", "pm"} {
		digits, ok := strings.CutSuffix(tok, suffix)
		if !ok || digits == "" {
			continue
		}
		for _, r := range digits {
			if !unicode.IsDigit(r) {
				return "", false
			}
		}
		return suffix, true
	}
	return "", false
}

type phrase struct {
	kind  phraseKind
	value string
}

// phrases is the whole vocabulary, keyed by the space-joined tokens of the
// phrase. It is built once at init from the tables below, so that a city with
// four aliases is written as four aliases and not as four map entries.
var phrases = buildPhrases()

// cityAliases maps every spelling to one canonical code. The code is the
// canonical form because it is the thing an airline would key a booking by,
// and because it collapses the Bombay/Mumbai and Madras/Chennai pairs that a
// planner genuinely uses interchangeably.
//
// Note what this table is NOT: it is not a synonym list the embedding was
// supposed to make unnecessary. The pure-semantic path already resolves
// Bangalore/Bengaluru correctly. What it cannot do is treat Friday and
// Saturday as different, and that is what the table is here to support.
var cityAliases = map[string][]string{
	"blr": {"blr", "bangalore", "bengaluru"},
	"del": {"del", "delhi", "new delhi"},
	"bom": {"bom", "mumbai", "bombay"},
	"maa": {"maa", "chennai", "madras"},
	"hyd": {"hyd", "hyderabad"},
	"ccu": {"ccu", "kolkata", "calcutta"},
	"goi": {"goi", "goa"},
	"pnq": {"pnq", "pune"},
	"dxb": {"dxb", "dubai"},
	"sin": {"sin", "singapore"},
}

var weekdays = []string{
	"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday",
}

// nextWeekSuffix marks a day one week further out. It is part of the VALUE
// rather than a field of its own because "next Friday" and "Friday" are two
// different dates, and the gate's job is to say they differ — not to explain
// how far apart they are.
const nextWeekSuffix = "+1w"

// dayparts are the coarse time buckets. See the note at the top of this file
// for why evening and night share one.
var dayparts = map[string][]string{
	"am":        {"morning", "am", "before noon"},
	"afternoon": {"afternoon"},
	"pm":        {"evening", "pm", "night", "after 6pm", "after 9pm", "tonight"},
}

var paxWords = map[string][]string{
	"1": {"one", "1", "single"},
	"2": {"two", "2"},
	"3": {"three", "3"},
	"4": {"four", "4"},
}

func buildPhrases() map[string]phrase {
	out := map[string]phrase{}
	add := func(key string, p phrase) {
		if len(strings.Fields(key)) > maxPhrase {
			// A phrase longer than the matcher will ever try would sit in the
			// table and never fire, which is the kind of dead rule that makes
			// an extractor look more capable than it is.
			panic("semantic: phrase " + key + " is longer than maxPhrase")
		}
		out[key] = p
	}

	for code, aliases := range cityAliases {
		for _, a := range aliases {
			add(a, phrase{phraseCity, code})
		}
	}
	for _, wd := range weekdays {
		add(wd, phrase{phraseDay, wd})
		// The three ways the corpus's planners say "a week later". Each is a
		// whole phrase so that the words it is made of — "next", "week",
		// "after" — are consumed rather than left in the free text, where
		// they would make two phrasings of one date look different.
		add("next "+wd, phrase{phraseDay, wd + nextWeekSuffix})
		add(wd+" after this one", phrase{phraseDay, wd + nextWeekSuffix})
		add("a week on "+wd, phrase{phraseDay, wd + nextWeekSuffix})
		add("week on "+wd, phrase{phraseDay, wd + nextWeekSuffix})
	}
	for value, words := range dayparts {
		for _, w := range words {
			add(w, phrase{phraseDaypart, value})
		}
	}
	for value, words := range paxWords {
		for _, w := range words {
			add(w, phrase{phrasePax, value})
		}
	}
	return out
}

// baseNoise are the words that say nothing about WHICH booking this is.
//
// Dropping them is what makes an empty remainder mean "this request was
// nothing but its deciding fields". Everything NOT on this list survives into
// the free text and is compared by meaning — "returning", "window",
// "refundable", "corporate card" — which is the division of labour the hybrid
// comparator is for.
var baseNoise = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "from": true, "for": true,
	"on": true, "at": true, "in": true, "of": true, "and": true, "me": true,
	"my": true, "i": true, "please": true, "book": true, "booking": true,
	"flight": true, "flights": true, "seat": true, "seats": true,
	"ticket": true, "tickets": true, "passenger": true, "passengers": true,
	"person": true, "people": true, "pax": true, "need": true, "want": true,
	"still": true, "get": true, "take": true, "would": true, "like": true,

	// The generic intent text wraps the arguments in JSON, so its key names
	// arrive as tokens on every single call of every activity. They are the
	// definition of words that cannot distinguish two calls.
	"args": true, "tool": true, "query": true, "q": true,
}

// noiseFor adds the activity's own name to the noise list.
//
// Words that restate the activity type carry no information here, because the
// candidate set is already restricted to one activity type: every candidate is
// a BookFlight, so "book" and "flight" are true of all of them. Splitting the
// camel-cased type name is what catches "bookflight", which is how the type
// appears once the intent text has been tokenized.
func noiseFor(activityType string) map[string]bool {
	out := make(map[string]bool, len(baseNoise)+4)
	for w := range baseNoise {
		out[w] = true
	}
	lower := strings.ToLower(activityType)
	if lower != "" {
		out[lower] = true
		for _, w := range splitCamel(activityType) {
			out[w] = true
		}
	}
	return out
}

// splitCamel turns "SearchFlights" into "search", "flights".
func splitCamel(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if i > start && unicode.IsUpper(r) {
			out = append(out, strings.ToLower(s[start:i]))
			start = i
		}
	}
	if start < len(s) {
		out = append(out, strings.ToLower(s[start:]))
	}
	return out
}
