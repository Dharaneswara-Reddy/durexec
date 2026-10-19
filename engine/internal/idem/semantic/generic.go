package semantic

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// THE GENERIC VALUE EXTRACTOR
// ---------------------------------------------------------------------------
//
// itinerary.go recovers deciding fields from prose by KNOWING THE WORDS: ten
// airports, seven weekdays, three dayparts. That cannot be the answer. The
// vocabulary was written by reading the corpus it is then scored on, which is
// selection on the test set; and it works for flight booking and for nothing
// else, so each new domain would need a new table written by the same
// circular method.
//
// This file is the alternative. It finds deciding fields BY THEIR FORM and
// never by a list of known words.
//
// # Why form is enough to try
//
// A rephrasing changes the WORDS and keeps the VALUES. Compare
//
//	"Book BLR to DEL Friday for 2"
//	"Reserve two seats, Bangalore to Delhi, this Friday"
//
// Almost no prose survives. The quantity survives (2, "two"), the day
// survives (Friday), the two places survive. That is not a fact about
// flights: a payment keeps its amount and its payee, a refund keeps its order
// number, a meeting keeps its date and its hour. Deciding fields are
// overwhelmingly value-like tokens, and value-like tokens have shapes —
// digits, currency markers, date formats, clock formats, code formats,
// addresses. Shapes need no vocabulary.
//
// # The one heuristic part, and why it is kept conservative
//
// Proper nouns have no shape in English except CAPITALISATION, which is a
// convention rather than a format. So the proper-noun rule is the only
// genuinely heuristic rule here, and it is deliberately the narrowest one
// that is any use: a token is a name only if it begins with an upper-case
// letter, is otherwise alphabetic, is at least two characters, is NOT the
// first token of a sentence, and is not one of a handful of function words
// that are routinely capitalised anyway. Sentence-initial tokens are excluded
// because "Book a flight" and "Send $500" begin with a verb, and reading
// those verbs as deciding values would make two phrasings of one action
// differ on the first word — the worst possible failure, since it closes the
// gate on the very pairs the gate exists to pass.
//
// The cost of that narrowness is stated rather than hidden, because it is
// large on this engine: app/agent renders its tool calls through
// agent.CanonicalIntent, which LOWERCASES. By the time an intent text reaches
// any FieldExtractor on that path there is no capitalisation left, so the
// proper-noun rule contributes exactly nothing and every name falls into the
// free text. That is a property of the activity's intent renderer, not of
// this file, and cmd/fieldeval measures both: the engine's own lowercased
// path, and the same extractor on case-preserving text, so the difference is
// a number instead of an opinion.
//
// # What is a closed class and what would be a vocabulary
//
// Three small tables appear below: currency markers, month names and weekday
// names, plus the spelled-out number words. They are closed classes of the
// language — finite, fixed, and the same in every domain — and the brief for
// this extractor names them explicitly ("money amounts with any currency
// marker", "dates and days in any common form"). They are not a domain
// vocabulary: nothing here knows an airport, a product, a payee or a reason
// code, and adding a domain would add no entry to any table in this file.
// That is the test to apply to any future edit.
//
// # Normalisation is the whole game
//
// If "two" and "2" extract differently, or "$500" and "500 dollars", or
// "Friday" and "friday", then a rephrasing that merely spells a value another
// way is read as a different action and the gate closes on a true duplicate.
// Every value this file produces is therefore canonicalised to one token, and
// generic_test.go table-tests each normalisation against the spellings a
// planner actually produces.
//
// # What it will get wrong, before any measurement
//
// Two failure modes are inherent and are reported rather than tuned away:
//
//   - OVER-EXTRACTION. A number that decides nothing — a flight number in a
//     transcript, a street number in an address, a percentage in a note —
//     becomes a deciding field, and the gate then closes on two phrasings of
//     one action because only one of them mentioned it. Absent-versus-present
//     counts as a difference by design (see FirstDifference), so this shows
//     up as missed duplicates, which is the safe direction and a real cost.
//   - UNDER-EXTRACTION. A value carried by prose alone — a place in lower
//     case, a person's name, "the usual amount" — is invisible here and lands
//     in the free text, where similarity decides it. That is not silent: it
//     is exactly the division of labour the hybrid comparator is for, and it
//     is why this extractor consults the embedding model on corpora where the
//     hand-written one never had to.

// The generic deciding-field names. Short, lower-case, and constant because
// they are written into journaled explanations.
//
// Values of the same name found more than once are joined with ">" IN ORDER OF
// APPEARANCE. One rule, applied to every field: "Bangalore to Delhi" and
// "Delhi to Bangalore" therefore differ, which is correct and necessary, and
// "pay 500 for invoice 88" and "for invoice 88 pay 500" also differ, which is
// not. The second is a measured cost of the first; the alternative, sorting
// the values into a bag, would make a reversed route look like the same trip
// and lose a booking, and losing a booking is the failure this whole mechanism
// exists to prevent.
const (
	// GenericFieldNum is a cardinal number that is not an amount of money, a
	// date, a time, a phone number or part of a code.
	GenericFieldNum = "num"

	// GenericFieldMoney is the numeric part of an amount, and
	// GenericFieldCurrency the marker that came with it. They are two fields
	// because "500 USD" and "500 INR" are different amounts of money, and a
	// single merged token would make the currency unreadable in an
	// explanation.
	GenericFieldMoney    = "money"
	GenericFieldCurrency = "currency"

	// GenericFieldDate is a calendar date or a named day, canonicalised.
	GenericFieldDate = "date"

	// GenericFieldClock is a wall-clock time, canonicalised to 24-hour HH:MM.
	GenericFieldClock = "time"

	// GenericFieldCode is an identifier: anything shaped like a reference
	// rather than a quantity.
	GenericFieldCode = "id"

	GenericFieldEmail = "email"
	GenericFieldPhone = "phone"

	// GenericFieldName is the capitalisation heuristic's output. See the
	// header for why it is the only heuristic here and why it is narrow.
	GenericFieldName = "name"
)

