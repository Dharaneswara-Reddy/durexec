package semantic

// ---------------------------------------------------------------------------
// GENERIC-V2: A PAIRWISE, DOMAIN-INDEPENDENT DUPLICATE CHECK
// ---------------------------------------------------------------------------
//
// # What was wrong with extracting fields one side at a time
//
// The first generic extractor (generic.go) read each intent ON ITS OWN and
// kept only value-shaped tokens: numbers, dates, codes, addresses. Measured on
// the ten-domain corpus, gate-only, it caught 149 of 150 duplicates and
// wrongly suppressed 40 of 150 distinct actions. Reading those 40 showed three
// causes, none specific to one domain:
//
//  1. The deciding value was a WORD, not a shape: a payee (Acme, Borzoi), a
//     city (Pune, Nagpur), a service level (standard, express), a plan
//     (growth, scale), a reason (damaged, late), an action (delete, snapshot).
//     A shape reader cannot see any of these.
//  2. Direction was lost: MAA to CCU and CCU to MAA have the same values.
//  3. One side stated a value the other did not ("for three"), and the
//     neutral gate treated absence as agreement.
//
// # The idea
//
// Compare the two requests WITH EACH OTHER instead of extracting a fixed
// schema from each. A genuine repeat may say more or less, and may use
// other words for the same things; what it must not do is REPLACE something.
// So:
//
//   - Typed values (amounts, g2Currencies, counts, measures, dates, g2Weekdays,
//     times, parts of the day, durations, identifiers, e-mail and host
//     addresses, IPs, versions) are normalised by FORM and compared by kind.
//     A value of one kind that is replaced by a different value of the same
//     kind is a contradiction.
//   - Every other content word is matched against the other side's content
//     words: exactly, through a shared WordNet synset ("book"/"reserve",
//     "buy"/"purchase"), through an abbreviation ("BLR"/"Bangalore",
//     "INFY"/"Infosys"), or as a prefix or spelling variant ("prod"/
//     "production"). A word left g2Unmatched on BOTH sides is a replacement:
//     "Pune" became "Nagpur", "standard" became "express".
//   - "X to Y" is kept as an ordered pair, so a reversed route contradicts.
//   - A value present on one side only is NEW INFORMATION, not agreement, so
//     the pair is escalated rather than called a duplicate. The single
//     exception is a count of one, which unstated counts default to.
//
// The verdict is DUPLICATE only when nothing was replaced, nothing was added
// that changes the action, and at least one value or content word was
// actually shared; DISTINCT on any contradiction or replacement; ESCALATE
// otherwise. Being unsure is never resolved towards suppression.
//
// Nothing here names a domain. The lexical knowledge is general English
// (WordNet 3.0, number words, month and weekday names, currency codes and
// symbols, units of measure); the rest is the form of the tokens.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/semantic/places"
	"github.com/dharaneswar339/durexec/internal/idem/semantic/wordnet"
)

// PairVerdict is generic-v2's answer for one pair of intents.
type PairVerdict struct {
	Verdict core.DedupVerdict
	// Reason is the human-readable explanation that is journaled.
	Reason string
	// Shared counts the values and content words the two sides agreed on.
	Shared int
}

// GenericV2Compare compares two rendered intents of the same activity type.
// It is pure and deterministic.
func GenericV2Compare(activityType, a, b string) PairVerdict {
	return genericV2(activityType, a, b, false)
}

// GenericV2PlacesCompare is GenericV2Compare with the optional place
// knowledge of package places switched on: airport codes and the names of
// their cities are treated as the same word.
func GenericV2PlacesCompare(activityType, a, b string) PairVerdict {
	return genericV2(activityType, a, b, true)
}

func genericV2(activityType, a, b string, withPlaces bool) PairVerdict {
	x := g2Analyse(activityType, a)
	y := g2Analyse(activityType, b)
	x.places, y.places = withPlaces, withPlaces
	return g2CompareAnalyses(x, y)
}

