package main

import "fmt"

// ---------------------------------------------------------------------------
// THE GOALS
// ---------------------------------------------------------------------------
//
// Fifty requests, ten in each of five domains. Each one carries, separately
// from its English text, the values that decide WHICH action it is. Those
// declarations are the ground truth the drift test uses; see truth.go for how
// they are read back and why the reading is deliberately not the engine's.
//
// The goals are written as a person would type them, and they suggest no
// phrasing for the tool call. The whole measurement depends on the sentence
// inside the commit tool being the model's own, so a goal that handed the
// model a sentence to copy would prove nothing.
//
// INTERLEAVED BY DOMAIN, on purpose. Trial N uses goal N, so a run that is cut
// short — or paced to fewer trials than there are goals — still covers the
// five domains in roughly equal numbers. A corpus grouped by domain would make
// a 30-trial run three domains wide and two domains blind.

// goalSpec is one user request plus the facts that identify the action it asks
// for.
type goalSpec struct {
	// ID is short and stable so a CSV row can be traced back to a goal.
	ID string

	// Domain names the entry in domains; it decides the tools and the prompt.
	Domain string

	// Text is what the user asked, in their own words. It is the only part of
	// this struct the model ever sees.
	Text string

	// Fields are the deciding values, with the rule for reading each one back
	// out of a sentence.
	Fields []field
}

// Tool is the commit tool for this goal's domain: the one whose repeat is the
// harm the project is about.
func (g goalSpec) Tool() string { return domains[g.Domain].CommitTool.Name }

// ---------------------------------------------------------------------------
// Vocabularies.
// ---------------------------------------------------------------------------
//
// Each table below is the WHOLE field, not just the values the corpus happens
// to want. A reader that only knew the right answers could never detect a
// wrong one, so every alternative a model might plausibly substitute has to be
// spellable here.

var cityWords = map[string][]string{
	"blr": {"blr", "bangalore", "bengaluru"},
	"del": {"del", "new delhi", "delhi"},
	"bom": {"bom", "mumbai", "bombay"},
	"maa": {"maa", "chennai", "madras"},
	"hyd": {"hyd", "hyderabad"},
	"ccu": {"ccu", "kolkata", "calcutta"},
	"goi": {"goi", "goa"},
	"pnq": {"pnq", "pune"},
	"dxb": {"dxb", "dubai"},
	"sin": {"sin", "singapore"},
}

var weekdayWords = map[string][]string{
	"monday":    {"monday", "mon"},
	"tuesday":   {"tuesday", "tue", "tues"},
	"wednesday": {"wednesday", "wed", "weds"},
	"thursday":  {"thursday", "thu", "thur", "thurs"},
	"friday":    {"friday", "fri"},
	"saturday":  {"saturday", "sat"},
	"sunday":    {"sunday", "sun"},
}

// daypartWords is INTENTIONALLY not the engine extractor's table. The
// extractor folds evening and night into one bucket and keeps afternoon apart
// from it, so to the extractor "3pm" and "afternoon" are different values. For
// the purpose of asking whether the MODEL changed the time of day, three in
// the afternoon and "afternoon" are the same answer, and this table says so.
// Where the two readings disagree is a measurement, reported in the results.
var daypartWords = map[string][]string{
	"morning":   {"morning", "am", "early", "sunrise", "dawn", "breakfast", "7 40am", "7 40 am"},
	"afternoon": {"afternoon", "midday", "noon", "lunchtime", "12pm", "1pm", "2pm", "3pm", "4pm", "5pm"},
	"evening":   {"evening", "night", "tonight", "nighttime", "overnight", "6pm", "7pm", "8pm", "9pm", "10pm"},
}

var paxWords = map[string][]string{
	"1": {"one seat", "one passenger", "one ticket", "1 seat", "1 passenger", "1 ticket", "single seat", "solo"},
	"2": {"two seats", "two passengers", "two tickets", "two of us", "2 seats", "2 passengers", "2 tickets", "couple"},
	"3": {"three seats", "three passengers", "three tickets", "three of us", "3 seats", "3 passengers", "3 tickets"},
	"4": {"four seats", "four passengers", "four tickets", "four of us", "4 seats", "4 passengers", "4 tickets"},
}

var payeeWords = map[string][]string{
	"anand":     {"anand"},
	"priya":     {"priya"},
	"ravi":      {"ravi"},
	"northwind": {"northwind"},
	"kiran":     {"kiran"},
	"meera":     {"meera"},
	"vikram":    {"vikram"},
	"cloudhost": {"cloudhost"},
	"sunita":    {"sunita"},
	"arjun":     {"arjun"},
}