// GenericFields is a FieldExtractor that needs no domain vocabulary.
//
// It is pure and deterministic: the same text yields the same fields in the
// same order on every node and in every process, which is what
// FieldExtractor's contract requires and what makes a journaled explanation
// reproducible from the log.
//
// Recognised is false only when NOTHING value-like was found. That condition
// is load-bearing: a Fields with zero deciding fields would make every call of
// an activity match every other call of it, because FirstDifference has
// nothing to disagree about. Returning Recognised false instead routes the
// call to the pure-semantic comparison and says so in the verdict.
func GenericFields(activityType, intentText string) (Fields, error) {
	toks := genericTokens(intentText)
	noise := genericNoiseFor(activityType)
	valueNoise := genericValueNoise(activityType)

	// found collects values per field name, in order of appearance, with
	// EXACT REPEATS DROPPED.
	//
	// Dropping repeats is not tidying, it is a correctness fix the live
	// crash-and-rephrase run paid for. The hand-written extractor read
	// "Friday morning, AM" as daypart "am>am" and the reworded "Friday AM" as
	// daypart "am", and the gate then called one booking two bookings because
	// one sentence had said the same thing twice. A value restated is still
	// one value. Two DIFFERENT values of one field are still kept, in order,
	// because "Bangalore to Delhi" and "Delhi to Bangalore" are two trips.
	found := map[string][]string{}
	add := func(name, value string) {
		if value == "" {
			return
		}
		if valueNoise[value] {
			// A VALUE THAT IS NOISE IS NOT A VALUE. The engine's intent text
			// is `ActivityType({"args":"...","tool":"ActivityType"})`, so the
			// activity's own name arrives inside the JSON as well as in
			// front of it — and the copy inside the JSON is not
			// sentence-initial, so the proper-noun rule read it as a name on
			// EVERY call of that activity. That made every intent
			// "recognised" on the strength of its own type name, which is
			// the one token that cannot distinguish two calls of it. Found
			// by reading the signatures in a first run; see the results file,
			// which reports the before and after.
			return
		}
		for _, existing := range found[name] {
			if existing == value {
				return
			}
		}
		found[name] = append(found[name], value)
	}

	var remainder []string
	for i := 0; i < len(toks); {
		kind, value, extra, width := matchValue(toks, i)
		if kind == "" {
			lower := toks[i].lower
			if !noise[lower] && lower != "" {
				remainder = append(remainder, lower)
			}
			i++
			continue
		}
		add(kind, value)
		for _, e := range extra {
			add(e.Name, e.Value)
		}
		i += width
	}

	f := Fields{Remainder: strings.Join(remainder, " ")}
	for name, values := range found {
		// VALUES OF ONE FIELD ARE SORTED, NOT LEFT IN THE ORDER THEY WERE
		// SAID.
		//
		// This is a judgement with a cost on both sides, so here is both.
		// Order of appearance is a property of the SENTENCE, not of the
		// action: "open a P1 ticket against TCK-219" and "file TCK-219 as
		// P1" name the same two values in opposite orders, and reading them
		// as two different tickets splits a true duplicate on word order
		// alone. Sorting fixes that for every field in every domain.
		//
		// What it gives up is direction. This extractor cannot tell which
		// of two places is the origin — that is exactly what it would need a
		// vocabulary for — so with the values sorted, "Bangalore to Delhi"
		// and "Delhi to Bangalore" extract identically and the gate can no
		// longer separate an outbound booking from its return leg. That is a
		// real failure and the flight domain's reversed-direction negatives
		// measure it; the report names them.
		sort.Strings(values)
		f.Deciding = append(f.Deciding, Field{Name: name, Value: strings.Join(values, ">")})
	}
	f.sortDeciding()
	f.Recognised = len(f.Deciding) > 0
	return f, nil
}

var _ FieldExtractor = GenericFields

// ---------------------------------------------------------------------------
// Tokens.
// ---------------------------------------------------------------------------

// token is one word of the intent, kept in both spellings.
//
// The original spelling is kept because the proper-noun rule is the one rule
// that reads capitalisation, and lowercasing first would destroy the only
// signal it has. Sentence reports whether this token opened a sentence, which
// is computed here, while the surrounding punctuation is still visible.
type token struct {
	raw   string
	lower string

	// SentenceStart is true for the first token of the text and for the first
	// token after a sentence-ending mark. Those tokens are capitalised by
	// convention and say nothing about whether they are names.
	SentenceStart bool
}