// ---------------------------------------------------------------------------
// Analysis of one intent.
// ---------------------------------------------------------------------------

type g2Analysis struct {
	values map[string][]string // kind -> canonical values (a multiset)
	words  []string            // content words, lower case, in order
	routes [][2]string         // ordered "X to Y" pairs of content words or ids
	// frameHit records that the intent states its action in words that
	// restate the activity type ("delete" for DeleteResource). A request
	// that states it and one that instead says something else in that place
	// ("snapshot") have replaced the action.
	frameHit bool
	places   bool
}

var (
	reEmail   = regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	reIP      = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	reVersion = regexp.MustCompile(`^v?\d+(\.\d+){1,3}$`)
	reHost    = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)+$`)
	reTime    = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)?$`)
	reNumber  = regexp.MustCompile(`^\d+(?:[.,]\d+)*$`)
	reSlashDt = regexp.MustCompile(`^(\d{1,2})[/-](\d{1,2})(?:[/-](\d{2,4}))?$`)
	reISODate = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
	reOrdinal = regexp.MustCompile(`^(\d{1,2})(st|nd|rd|th)$`)
	reNumUnit = regexp.MustCompile(`^(\d+(?:\.\d+)?)([a-z]+)$`)
	reXCount  = regexp.MustCompile(`^x(\d+)$`)
	reAtVer   = regexp.MustCompile(`^([a-z][a-z0-9_-]*)@(v?\d+(?:\.\d+){1,3})$`)
)

var g2Weekdays = map[string]string{
	"mon": "mon", "monday": "mon", "tue": "tue", "tues": "tue", "tuesday": "tue",
	"wed": "wed", "weds": "wed", "wednesday": "wed", "thu": "thu", "thur": "thu", "thurs": "thu", "thursday": "thu",
	"fri": "fri", "friday": "fri", "sat": "sat", "saturday": "sat", "sun": "sun", "sunday": "sun",
}

var g2Months = map[string]int{
	"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3, "apr": 4, "april": 4,
	"may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7, "aug": 8, "august": 8,
	"sep": 9, "sept": 9, "september": 9, "oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
}

var g2Currencies = map[string]string{
	"inr": "inr", "rs": "inr", "rupee": "inr", "rupees": "inr", "₹": "inr",
	"usd": "usd", "dollar": "usd", "dollars": "usd", "$": "usd",
	"gbp": "gbp", "pound": "gbp", "pounds": "gbp", "£": "gbp",
	"eur": "eur", "euro": "eur", "euros": "eur", "€": "eur",
}

// units of MEASURE keep their unit, because 500 mg and 500 tablets are not
// the same fact. Count nouns ("nights", "seats") are not listed: their number
// is kept as a plain count.
var g2MeasureUnits = map[string]string{
	"mg": "mg", "milligram": "mg", "milligrams": "mg", "g": "g", "gram": "g", "grams": "g",
	"kg": "kg", "kilogram": "kg", "kilograms": "kg", "ml": "ml", "l": "l", "litre": "l", "liter": "l",
	"gb": "gb", "mb": "mb", "tb": "tb", "km": "km", "%": "pct", "percent": "pct",
	"min": "min", "mins": "min", "minute": "min", "minutes": "min",
	"h": "hr", "hr": "hr", "hrs": "hr", "hour": "hr", "hours": "hr",
}

var g2Dayparts = map[string]string{
	"am": "am", "morning": "am", "forenoon": "am",
	"pm": "pm", "afternoon": "pm", "evening": "pm", "night": "pm", "tonight": "pm",
}

var g2SmallNumbers = map[string]int{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14,
	"fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
	"single": 1, "couple": 2, "dozen": 12,
}

var g2ScaleWords = map[string]int{"hundred": 100, "thousand": 1000, "lakh": 100000, "million": 1000000}