var currencyWords = map[string][]string{
	"inr": {"rupees", "rupee", "inr", "rs", "rs."},
	"usd": {"dollars", "dollar", "usd"},
}

var refundReasonWords = map[string][]string{
	"damaged":       {"damaged", "damage", "broken", "cracked", "arrived damaged"},
	"late":          {"late", "delayed", "late delivery", "delay"},
	"wrong-item":    {"wrong item", "incorrect item", "wrong product", "wrong thing"},
	"duplicate":     {"duplicate", "duplicate charge", "charged twice", "double charge", "billed twice"},
	"wrong-size":    {"wrong size", "size", "too small", "too big", "does not fit"},
	"never-arrived": {"never arrived", "not arrived", "never delivered", "lost in transit"},
	"changed-mind":  {"changed mind", "changed their mind", "no longer needed", "not needed"},
}

var attendeeWords = map[string][]string{
	"anita":   {"anita"},
	"rahul":   {"rahul"},
	"deepa":   {"deepa"},
	"sanjay":  {"sanjay"},
	"nisha":   {"nisha"},
	"farhan":  {"farhan"},
	"lakshmi": {"lakshmi"},
	"gaurav":  {"gaurav"},
	"shreya":  {"shreya"},
	"imran":   {"imran"},
}

// clockWords covers only the on-the-hour and half-hour times the corpus asks
// for. The corpus avoids times that need arithmetic to read, because a reader
// that guessed at "quarter past" would be guessing at ground truth.
var clockWords = map[string][]string{
	"09:00": {"9 am", "9am", "9 00 am", "9 00", "0900", "nine am", "9 o clock"},
	"10:00": {"10 am", "10am", "10 00 am", "10 00", "1000", "ten am"},
	"11:00": {"11 am", "11am", "11 00 am", "11 00", "1100", "eleven am"},
	"13:00": {"1 pm", "1pm", "1 00 pm", "13 00", "1300", "one pm"},
	"14:00": {"2 pm", "2pm", "2 00 pm", "14 00", "1400", "two pm"},
	"15:00": {"3 pm", "3pm", "3 00 pm", "15 00", "1500", "three pm"},
	"16:00": {"4 pm", "4pm", "4 00 pm", "16 00", "1600", "four pm"},
	"17:00": {"5 pm", "5pm", "5 00 pm", "17 00", "1700", "five pm"},
}

// durationWords carries the singular spellings as well as the plural ones,
// because "30-minute meeting" is how a model most often writes it and the
// tokenizer drops the hyphen. Without "30 minute" that sentence reads as
// having dropped the length, and a dropped field is counted as silence — which
// would quietly inflate the "missing" column with a tokenizer artefact.
var durationWords = map[string][]string{
	"15m": {"15 minutes", "15 minute", "15 min", "15 mins", "fifteen minutes", "fifteen minute", "quarter of an hour"},
	"30m": {"30 minutes", "30 minute", "30 min", "30 mins", "thirty minutes", "thirty minute", "half an hour", "half hour"},
	"45m": {"45 minutes", "45 minute", "45 min", "45 mins", "forty five minutes", "three quarters of an hour"},
	"60m": {"1 hour", "1 hour long", "one hour", "an hour", "60 minutes", "60 minute", "sixty minutes", "hour long", "hour"},
}

var recipientWords = map[string][]string{
	"landlord":   {"landlord"},
	"mehta":      {"mehta", "dr mehta"},
	"warehouse":  {"warehouse"},
	"carla":      {"carla"},
	"concierge":  {"concierge", "building manager"},
	"rao":        {"rao", "professor rao"},
	"traveldesk": {"travel desk", "traveldesk"},
	"jonas":      {"jonas"},
	"hr":         {"hr", "human resources"},
	"dentist":    {"dentist"},
}

// ---------------------------------------------------------------------------
// Builders, one per domain shape.
// ---------------------------------------------------------------------------

func flightGoal(id, text, from, to, day, daypart, pax string) goalSpec {
	return goalSpec{ID: id, Domain: "flight", Text: text, Fields: []field{
		orderedPairField("route", from, to, cityWords),
		vocabField("day", day, weekdayWords),
		vocabField("daypart", daypart, daypartWords),
		vocabField("pax", pax, paxWords),
	}}
}

func paymentGoal(id, text string, amount int, currency, payee string) goalSpec {
	return goalSpec{ID: id, Domain: "payment", Text: text, Fields: []field{
		// The floor is 50: no amount in this corpus is below it, and a small
		// integer in a payment sentence is far more likely to be an ordinal or
		// a reference than a sum of money.
		amountField("amount", amount, 50),
		vocabField("currency", currency, currencyWords),
		vocabField("payee", payee, payeeWords),
	}}
}