// genericTokens splits an intent text into tokens without destroying the
// shapes the matchers look for.
//
// It cannot use itinerary.go's tokenize, which splits on every non-alphanumeric
// rune: that would turn "2026-03-12" into three numbers, "07:40" into two,
// "alice@example.com" into three words and "$500" into a bare 500. The
// separators those forms are made of are exactly what identifies them.
//
// So the split is on whitespace, after three repairs:
//
//   - a comma BETWEEN TWO DIGITS is deleted, so "Rs 2,199" is one amount
//     rather than two numbers. Every other comma becomes a space.
//   - the JSON scaffolding the engine wraps arguments in — braces, brackets,
//     quotes, parentheses, commas, semicolons — becomes whitespace, so
//     `SendPayment({"args":"pay 500","tool":"SendPayment"})` falls apart into
//     words instead of arriving as two enormous tokens. A COLON is not on
//     that list, because "07:40" needs it; an unquoted JSON colon ends up as
//     a token of its own and is trimmed away below.
//   - leading and trailing punctuation is trimmed per token, so "Delhi," and
//     "Delhi" are one word and a sentence-final "." does not defeat a match.
//     A trailing "." is kept when digits follow it, so "500.50" survives.
func genericTokens(s string) []token {
	var b strings.Builder
	b.Grow(len(s))
	runes := []rune(s)
	for i, r := range runes {
		if r == ',' {
			// A thousands separator is deleted; any other comma is a break.
			prevDigit := i > 0 && unicode.IsDigit(runes[i-1])
			nextDigit := i+1 < len(runes) && unicode.IsDigit(runes[i+1])
			if prevDigit && nextDigit {
				continue
			}
			b.WriteRune(' ')
			continue
		}
		if strings.ContainsRune("{}[]\"()<>;|", r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}

	fields := strings.Fields(b.String())
	out := make([]token, 0, len(fields))
	sentenceStart := true
	for _, raw := range fields {
		trimmed, endsSentence := trimEdges(raw)
		if trimmed == "" {
			// Pure punctuation, such as the bare ":" a JSON key leaves
			// behind. It is not a sentence boundary and not a word.
			continue
		}
		out = append(out, token{raw: trimmed, lower: strings.ToLower(trimmed), SentenceStart: sentenceStart})
		sentenceStart = endsSentence
	}
	return out
}

// trimEdges strips punctuation from both ends of one token and reports whether
// what it stripped ended a sentence.
func trimEdges(s string) (trimmed string, endsSentence bool) {
	keep := func(r rune) bool {
		// The runes a VALUE may legitimately begin or end with. '#', '$' and
		// the currency signs lead; letters and digits do both; '%' trails.
		return unicode.IsLetter(r) || unicode.IsDigit(r) ||
			strings.ContainsRune("#$%+", r) || isCurrencySign(r)
	}
	start := 0
	for start < len(s) && !keep(rune(s[start])) && !isMultibyte(s[start]) {
		start++
	}
	end := len(s)
	for end > start {
		last := s[end-1]
		if keep(rune(last)) || isMultibyte(last) {
			break
		}
		if last == '.' || last == '!' || last == '?' {
			endsSentence = true
		}
		end--
	}
	return s[start:end], endsSentence
}

// isMultibyte reports whether a byte is part of a multi-byte rune, so the
// byte-wise trim above leaves non-ASCII values such as "€40" alone instead of
// chewing into them.
func isMultibyte(b byte) bool { return b >= 0x80 }

// ---------------------------------------------------------------------------
// The matchers, in priority order.
// ---------------------------------------------------------------------------

// extraField is a second field a matcher produces alongside its own: a money
// match also yields a currency, a clock match also yields a daypart.
type extraField struct {
	Name  string
	Value string
}

// matchValue tries every matcher at position i and returns the first that
// fires, with how many tokens it consumed.
//
// ORDER IS LOAD-BEARING and runs from most specific to least:
//
//	email   before anything, because an address contains dots and digits
//	money   before date and number, so "$500" is not the number 500 alone
//	date    before clock and code, so "2026-03-12" is not a code and
//	        "12/03" is not two numbers
//	clock   before code and number, so "07:40" is one time
//	phone   before number, so a ten-digit run is not a cardinal
//	code    before number, so "ORD-123" is not the number 123
//	number  last among the shapes
//	name    last of all, because capitalisation is the weakest signal
func matchValue(toks []token, i int) (name, value string, extra []extraField, width int) {
	if v, ok := matchEmail(toks[i]); ok {
		return GenericFieldEmail, v, nil, 1
	}
	if amount, cur, n, ok := matchMoney(toks, i); ok {
		return GenericFieldMoney, amount, []extraField{{GenericFieldCurrency, cur}}, n
	}
	if v, n, ok := matchDate(toks, i); ok {
		return GenericFieldDate, v, nil, n
	}
	if v, daypart, n, ok := matchClock(toks, i); ok {
		return GenericFieldClock, v, []extraField{{FieldDaypart, daypart}}, n
	}
	if v, n, ok := matchDaypart(toks, i); ok {
		return FieldDaypart, v, nil, n
	}
	if v, n, ok := matchPhone(toks, i); ok {
		return GenericFieldPhone, v, nil, n
	}
	if v, ok := matchCode(toks[i]); ok {
		return GenericFieldCode, v, nil, 1
	}
	if v, n, ok := matchNumber(toks, i); ok {
		return GenericFieldNum, v, nil, n
	}
	if v, ok := matchName(toks[i]); ok {
		return GenericFieldName, v, nil, 1
	}
	return "", "", nil, 0
}

// ---------------------------------------------------------------------------
// Email.
// ---------------------------------------------------------------------------

// matchEmail accepts the shape and not the grammar: something, an '@',
// something with a dot in it. A stricter parser would reject real addresses
// and gain nothing, because the only thing done with the result is comparing
// it with another one for equality.
func matchEmail(t token) (string, bool) {
	at := strings.IndexByte(t.lower, '@')
	if at <= 0 || at == len(t.lower)-1 {
		return "", false
	}
	local, host := t.lower[:at], t.lower[at+1:]
	if strings.ContainsAny(local, "@") || !strings.Contains(host, ".") {
		return "", false
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return "", false
	}
	// Lower case is the canonical form. Hosts are case-insensitive, and no
	// mail system in practice distinguishes two local parts by case, so
	// folding both is what makes "Alice@Example.COM" one value.
	return t.lower, true
}

// ---------------------------------------------------------------------------
// Money.
// ---------------------------------------------------------------------------

// currencyWords maps every marker to one canonical code.
//
// A closed class, not a vocabulary: it is the same table in every domain and
// a new domain adds nothing to it. Symbols are handled separately in
// currencyForSign.
var currencyWords = map[string]string{
	"usd": "usd", "dollar": "usd", "dollars": "usd", "us$": "usd",
	"inr": "inr", "rs": "inr", "rs.": "inr", "rupee": "inr", "rupees": "inr", "inr.": "inr",
	"eur": "eur", "euro": "eur", "euros": "eur",
	"gbp": "gbp", "pound": "gbp", "pounds": "gbp", "sterling": "gbp",
	"jpy": "jpy", "yen": "jpy",
	"aud": "aud", "cad": "cad", "chf": "chf", "sgd": "sgd", "aed": "aed", "dirham": "aed",
}

func isCurrencySign(r rune) bool {
	switch r {
	case '$', '€', '£', '¥', '₹':
		return true
	}
	return false
}

func currencyForSign(r rune) string {
	switch r {
	case '$':
		return "usd"
	case '€':
		return "eur"
	case '£':
		return "gbp"
	case '¥':
		return "jpy"
	case '₹':
		return "inr"
	}
	return ""
}

// matchMoney reads an amount and its currency as one unit, in any of the
// orders a planner writes them:
//
//	$500   €40   ₹2000   Rs2000          one token, sign or word glued on
//	$ 500   Rs 2000   USD 500            marker then amount
//	500 USD   500 dollars   40 euros     amount then marker
//	five hundred dollars                 spelled-out amount then marker
//
// A bare number with no marker anywhere is NOT money. Deciding otherwise
// would make every quantity in every domain an amount, and "2 passengers"
// would compare against "2 dollars" on the money field.
func matchMoney(toks []token, i int) (amount, currency string, width int, ok bool) {
	t := toks[i]

	// Form 1: the marker is glued to the digits, as a sign or as letters.
	if r := []rune(t.raw); len(r) > 0 && isCurrencySign(r[0]) {
		if v, ok := normaliseDecimal(string(r[1:])); ok {
			return v, currencyForSign(r[0]), 1, true
		}
	}
	for word, code := range currencyWords {
		if len(t.lower) > len(word) && strings.HasPrefix(t.lower, word) {
			if v, ok := normaliseDecimal(t.lower[len(word):]); ok {
				return v, code, 1, true
			}
		}
		if len(t.lower) > len(word) && strings.HasSuffix(t.lower, word) {
			if v, ok := normaliseDecimal(t.lower[:len(t.lower)-len(word)]); ok {
				return v, code, 1, true
			}
		}
	}

	// Form 2: a marker token, then the amount.
	if code, isMarker := currencyMarker(t); isMarker && i+1 < len(toks) {
		if v, n, ok := readCardinal(toks, i+1); ok {
			return v, code, 1 + n, true
		}
	}

	// Form 3: the amount, then a marker token.
	if v, n, ok := readCardinal(toks, i); ok && i+n < len(toks) {
		if code, isMarker := currencyMarker(toks[i+n]); isMarker {
			return v, code, n + 1, true
		}
	}
	return "", "", 0, false
}

// currencyMarker reads a standalone marker token: "USD", "Rs", "$", "€".
func currencyMarker(t token) (string, bool) {
	if code, ok := currencyWords[strings.TrimSuffix(t.lower, ".")]; ok {
		return code, true
	}
	if r := []rune(t.raw); len(r) == 1 && isCurrencySign(r[0]) {
		return currencyForSign(r[0]), true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Dates.
// ---------------------------------------------------------------------------

// monthNames is the other closed class. Three-letter abbreviations are
// included because planners use them and because truncating to three letters
// is not a rule that can be applied blindly ("march" and "mar" yes, but "may"
// is already three).
var monthNames = map[string]int{
	"january": 1, "jan": 1, "february": 2, "feb": 2, "march": 3, "mar": 3,
	"april": 4, "apr": 4, "may": 5, "june": 6, "jun": 6, "july": 7, "jul": 7,
	"august": 8, "aug": 8, "september": 9, "sep": 9, "sept": 9,
	"october": 10, "oct": 10, "november": 11, "nov": 11, "december": 12, "dec": 12,
}

// weekdayNames maps a named day and its abbreviation to one canonical token.
var weekdayNames = map[string]string{
	"monday": "monday", "mon": "monday",
	"tuesday": "tuesday", "tue": "tuesday", "tues": "tuesday",
	"wednesday": "wednesday", "wed": "wednesday",
	"thursday": "thursday", "thu": "thursday", "thur": "thursday", "thurs": "thursday",
	"friday": "friday", "fri": "friday",
	"saturday": "saturday", "sat": "saturday",
	"sunday": "sunday", "sun": "sunday",
}

// relativeWeek are the modifiers that push a named day a week out. "this" and
// "coming" do NOT: "this Friday" is the same Friday as "Friday", and reading
// it as a different date would split a true duplicate.
var relativeWeek = map[string]bool{"next": true}

// matchDate recognises a calendar date or a named day and canonicalises it.
//
// Canonical forms, chosen so two spellings of one date produce one token:
//
//	a full date          "2026-03-12"   (ISO, whatever it was written as)
//	a day and month      "03-12"        (no year stated, none invented)
//	a named day          "friday", or "friday+1w" after "next"
//	a relative day       "today", "tomorrow", "yesterday" — kept as the word
//
// A relative day is NOT resolved to a calendar date. Resolving it would need
// the current time, and a FieldExtractor that reads a clock is not pure: the
// same intent text would extract differently tomorrow, and an explanation
// recovered from the log would no longer match the one the engine reached.
//
// AMBIGUOUS NUMERIC DATES are read day-first ("12/03" is 12 March), which is
// a judgement and could be wrong for a US-written input. It is stated here
// rather than hidden because the alternative — guessing per input — would
// make "03/12" and "12/03" sometimes the same date and sometimes not.
func matchDate(toks []token, i int) (value string, width int, ok bool) {
	t := toks[i]

	// "next friday", "next monday"
	if relativeWeek[t.lower] && i+1 < len(toks) {
		if day, ok := weekdayNames[toks[i+1].lower]; ok {
			return day + nextWeekSuffix, 2, true
		}
	}
	// "this friday" / "coming friday": the same day, so the modifier is
	// consumed and only the day is kept. Leaving "this" in the free text
	// would be harmless; consuming it keeps the remainder of two phrasings
	// of one day identical, which is the point.
	if (t.lower == "this" || t.lower == "coming") && i+1 < len(toks) {
		if day, ok := weekdayNames[toks[i+1].lower]; ok {
			return day, 2, true
		}
	}
	if day, ok := weekdayNames[t.lower]; ok {
		return day, 1, true
	}
	switch t.lower {
	case "today", "tomorrow", "yesterday":
		return t.lower, 1, true
	}

	// A numeric date: 2026-03-12, 12/03/2026, 12.03, 2026/03/12.
	if v, ok := numericDate(t.lower); ok {
		return v, 1, true
	}

	// "12 march", "12th march", "12 march 2026"
	if day, ok := dayOfMonth(t.lower); ok && i+1 < len(toks) {
		if m, isMonth := monthNames[strings.TrimSuffix(toks[i+1].lower, ".")]; isMonth {
			if i+2 < len(toks) {
				if y, ok := yearNumber(toks[i+2].lower); ok {
					return fmt.Sprintf("%04d-%02d-%02d", y, m, day), 3, true
				}
			}
			return fmt.Sprintf("%02d-%02d", m, day), 2, true
		}
	}
	// "march 12", "march 12 2026"
	if m, isMonth := monthNames[strings.TrimSuffix(t.lower, ".")]; isMonth && i+1 < len(toks) {
		if day, ok := dayOfMonth(toks[i+1].lower); ok {
			if i+2 < len(toks) {
				if y, ok := yearNumber(toks[i+2].lower); ok {
					return fmt.Sprintf("%04d-%02d-%02d", y, m, day), 3, true
				}
			}
			return fmt.Sprintf("%02d-%02d", m, day), 2, true
		}
	}
	return "", 0, false
}

// numericDate reads the all-digit date forms, separated by '-', '/' or '.'.
func numericDate(s string) (string, bool) {
	sep := ""
	for _, c := range []string{"-", "/", "."} {
		if strings.Count(s, c) >= 2 || (strings.Count(s, c) == 1 && len(strings.Split(s, c)) == 2) {
			if strings.Count(s, c) > strings.Count(sep, c) || sep == "" {
				sep = c
			}
		}
	}
	if sep == "" {
		return "", false
	}
	parts := strings.Split(s, sep)
	if len(parts) != 2 && len(parts) != 3 {
		return "", false
	}
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" || len(p) > 4 {
			return "", false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", false
		}
		nums = append(nums, n)
	}

	if len(parts) == 3 {
		// Year first when the first component is four digits, otherwise
		// year last. Nothing else occurs in practice.
		if len(parts[0]) == 4 {
			y, m, d := nums[0], nums[1], nums[2]
			if !plausibleYMD(y, m, d) {
				return "", false
			}
			return fmt.Sprintf("%04d-%02d-%02d", y, m, d), true
		}
		d, m, y := nums[0], nums[1], nums[2]
		if m > 12 && d <= 12 {
			// Written month-first. Swapping is safe here because only one
			// reading is a real date.
			d, m = m, d
		}
		if y < 100 {
			y += 2000
		}
		if !plausibleYMD(y, m, d) {
			return "", false
		}
		return fmt.Sprintf("%04d-%02d-%02d", y, m, d), true
	}

	d, m := nums[0], nums[1]
	if m > 12 && d <= 12 {
		d, m = m, d
	}
	if m < 1 || m > 12 || d < 1 || d > 31 {
		return "", false
	}
	return fmt.Sprintf("%02d-%02d", m, d), true
}

func plausibleYMD(y, m, d int) bool {
	return y >= 1900 && y <= 2200 && m >= 1 && m <= 12 && d >= 1 && d <= 31
}

// dayOfMonth reads "12" or "12th" as a day number.
func dayOfMonth(s string) (int, bool) {
	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "st"), "nd"), "rd"), "th")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 31 {
		return 0, false
	}
	return n, true
}