// g2Stopwords: function words and the generic request frame every task shares.
// None of them can carry the identity of an action.
var g2Stopwords = g2ToSet(`a an the this that these those it its i me my mine we us our you your he him his she her
they them their and or but so of for to from in on at by with via as into onto out up down over under about
is are was were be been being am do does did done have has had will would shall should can could may might must
please kindly just also then now asap soon want wants wanted need needs needed like would get got let lets make
made go going give gave given put set use using some any all each every new same other another one's
there here which who whom whose what when where why how not no yes ok okay thanks thank hi hello dear
again quickly right away immediately sure fine starting start from till until through ago today
because against about after before since while if whether than though although x it's via per`)

func g2ToSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// g2SplitCamel turns "ReserveHotel" into ["reserve", "hotel"].
func g2SplitCamel(s string) []string {
	var out []string
	cur := []rune{}
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) && len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		out = append(out, strings.ToLower(string(cur)))
	}
	return out
}

// g2Tokenize lower-cases and splits on everything that cannot be part of a
// value. Dots, colons, slashes, @, hyphens and plus signs survive inside a
// token so that addresses, times, dates and versions stay whole.
var reThousands = regexp.MustCompile(`(\d),(\d{3})\b`)

func g2Tokenize(s string) []string {
	s = strings.ToLower(s)
	for reThousands.MatchString(s) {
		s = reThousands.ReplaceAllString(s, "$1$2")
	}
	for sym, code := range map[string]string{"₹": " inr ", "$": " usd ", "£": " gbp ", "€": " eur ", "%": " percent ", "->": " to ", "→": " to ", ">": " to "} {
		s = strings.ReplaceAll(s, sym, code)
	}
	s = strings.NewReplacer("–", "-", "—", "-", "’", "'", "‘", "'").Replace(s)
	f := strings.FieldsFunc(s, func(r rune) bool {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
		switch r {
		case '.', ':', '/', '@', '-', '_', '+', '\'':
			return false
		}
		return true
	})
	out := make([]string, 0, len(f))
	for _, t := range f {
		t = strings.Trim(t, ".:/-_'+")
		t = strings.TrimSuffix(t, "'s")
		if t == "" {
			continue
		}
		// "3-day", "2-night": a count joined to its noun.
		if i := strings.IndexByte(t, '-'); i > 0 {
			if _, err := strconv.Atoi(t[:i]); err == nil && g2IsAlpha(t[i+1:]) {
				out = append(out, t[:i], t[i+1:])
				continue
			}
			// "eighty-two" and "one-hour" become two tokens for the readers.
			if _, ok := g2SmallNumbers[t[:i]]; ok && g2IsAlpha(t[i+1:]) {
				out = append(out, t[:i], t[i+1:])
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func g2IsAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

func g2HasDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func g2HasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// g2ReadNumberWords reads a run of number words starting at i ("eighty two
// thousand five hundred") and returns the value and the tokens consumed.
func g2ReadNumberWords(toks []string, i int) (int, int) {
	total, cur, n := 0, 0, 0
	for j := i; j < len(toks); j++ {
		t := toks[j]
		if v, ok := g2SmallNumbers[t]; ok {
			cur += v
			n = j - i + 1
			continue
		}
		if v, ok := g2ScaleWords[t]; ok && (n > 0 || (j > i && (toks[j-1] == "a" || toks[j-1] == "an"))) {
			if cur == 0 {
				cur = 1
			}
			if v >= 1000 {
				total += cur * v
				cur = 0
			} else {
				cur *= v
			}
			n = j - i + 1
			continue
		}
		if t == "and" && n > 0 && j+1 < len(toks) {
			if _, ok := g2SmallNumbers[toks[j+1]]; ok {
				continue
			}
		}
		break
	}
	if n == 0 {
		return 0, 0
	}
	return total + cur, n
}

func g2ParseNum(s string) (string, bool) {
	if !reNumber.MatchString(s) {
		return "", false
	}
	// "1,500" and "82,000" are thousands separators; "10.5" is a decimal.
	if strings.Contains(s, ",") {
		s = strings.ReplaceAll(s, ",", "")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatFloat(f, 'f', -1, 64), true
}

func g2Analyse(activityType, text string) g2Analysis {
	an := g2Analysis{values: map[string][]string{}}
	add := func(kind, v string) { an.values[kind] = append(an.values[kind], v) }

	frame := map[string]bool{}
	for _, w := range g2SplitCamel(activityType) {
		frame[w] = true
	}

	toks := g2Tokenize(text)
	used := make([]bool, len(toks))
	// pending number waiting to learn whether a currency or unit follows
	type num struct {
		val string
		at  int
	}
	var lastNum *num

	countedAt := -1
	addNumber := func(v string, at int) {
		lastNum = &num{v, at}
		add("num", v)
		countedAt = at + 1
	}
	dropLastNum := func() {
		if lastNum == nil {
			return
		}
		nums := an.values["num"]
		for i := len(nums) - 1; i >= 0; i-- {
			if nums[i] == lastNum.val {
				an.values["num"] = append(nums[:i], nums[i+1:]...)
				break
			}
		}
	}

	for i := 0; i < len(toks); i++ {
		t := toks[i]
		next := ""
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		prev := ""
		if i > 0 {
			prev = toks[i-1]
		}

		switch {
		case reEmail.MatchString(t):
			add("addr", t)
		case reAtVer.MatchString(t):
			m := reAtVer.FindStringSubmatch(t)
			an.words = append(an.words, m[1])
			add("ver", strings.TrimPrefix(m[2], "v"))
		case reIP.MatchString(t):
			add("ip", t)
		case reISODate.MatchString(t):
			m := reISODate.FindStringSubmatch(t)
			mo, _ := strconv.Atoi(m[2])
			d, _ := strconv.Atoi(m[3])
			add("date", fmt.Sprintf("%02d-%02d", mo, d))
		case reVersion.MatchString(t) && (strings.HasPrefix(t, "v") || strings.Count(t, ".") >= 2 ||
			prev == "version" || prev == "release" || prev == "v"):
			add("ver", strings.TrimPrefix(t, "v"))
		case reHost.MatchString(t) && g2HasLetter(t):
			add("addr", t)
		case reSlashDt.MatchString(t) && strings.ContainsAny(t, "/"):
			m := reSlashDt.FindStringSubmatch(t)
			a1, _ := strconv.Atoi(m[1])
			a2, _ := strconv.Atoi(m[2])
			// Day-first and month-first are both possible; keep every
			// reading that is a real date and let the comparison accept any
			// overlap.
			var alts []string
			if a2 >= 1 && a2 <= 12 && a1 >= 1 && a1 <= 31 {
				alts = append(alts, fmt.Sprintf("%02d-%02d", a2, a1))
			}
			if a1 >= 1 && a1 <= 12 && a2 >= 1 && a2 <= 31 && a1 != a2 {
				alts = append(alts, fmt.Sprintf("%02d-%02d", a1, a2))
			}
			if len(alts) > 0 {
				add("date", strings.Join(alts, "|"))
			}
		case g2Weekdays[t] != "":
			wd := g2Weekdays[t]
			if g2ShiftNear(toks, i) {
				wd += "+"
			}
			add("wd", wd)
		case g2Months[t] != 0 && !(t == "may" && !g2IsDayNumber(prev) && !g2IsDayNumber(next)):
			mo := g2Months[t]
			day := 0
			if d, ok := g2DayNumber(next); ok {
				day = d
				used[i+1] = true
				i++
			} else if d, ok := g2DayNumber(prev); ok {
				day = d
				dropLastNum()
			} else if prev == "of" && i >= 2 {
				if d, ok := g2DayNumber(toks[i-2]); ok {
					day = d
					dropLastNum()
				}
			}
			if day > 0 {
				add("date", fmt.Sprintf("%02d-%02d", mo, day))
			} else {
				add("month", fmt.Sprintf("%02d", mo))
			}
		case t == "noon" || t == "midday":
			if prev == "before" {
				add("dp", "am")
			} else if prev == "after" {
				add("dp", "pm")
			} else {
				add("time", "12:00")
			}
		case g2Dayparts[t] != "" && !(t == "am" && prev == "i"):
			add("dp", g2Dayparts[t])
		case reTime.MatchString(t) && (strings.Contains(t, ":") || strings.HasSuffix(t, "am") || strings.HasSuffix(t, "pm")):
			m := reTime.FindStringSubmatch(t)
			h, _ := strconv.Atoi(m[1])
			mm := 0
			if m[2] != "" {
				mm, _ = strconv.Atoi(m[2])
			}
			if m[3] == "pm" && h < 12 {
				h += 12
			}
			if m[3] == "am" && h == 12 {
				h = 0
			}
			add("time", fmt.Sprintf("%02d:%02d", h, mm))
			if m[3] != "" {
				add("dp", m[3])
			}
		case (next == "am" || next == "pm") && g2IsSmallInt(t):
			h, _ := strconv.Atoi(t)
			if next == "pm" && h < 12 {
				h += 12
			}
			add("time", fmt.Sprintf("%02d:00", h))
			add("dp", next)
			i++
		case prev == "at" && (g2IsSmallInt(t) || (g2SmallNumbers[t] >= 1 && g2SmallNumbers[t] <= 12 && t != "single" && t != "couple")) &&
			g2MeasureUnits[next] == "":
			h, err := strconv.Atoi(t)
			if err != nil {
				h = g2SmallNumbers[t]
			}
			add("time", fmt.Sprintf("%02d:00", h))
		case g2Currencies[t] != "":
			add("cur", g2Currencies[t])
		case g2MeasureUnits[t] != "" && lastNum != nil && lastNum.at == i-1:
			// "500 mg", "30 minutes": the previous number is a measure.
			dropLastNum()
			add("measure", lastNum.val+g2NormaliseUnit(g2MeasureUnits[t]))
			lastNum = nil
		case reXCount.MatchString(t):
			addNumber(reXCount.FindStringSubmatch(t)[1], i)
		case reOrdinal.MatchString(t):
			m := reOrdinal.FindStringSubmatch(t)
			addNumber(m[1], i)
		case reNumUnit.MatchString(t) && g2MeasureUnits[reNumUnit.FindStringSubmatch(t)[2]] != "":
			m := reNumUnit.FindStringSubmatch(t)
			v, _ := g2ParseNum(m[1])
			add("measure", v+g2NormaliseUnit(g2MeasureUnits[m[2]]))
		case g2HasDigit(t) && g2HasLetter(t):
			// An identifier: letters and digits together, separators ignored.
			add("id", strings.Map(func(r rune) rune {
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					return r
				}
				return -1
			}, t))
		case g2HasDigit(t):
			if v, ok := g2ParseNum(t); ok {
				addNumber(v, i)
			} else {
				add("id", strings.Map(func(r rune) rune {
					if unicode.IsDigit(r) {
						return r
					}
					return -1
				}, t))
			}
		case t == "half" && next == "an" && i+2 < len(toks) && toks[i+2] == "hour":
			add("measure", "30min")
			i += 2
		case t == "half" && next == "hour":
			add("measure", "30min")
			i++

		case (t == "an" || t == "a") && (next == "hour"):
			add("measure", "60min")
			i++
		default:
			if v, n := g2ReadNumberWords(toks, i); n > 0 {
				addNumber(strconv.Itoa(v), i+n-1)
				i += n - 1
				continue
			}
			if !g2IsAlpha(strings.ReplaceAll(t, "-", "")) {
				continue
			}
			if g2Stopwords[t] || g2Shift[t] {
				continue
			}
			// The noun a count counts ("one seat", "3 passengers", "two
			// people") names the unit, not the thing: rewordings change it
			// freely while the number carries the fact. Only an ordinary
			// English word can be such a unit; "50 INFY" or "x60,
			// Indiranagar" is a count followed by a NAME, which is kept.
			if i == countedAt && wordnet.Known(t) {
				continue
			}
			if frame[t] || g2IsFrameInflection(t, frame) {
				an.frameHit = true
				continue
			}
			an.words = append(an.words, t)
		}
	}

	// A number immediately before or after a currency is money.
	if cur := an.values["cur"]; len(cur) > 0 {
		_ = cur
	}

	// Ordered pairs: "X to Y" where both are content words or identifiers.
	for i := 1; i+1 < len(toks); i++ {
		if toks[i] != "to" {
			continue
		}
		x, y := "", ""
		for j := i - 1; j >= 0 && j >= i-3; j-- {
			if !g2Stopwords[toks[j]] {
				x = toks[j]
				break
			}
		}
		for j := i + 1; j < len(toks) && j <= i+3; j++ {
			if !g2Stopwords[toks[j]] {
				y = toks[j]
				break
			}
		}
		if g2IsEndpoint(x, frame) && g2IsEndpoint(y, frame) {
			an.routes = append(an.routes, [2]string{x, y})
		}
	}
	for k := range an.values {
		sort.Strings(an.values[k])
	}
	return an
}

// g2Shift words move a weekday to a later week: "next Friday", "Friday
// week", "a week on Friday", "the Friday after this one".
var g2Shift = map[string]bool{"next": true, "following": true, "week": true, "fortnight": true, "after": true}

func g2ShiftNear(toks []string, i int) bool {
	for j := i - 3; j <= i+3; j++ {
		if j < 0 || j >= len(toks) || j == i {
			continue
		}
		if g2Shift[toks[j]] {
			// "after 6pm" and "after noon" shift a time, not the week
			if toks[j] == "after" && j+1 < len(toks) && (reTime.MatchString(toks[j+1]) || toks[j+1] == "noon") {
				continue
			}
			return true
		}
	}
	return false
}

func g2NormaliseUnit(u string) string {
	if u == "hr" {
		return "hr"
	}
	return u
}

func g2IsSmallInt(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 12
}

func g2IsDayNumber(s string) bool { _, ok := g2DayNumber(s); return ok }

func g2DayNumber(s string) (int, bool) {
	if m := reOrdinal.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 31 {
		return 0, false
	}
	return n, true
}

func g2IsEndpoint(t string, frame map[string]bool) bool {
	if g2Stopwords[t] || frame[t] {
		return false
	}
	if g2HasDigit(t) && !g2HasLetter(t) {
		return false
	}
	if g2Weekdays[t] != "" || g2Months[t] != 0 || g2Dayparts[t] != "" || g2Currencies[t] != "" {
		return false
	}
	return g2HasLetter(t)
}

// g2IsFrameInflection reports whether w is an inflected form of a word of the
// activity type ("booking" for BookFlight, "payments" for SendPayment).
// Synonyms are deliberately NOT dropped: WordNet files "sell" under "trade",
// and dropping it for PlaceTrade hid the one word that separated a sale from
// a purchase. A synonym still matches the other side's word in the ordinary
// way, so nothing is lost by keeping it.
func g2IsFrameInflection(w string, frame map[string]bool) bool {
	for _, b := range wordnet.Bases(w) {
		if frame[b] {
			return true
		}
	}
	return false
}

// g2IsFrameSynonym drops a word that only restates the activity type, such as
// "book" for ReserveHotel, using the same general synonym test as every other
// comparison.
func g2IsFrameSynonym(w string, frame map[string]bool) bool {
	for f := range frame {
		if len(f) > 2 && wordnet.Synonyms(w, f) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Matching two content words.
// ---------------------------------------------------------------------------

// g2WordsMatch reports whether two content words can name the same thing.
func g2WordsMatch(a, b string) bool { return g2WordsMatchP(a, b, false) }

func g2WordsMatchP(a, b string, withPlaces bool) bool {
	if a == b {
		return true
	}
	if withPlaces && places.Same(a, b) {
		return true
	}
	if wordnet.Synonyms(a, b) {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	// An abbreviation: a short token whose letters appear in order in the
	// long word, starting with the same letter ("blr"/"bangalore",
	// "ccu"/"calcutta", "infy"/"infosys").
	if len(short) >= 2 && len(short) <= 5 && len(long) >= len(short)+2 && short[0] == long[0] && g2IsSubsequence(short, long) {
		return true
	}
	// A clipped form: "prod"/"production", "info"/"information".
	if len(short) >= 4 && strings.HasPrefix(long, short) {
		return true
	}
	// A one-letter spelling variant of a long word: "canceled"/"cancelled".
	if len(short) >= 7 && g2EditDistanceAtMostOne(short, long) {
		return true
	}
	return false
}

func g2IsSubsequence(s, t string) bool {
	j := 0
	for i := 0; i < len(t) && j < len(s); i++ {
		if t[i] == s[j] {
			j++
		}
	}
	return j == len(s)
}

func g2EditDistanceAtMostOne(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > 1 {
		return false
	}
	i, j, diff := 0, 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		diff++
		if diff > 1 {
			return false
		}
		if len(a) == len(b) {
			i++
		}
		j++
	}
	return diff+(len(b)-j)+(len(a)-i) <= 1
}

// g2Unmatched returns the words of xs that match no word of ys.
// addrWords are the words an address spells out: "dana@northwind.test" gives
// "dana" and "northwind". A name stated beside its address is not new
// information.
func addrWords(a g2Analysis) []string {
	var out []string
	for _, v := range append(append([]string{}, a.values["addr"]...), a.values["id"]...) {
		for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == '@' || r == '.' || r == '-' || r == '_' }) {
			if g2IsAlpha(part) && len(part) >= 3 {
				out = append(out, part)
			}
		}
	}
	return out
}

func g2Unmatched(xs, ys []string) []string { return g2UnmatchedP(xs, ys, false) }

func g2UnmatchedP(xs, ys []string, withPlaces bool) []string {
	var out []string
	for _, x := range xs {
		found := false
		for _, y := range ys {
			if g2WordsMatchP(x, y, withPlaces) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, x)
		}
	}
	return g2DedupStrings(out)
}

func g2DedupStrings(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The comparison.
// ---------------------------------------------------------------------------

// g2ValueKinds are compared in this order, so the explanation is stable.
var g2ValueKinds = []string{"id", "addr", "ip", "ver", "date", "month", "wd", "time", "dp", "measure", "num", "cur"}

func g2AltsOverlap(a, b string) bool {
	for _, x := range strings.Split(a, "|") {
		for _, y := range strings.Split(b, "|") {
			if x == y {
				return true
			}
		}
	}
	return false
}

// g2MultisetDiff returns the values of xs that have no partner in ys, partners
// being used up one for one.
func g2MultisetDiff(xs, ys []string) []string {
	usedY := make([]bool, len(ys))
	var out []string
	for _, x := range xs {
		found := false
		for j, y := range ys {
			if !usedY[j] && g2AltsOverlap(x, y) {
				usedY[j] = true
				found = true
				break
			}
		}
		if !found {
			out = append(out, x)
		}
	}
	return out
}

func g2CompareAnalyses(x, y g2Analysis) PairVerdict {
	shared := 0
	var added []string

	// Typed values: numbers and measures are compared as one pool of
	// quantities so that "1500" and "Rs 1500" agree.
	xq := append(append([]string{}, x.values["num"]...), g2StripUnits(x.values["measure"])...)
	yq := append(append([]string{}, y.values["num"]...), g2StripUnits(y.values["measure"])...)
	for _, kind := range g2ValueKinds {
		var xs, ys []string
		switch kind {
		case "num":
			xs, ys = xq, yq
		case "measure":
			xs, ys = x.values["measure"], y.values["measure"]
		default:
			xs, ys = x.values[kind], y.values[kind]
		}
		onlyX := g2MultisetDiff(xs, ys)
		onlyY := g2MultisetDiff(ys, xs)
		if kind == "measure" {
			// Measures only contradict through their unit-bearing form when
			// both sides gave a unit; the bare quantity was compared above.
			onlyX, onlyY = g2UnitConflicts(onlyX, onlyY)
		}
		if len(onlyX) > 0 && len(onlyY) > 0 {
			return PairVerdict{Verdict: core.VerdictDistinct,
				Reason: fmt.Sprintf("generic-v2: %s differs (%s vs %s)", kind, strings.Join(onlyX, ","), strings.Join(onlyY, ","))}
		}
		shared += len(xs) - len(onlyX)
		for _, v := range append(onlyX, onlyY...) {
			if kind == "cur" {
				continue // an unstated currency is the local one
			}
			if kind == "num" && v == "1" {
				continue // an unstated count is one
			}
			if kind == "month" || kind == "dp" {
				// a month or part of day stated on one side only, alongside
				// an agreeing full date or time, adds nothing
				if len(x.values["date"])+len(x.values["time"]) > 0 && len(y.values["date"])+len(y.values["time"]) > 0 {
					continue
				}
			}
			added = append(added, kind+"="+v)
		}
	}

	// Direction.
	for _, rx := range x.routes {
		for _, ry := range y.routes {
			p := x.places
			if g2WordsMatchP(rx[0], ry[1], p) && g2WordsMatchP(rx[1], ry[0], p) && !g2WordsMatchP(rx[0], ry[0], p) {
				return PairVerdict{Verdict: core.VerdictDistinct,
					Reason: fmt.Sprintf("generic-v2: direction reversed (%s to %s vs %s to %s)", rx[0], rx[1], ry[0], ry[1])}
			}
		}
	}

	// Content words.
	p := x.places
	ux := g2Unmatched(g2UnmatchedP(x.words, y.words, p), addrWords(y))
	uy := g2Unmatched(g2UnmatchedP(y.words, x.words, p), addrWords(x))
	shared += len(g2DedupStrings(x.words)) - len(ux)
	if len(ux) > 0 && len(uy) > 0 {
		return PairVerdict{Verdict: core.VerdictDistinct, Shared: shared,
			Reason: fmt.Sprintf("generic-v2: words replaced (%s vs %s)", strings.Join(ux, ","), strings.Join(uy, ","))}
	}
	if shared == 0 {
		return PairVerdict{Verdict: core.VerdictDistinct,
			Reason: "generic-v2: nothing in common"}
	}
	if len(added) > 0 {
		return PairVerdict{Verdict: core.VerdictEscalate, Shared: shared,
			Reason: fmt.Sprintf("generic-v2: one side adds %s; nothing replaced, %d shared", strings.Join(added, ","), shared)}
	}
	extra := append(ux, uy...)
	why := "nothing replaced"
	if len(extra) > 0 {
		why = "one side only adds words (" + strings.Join(extra, ",") + ")"
	}
	return PairVerdict{Verdict: core.VerdictDuplicate, Shared: shared,
		Reason: fmt.Sprintf("generic-v2: %d value(s) and word(s) shared, %s", shared, why)}
}

func g2StripUnits(ms []string) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, strings.TrimRightFunc(m, unicode.IsLetter))
	}
	return out
}

// g2UnitConflicts keeps only measures whose unit is shared with a measure on the
// other side, so that "500mg" vs "60" is judged on the quantities alone.
func g2UnitConflicts(onlyX, onlyY []string) ([]string, []string) {
	unit := func(m string) string {
		return strings.TrimLeftFunc(m, func(r rune) bool { return unicode.IsDigit(r) || r == '.' })
	}
	ux := map[string]bool{}
	for _, m := range onlyX {
		ux[unit(m)] = true
	}
	var fx, fy []string
	for _, m := range onlyY {
		if ux[unit(m)] {
			fy = append(fy, m)
		}
	}
	uy := map[string]bool{}
	for _, m := range onlyY {
		uy[unit(m)] = true
	}
	for _, m := range onlyX {
		if uy[unit(m)] {
			fx = append(fx, m)
		}
	}
	return fx, fy
}