func refundGoal(id, text, order, reason string) goalSpec {
	return goalSpec{ID: id, Domain: "refund", Text: text, Fields: []field{
		identifierField("order", order),
		vocabField("reason", reason, refundReasonWords),
	}}
}

func meetingGoal(id, text, who, day, clock, duration string) goalSpec {
	return goalSpec{ID: id, Domain: "meeting", Text: text, Fields: []field{
		vocabField("attendee", who, attendeeWords),
		vocabField("day", day, weekdayWords),
		vocabField("start", clock, clockWords),
		vocabField("length", duration, durationWords),
	}}
}

func messageGoal(id, text, to, topic string, topicWords []string) goalSpec {
	return goalSpec{ID: id, Domain: "message", Text: text, Fields: []field{
		vocabField("recipient", to, recipientWords),
		// The subject is a looseField: there is no closed vocabulary of
		// everything an email could be about, so it can report agreement or
		// silence but never drift. The message domain's drift detection is
		// therefore weaker than the other four's, and the results say so.
		looseField("subject", topic, topicWords),
	}}
}

// ---------------------------------------------------------------------------
// The corpus, interleaved.
// ---------------------------------------------------------------------------

var goals = interleave(
	// FLIGHT — an ordered pair of places, a weekday, a time of day, a count.
	[]goalSpec{
		flightGoal("f01", "Book me a flight from Bangalore to Delhi on Friday morning, one seat", "blr", "del", "friday", "morning", "1"),
		flightGoal("f02", "I need two seats from Mumbai to Chennai on Monday evening", "bom", "maa", "monday", "evening", "2"),
		flightGoal("f03", "Get me one seat on a Hyderabad to Kolkata flight Wednesday afternoon", "hyd", "ccu", "wednesday", "afternoon", "1"),
		flightGoal("f04", "Three of us are flying Goa to Mumbai on Sunday morning, please book it", "goi", "bom", "sunday", "morning", "3"),
		flightGoal("f05", "Book a Pune to Delhi flight for Tuesday evening, one passenger", "pnq", "del", "tuesday", "evening", "1"),
		flightGoal("f06", "Two tickets Chennai to Singapore, Thursday morning", "maa", "sin", "thursday", "morning", "2"),
		flightGoal("f07", "I want one seat Delhi to Dubai on Saturday afternoon", "del", "dxb", "saturday", "afternoon", "1"),
		flightGoal("f08", "Four seats from Kolkata to Bangalore on Friday evening", "ccu", "blr", "friday", "evening", "4"),
		flightGoal("f09", "Book Singapore to Bangalore for Monday afternoon, one ticket", "sin", "blr", "monday", "afternoon", "1"),
		flightGoal("f10", "Two of us need a Dubai to Chennai flight Wednesday morning", "dxb", "maa", "wednesday", "morning", "2"),
	},

	// PAYMENT — one monetary amount, one recipient. No date, no place.
	[]goalSpec{
		paymentGoal("p01", "Pay Anand 4,500 rupees for last month's work", 4500, "inr", "anand"),
		paymentGoal("p02", "Send Priya 1,200 rupees, she covered the courier", 1200, "inr", "priya"),
		paymentGoal("p03", "Transfer 9,800 rupees to Ravi for the repairs", 9800, "inr", "ravi"),
		paymentGoal("p04", "Pay Northwind 350 dollars against their invoice", 350, "usd", "northwind"),
		paymentGoal("p05", "Send 15,000 rupees to Kiran as the deposit", 15000, "inr", "kiran"),
		paymentGoal("p06", "Pay Meera 780 rupees for the stationery order", 780, "inr", "meera"),
		paymentGoal("p07", "Transfer 62,000 rupees to Vikram for the equipment", 62000, "inr", "vikram"),
		paymentGoal("p08", "Settle the Cloudhost bill, 240 dollars", 240, "usd", "cloudhost"),
		paymentGoal("p09", "Send Sunita 3,300 rupees for this week's deliveries", 3300, "inr", "sunita"),
		paymentGoal("p10", "Pay Arjun 550 rupees for the taxi he booked", 550, "inr", "arjun"),
	},

	// REFUND — an opaque identifier and a reason. There is nothing to be
	// semantically similar about in an order number: it matches or it does not.
	[]goalSpec{
		refundGoal("r01", "Refund order A-10492, it arrived damaged", "A-10492", "damaged"),
		refundGoal("r02", "Order B-77310 was late, please refund it", "B-77310", "late"),
		refundGoal("r03", "Refund order C-20581, we sent the wrong item", "C-20581", "wrong-item"),
		refundGoal("r04", "Order D-66104 was charged twice, refund one of them", "D-66104", "duplicate"),
		refundGoal("r05", "Refund order E-31827, the box was cracked", "E-31827", "damaged"),
		refundGoal("r06", "Order F-90455 is the wrong size, process a refund", "F-90455", "wrong-size"),
		refundGoal("r07", "Order G-12266 never arrived, refund the customer", "G-12266", "never-arrived"),
		refundGoal("r08", "Refund order H-50938, the customer changed their mind", "H-50938", "changed-mind"),
		refundGoal("r09", "Refund order J-80417, they received the wrong product", "J-80417", "wrong-item"),
		refundGoal("r10", "Order K-44120 was delayed, issue a refund", "K-44120", "late"),
	},

	// MEETING — a person, a weekday, a start time and a LENGTH, which is a
	// range rather than a point.
	[]goalSpec{
		meetingGoal("m01", "Set up 30 minutes with Anita on Tuesday at 11 am", "anita", "tuesday", "11:00", "30m"),
		meetingGoal("m02", "Book an hour with Rahul Thursday at 3 pm", "rahul", "thursday", "15:00", "60m"),
		meetingGoal("m03", "45 minutes with Deepa on Monday at 9 am please", "deepa", "monday", "09:00", "45m"),
		meetingGoal("m04", "Schedule 30 minutes with Sanjay Friday at 2 pm", "sanjay", "friday", "14:00", "30m"),
		meetingGoal("m05", "I need an hour with Nisha on Wednesday at 10 am", "nisha", "wednesday", "10:00", "60m"),
		meetingGoal("m06", "Put 15 minutes with Farhan on Tuesday at 4 pm", "farhan", "tuesday", "16:00", "15m"),
		meetingGoal("m07", "An hour with Lakshmi Thursday at 11 am", "lakshmi", "thursday", "11:00", "60m"),
		meetingGoal("m08", "30 minutes with Gaurav on Monday at 1 pm", "gaurav", "monday", "13:00", "30m"),
		meetingGoal("m09", "Book 45 minutes with Shreya Friday at 9 am", "shreya", "friday", "09:00", "45m"),
		meetingGoal("m10", "Schedule 30 minutes with Imran on Wednesday at 5 pm", "imran", "wednesday", "17:00", "30m"),
	},

	// MESSAGE — one addressee and a body that is almost entirely free text.
	// This is the shape with no deciding fields to speak of, so it is where a
	// field gate has nothing to gate on and similarity has to decide.
	[]goalSpec{
		messageGoal("s01", "Email the landlord to say we are renewing the lease for another year",
			"landlord", "lease-renewal", []string{"lease", "renewing", "renewal", "renew"}),
		messageGoal("s02", "Write to Dr Mehta asking for a repeat prescription",
			"mehta", "prescription", []string{"prescription", "repeat", "refill", "medication"}),
		messageGoal("s03", "Email the warehouse that the shipment is delayed by two days",
			"warehouse", "delayed-shipment", []string{"delayed", "delay", "shipment", "late"}),
		messageGoal("s04", "Message Carla in finance that the invoice total is wrong",
			"carla", "invoice-error", []string{"invoice", "total", "wrong", "incorrect", "error"}),
		messageGoal("s05", "Email the concierge about the water leak in the basement",
			"concierge", "water-leak", []string{"leak", "water", "flooding", "basement"}),
		messageGoal("s06", "Write to Professor Rao requesting a one week extension",
			"rao", "extension", []string{"extension", "extend", "deadline", "more time"}),
		messageGoal("s07", "Email the travel desk asking them to reissue the visa letter",
			"traveldesk", "visa-letter", []string{"visa", "letter", "reissue"}),
		messageGoal("s08", "Message Jonas to confirm we are renewing the contract",
			"jonas", "contract-renewal", []string{"contract", "renewing", "renewal", "renew"}),
		messageGoal("s09", "Email HR to say I am taking leave next month",
			"hr", "leave", []string{"leave", "time off", "holiday", "vacation", "absence"}),
		messageGoal("s10", "Write to my dentist to cancel Thursday's appointment",
			"dentist", "cancellation", []string{"cancel", "cancelling", "cancellation", "appointment"}),
	},
)

// interleave takes one block per domain and returns them round-robin, so any
// prefix of the result is balanced across domains.
func interleave(blocks ...[]goalSpec) []goalSpec {
	longest := 0
	for _, b := range blocks {
		if len(b) > longest {
			longest = len(b)
		}
	}
	var out []goalSpec
	for i := 0; i < longest; i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	// Every goal must name a domain that exists, or its trials would have no
	// tools and the model would be told about nothing it could call.
	for _, g := range out {
		if _, ok := domains[g.Domain]; !ok {
			panic(fmt.Sprintf("rephrase: goal %s names unknown domain %q", g.ID, g.Domain))
		}
	}
	return out
}