func yearNumber(s string) (int, bool) {
	if len(s) != 4 {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1900 || n > 2200 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// Clock times and dayparts.
// ---------------------------------------------------------------------------

// daypartWords are the coarse buckets. Evening, night and "pm" share one, for
// the reason itinerary.go gives: no rule can resolve "PM" any finer than
// "after midday" without inventing information.
var daypartWords = map[string]string{
	"morning": "am", "am": "am", "a.m": "am", "a.m.": "am",
	"afternoon": "afternoon",
	"evening":   "pm", "night": "pm", "pm": "pm", "p.m": "pm", "p.m.": "pm",
	"tonight": "pm", "midnight": "pm", "noon": "noon", "midday": "noon",
}

// matchClock reads a wall-clock time and canonicalises it to 24-hour HH:MM.
//
//	"07:40"  -> 07:40      "7:40am" -> 07:40      "7:40 am" -> 07:40
//	"9am"    -> 09:00      "9 am"   -> 09:00      "21:15"   -> 21:15
//	"12am"   -> 00:00      "12pm"   -> 12:00
//
// It also yields a daypart, because a time of day implies one and a phrasing
// that says only "morning" has to be able to disagree with one that says
// "evening" even when neither names an hour. A 24-hour time with no marker
// takes its daypart from the hour, which is arithmetic rather than a guess.
//
// A BARE NUMBER IS NEVER A TIME. "book 9" is not nine o'clock; it is a nine
// of something. Requiring either a colon or an am/pm marker is what keeps
// every quantity in every domain out of this field.
func matchClock(toks []token, i int) (value, daypart string, width int, ok bool) {
	t := toks[i]

	// A marker in the NEXT token is tried first: "9 am", "7:40 pm". If the
	// single-token form ran first, "7:40" would match on its own as a
	// 24-hour time and the "am" left behind would then be read a second time
	// as a daypart — producing "am>am" on one phrasing and "am" on another,
	// which is a difference invented by the scanner rather than found in the
	// text.
	if i+1 < len(toks) {
		switch toks[i+1].lower {
		case "am", "a.m", "a.m.", "pm", "p.m", "p.m.":
			marker := strings.ReplaceAll(toks[i+1].lower, ".", "")
			if v, dp, ok := parseClock(t.lower + marker); ok {
				return v, dp, 2, true
			}
		case "o'clock", "oclock":
			if h, err := strconv.Atoi(t.lower); err == nil && h >= 0 && h <= 24 {
				return fmt.Sprintf("%02d:00", h%24), daypartForHour(h % 24), 2, true
			}
		}
	}
	// A marker glued on, or a bare HH:MM.
	if v, dp, ok := parseClock(t.lower); ok {
		return v, dp, 1, true
	}
	return "", "", 0, false
}

// parseClock reads one token as a time, or reports that it is not one.
func parseClock(s string) (value, daypart string, ok bool) {
	marker := ""
	switch {
	case strings.HasSuffix(s, "am"):
		marker, s = "am", strings.TrimSuffix(s, "am")
	case strings.HasSuffix(s, "pm"):
		marker, s = "pm", strings.TrimSuffix(s, "pm")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}

	hourStr, minStr := s, "0"
	if idx := strings.IndexByte(s, ':'); idx >= 0 {
		hourStr, minStr = s[:idx], s[idx+1:]
	} else if marker == "" {
		// No colon and no am/pm: not a time. See the note in matchClock.
		return "", "", false
	}
	if hourStr == "" || len(hourStr) > 2 || len(minStr) > 2 {
		return "", "", false
	}
	h, err := strconv.Atoi(hourStr)
	if err != nil {
		return "", "", false
	}
	m, err := strconv.Atoi(minStr)
	if err != nil || m < 0 || m > 59 {
		return "", "", false
	}

	switch marker {
	case "am":
		if h < 1 || h > 12 {
			return "", "", false
		}
		if h == 12 {
			h = 0
		}
		return fmt.Sprintf("%02d:%02d", h, m), "am", true
	case "pm":
		if h < 1 || h > 12 {
			return "", "", false
		}
		if h != 12 {
			h += 12
		}
		return fmt.Sprintf("%02d:%02d", h, m), "pm", true
	}
	if h < 0 || h > 23 {
		return "", "", false
	}
	return fmt.Sprintf("%02d:%02d", h, m), daypartForHour(h), true
}

// daypartForHour is the arithmetic a 24-hour clock already contains.
func daypartForHour(h int) string {
	switch {
	case h < 12:
		return "am"
	case h == 12:
		return "noon"
	case h < 17:
		return "afternoon"
	default:
		return "pm"
	}
}

// matchDaypart reads a named part of the day, including the two-word forms a
// planner writes ("before noon", "after 6pm" — the hour there is matched as a
// clock first, so only the word forms reach here).
func matchDaypart(toks []token, i int) (value string, width int, ok bool) {
	if i+1 < len(toks) && toks[i].lower == "before" {
		if toks[i+1].lower == "noon" || toks[i+1].lower == "midday" {
			return "am", 2, true
		}
	}
	if v, found := daypartWords[toks[i].lower]; found {
		return v, 1, true
	}
	return "", 0, false
}

// ---------------------------------------------------------------------------
// Phone numbers.
// ---------------------------------------------------------------------------

// matchPhone reads a run of digits long enough that it cannot be a quantity.
//
// Seven is the threshold: shorter runs are years, amounts and counts, and a
// subscriber number is never shorter. The value is the digits alone, so
// "+91 98765 43210", "9876543210" and "98765-43210" compare equal where they
// are the same number — except for the country code, which is kept when it is
// written, because a number with one and a number without it are not provably
// the same line.
func matchPhone(toks []token, i int) (value string, width int, ok bool) {
	// A '+'-led international run may be spread over up to three tokens.
	if strings.HasPrefix(toks[i].raw, "+") {
		digits, n := "", 0
		for j := i; j < len(toks) && j < i+3; j++ {
			d := digitsOf(toks[j].lower)
			if d == "" || d != stripSeparators(toks[j].lower) {
				break
			}
			digits += d
			n = j - i + 1
		}
		if len(digits) >= 10 {
			return digits, n, true
		}
	}
	s := toks[i].lower
	if stripSeparators(s) != digitsOf(s) {
		return "", 0, false
	}
	d := digitsOf(s)

	// Two tokens that together make a subscriber number: "98765 43210".
	// Tried before the single-token rule because neither half reaches seven
	// digits on its own and both would otherwise be read as cardinals. Each
	// half must be at least four digits and the join a plausible length, so
	// "book 2 3" cannot become a phone number.
	if len(d) >= 4 && len(d) < 10 && i+1 < len(toks) {
		next := toks[i+1].lower
		if stripSeparators(next) == digitsOf(next) && len(digitsOf(next)) >= 4 {
			if joined := d + digitsOf(next); len(joined) >= 10 && len(joined) <= 15 {
				return joined, 2, true
			}
		}
	}
	if len(d) < 7 || len(d) > 15 {
		return "", 0, false
	}
	return d, 1, true
}

func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stripSeparators(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '+' || r == '.' || r == '_' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Identifiers.
// ---------------------------------------------------------------------------

// matchCode reads anything shaped like a reference rather than a quantity:
//
//	ORD-123   INV/2026/88   AB12CD   6e-2113   a uuid
//
// The shape is: contains a digit, and contains either a letter or an internal
// separator. A '#' prefix is accepted and stripped.
//
// A '#' FOLLOWED BY NOTHING BUT DIGITS IS NOT A CODE. "#4417" and "4417" are
// the same reference written two ways, and routing one to this field and the
// other to the number field would split a true duplicate on a typographic
// difference. Both become the number 4417.
//
// The canonical value lowercases and removes every separator, so "ORD-123",
// "ord/123" and "ORD123" are one value. The cost is that "ord 123" — the same
// reference with a space — is NOT, because a space is a token boundary and
// nothing here can tell "ord 123" from two unrelated words. That is an
// under-extraction and it is measured rather than special-cased.
func matchCode(t token) (string, bool) {
	s := strings.TrimPrefix(t.lower, "#")
	if s == "" {
		return "", false
	}
	hasDigit, hasLetter, hasSep := false, false, false
	for idx, r := range s {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsLetter(r):
			hasLetter = true
		case r == '-' || r == '/' || r == '_' || r == '.':
			// A separator only counts when it is internal; a trailing one is
			// punctuation the trim missed.
			if idx > 0 && idx < len(s)-1 {
				hasSep = true
			}
		default:
			return "", false
		}
	}
	if !hasDigit || !(hasLetter || hasSep) {
		return "", false
	}
	if !hasLetter {
		// Digits and separators only. If that parses as a decimal it is a
		// quantity, not a reference: "500.00" is five hundred, and reading
		// it as the code "50000" would put an amount in the wrong field and
		// make it disagree with the same amount written plainly.
		if _, isNumber := normaliseDecimal(s); isNumber {
			return "", false
		}
	}
	// A pure-digit run with separators is a date, a time or a phone number,
	// all of which were tried before this. Reaching here with no letter means
	// a form none of them recognised, and calling that an identifier is more
	// useful than dropping it into the free text.
	var b strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) || unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String(), true
}

// ---------------------------------------------------------------------------
// Cardinal numbers, written either way.
// ---------------------------------------------------------------------------

// numberWords are the spelled-out cardinals, and the multipliers that combine
// them. Another closed class: it is the same list for every domain.
var numberWords = map[string]int{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fourty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// multiplierNames are the words that scale the group before them. A
// multiplier of a thousand or more closes the group and adds it to the total
// ("two thousand five hundred" is 2000 + 500); "hundred" only scales it.
var multiplierNames = map[string]int{
	"hundred": 100, "thousand": 1000, "lakh": 100000, "lac": 100000,
	"million": 1000000, "crore": 10000000, "billion": 1000000000,
}

// matchNumber reads a cardinal and normalises it, so that "2" and "two" are
// the same field value. Without that, a planner that spells a count out turns
// one booking into two.
func matchNumber(toks []token, i int) (value string, width int, ok bool) {
	// "a single seat" and "one seat" are the same count, and "single" is a
	// cardinal in everything but spelling.
	if toks[i].lower == "single" {
		return "1", 1, true
	}
	return readCardinal(toks, i)
}

// readCardinal is the shared number reader: matchNumber uses it for bare
// counts and matchMoney for amounts, so "$500" and "five hundred dollars"
// normalise through the same code and cannot drift apart.
func readCardinal(toks []token, i int) (value string, width int, ok bool) {
	if v, ok := normaliseDecimal(toks[i].lower); ok {
		// Digits, possibly with a glued multiplier such as "5k".
		if i+1 < len(toks) {
			if mult, isMult := multiplierNames[toks[i+1].lower]; isMult {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					return trimFloat(f * float64(mult)), 2, true
				}
			}
		}
		return v, 1, true
	}
	if n, w, ok := readSpelledNumber(toks, i); ok {
		return strconv.Itoa(n), w, true
	}
	return "", 0, false
}

// readSpelledNumber reads "five hundred", "twenty five", "two thousand" and
// "one hundred and fifty" as one value.
//
// It is a small accumulator rather than a table of phrases: a table would
// need every combination written out, and a combination it lacked would be
// read as two separate numbers — which is worse than not reading it at all,
// because two numbers are two deciding values.
func readSpelledNumber(toks []token, i int) (total, width int, ok bool) {
	n, group := 0, 0
	consumed, sawWord := 0, false

	for j := i; j < len(toks); j++ {
		w := toks[j].lower
		if v, isWord := numberWords[w]; isWord {
			group += v
			sawWord, consumed = true, j-i+1
			continue
		}
		if mult, isMult := multiplierNames[w]; isMult && sawWord {
			if group == 0 {
				group = 1
			}
			if mult >= 1000 {
				n += group * mult
				group = 0
			} else {
				group *= mult
			}
			consumed = j - i + 1
			continue
		}
		if w == "and" && sawWord && j+1 < len(toks) {
			if _, isWord := numberWords[toks[j+1].lower]; isWord {
				continue
			}
		}
		break
	}
	if !sawWord {
		return 0, 0, false
	}
	return n + group, consumed, true
}

// normaliseDecimal reads a digit string, with or without a decimal point, and
// returns its shortest exact spelling.
//
// "500", "500.00" and "500.0" all become "500", because they are one amount
// and a gate that told them apart would split a true duplicate over trailing
// zeros. "500.50" becomes "500.5" for the same reason.
func normaliseDecimal(s string) (string, bool) {
	s = strings.TrimSpace(s)
	// "#4417" and "4417" are one reference written two ways. The hash is
	// dropped here, in the one place every numeric path goes through, so no
	// caller can forget it.
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimSuffix(s, "/-") // "Rs 2000/-"
	if s == "" {
		return "", false
	}
	seenDigit, seenDot := false, false
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			seenDigit = true
		case r == '.':
			if seenDot {
				return "", false
			}
			seenDot = true
		default:
			return "", false
		}
	}
	if !seenDigit {
		return "", false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", false
	}
	return trimFloat(f), true
}

// trimFloat prints a number without a trailing fractional zero.
func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ---------------------------------------------------------------------------
// Proper nouns: the one heuristic.
// ---------------------------------------------------------------------------

// capitalStopWords are the words that are capitalised for reasons other than
// being names. It is deliberately tiny — a dozen entries — because every
// entry is a word this extractor refuses to treat as a value, and a long list
// would be the vocabulary this file exists to avoid.
var capitalStopWords = map[string]bool{
	"i": true, "i'm": true, "i'll": true, "ok": true, "okay": true,
	"yes": true, "no": true, "please": true, "thanks": true,
	"am": true, "pm": true, "and": true, "the": true, "a": true, "an": true,
}

// matchName applies the capitalisation heuristic. See the file header for why
// it is narrow and what that costs.
//
// It deliberately accepts ALL-CAPS tokens such as "BLR": a three-letter code
// written in capitals is a value by any reading, and refusing it would throw
// away the most value-like tokens in the corpus. It deliberately refuses
// sentence-initial tokens, which is what keeps "Book", "Send" and "Transfer"
// out of the gate.
func matchName(t token) (string, bool) {
	if t.SentenceStart {
		return "", false
	}
	r := []rune(t.raw)
	if len(r) < 2 || !unicode.IsUpper(r[0]) {
		return "", false
	}
	for _, c := range r {
		if !unicode.IsLetter(c) && c != '\'' && c != '-' {
			return "", false
		}
	}
	if capitalStopWords[t.lower] {
		return "", false
	}
	return t.lower, true
}

// ---------------------------------------------------------------------------
// Free-text noise.
// ---------------------------------------------------------------------------

// genericFunctionWords are the words dropped from the remainder: closed-class
// function words, and the JSON keys the engine's intent text wraps arguments
// in.
//
// Note what is NOT here, and compare it with itinerary.go's baseNoise. That
// list drops "book", "flight", "seat", "ticket", "passenger" — content words
// that happen to be uninformative IN FLIGHT BOOKING. Dropping them is a
// domain judgement, and a payments corpus would need a different list. This
// list contains no content word at all, so it needs no judgement and travels
// unchanged to any domain.
var genericFunctionWords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "from": true, "for": true,
	"on": true, "at": true, "in": true, "of": true, "and": true, "or": true,
	"with": true, "by": true, "as": true, "is": true, "be": true, "it": true,
	"this": true, "that": true, "these": true, "those": true, "there": true,
	"i": true, "me": true, "my": true, "we": true, "our": true, "you": true,
	"your": true, "please": true, "kindly": true, "would": true, "could": true,
	"should": true, "will": true, "shall": true, "can": true, "do": true,
	"does": true, "did": true, "have": true, "has": true, "had": true,
	"am": true, "are": true, "was": true, "were": true, "been": true,

	// The keys of the JSON envelope IntentText builds. They arrive as tokens
	// on every call of every activity, so they are by definition incapable of
	// telling two calls apart.
	"args": true, "tool": true, "query": true, "q": true, "input": true,
}

// genericValueNoise is the much narrower list applied to EXTRACTED VALUES:
// the activity's own name, the words it is made of, and the keys of the JSON
// envelope the engine wraps arguments in.
//
// It is separate from genericFunctionWords, and it has to be. That list
// contains "am" — the verb — and a daypart's canonical value is also "am", so
// filtering values through it would delete every morning from every intent.
// A value filter may only remove tokens that are incapable of distinguishing
// two calls OF THE SAME ACTIVITY TYPE, which is exactly what this list is:
// every candidate the engine compares against is already the same type, so
// the type's own name is true of all of them.
func genericValueNoise(activityType string) map[string]bool {
	out := map[string]bool{
		"args": true, "tool": true, "query": true, "q": true, "input": true,
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

// genericNoiseFor adds the activity's own name to the function words, the way
// noiseFor does and for the same reason: the candidate set is already
// restricted to one activity type, so words restating that type are true of
// every candidate.
func genericNoiseFor(activityType string) map[string]bool {
	out := make(map[string]bool, len(genericFunctionWords)+4)
	for w := range genericFunctionWords {
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
