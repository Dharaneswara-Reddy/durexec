package semantic

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// NORMALISATION IS THE WHOLE APPROACH, SO IT IS WHAT IS TESTED HARDEST
// ---------------------------------------------------------------------------
//
// The generic extractor's bet is that a rephrasing keeps the VALUES and
// changes only the words. The bet is lost the moment two spellings of one
// value extract differently: "two" against "2", "$500" against "500 dollars",
// "2026-03-12" against "12 March 2026". Each of those would make the gate
// close on a true duplicate, and a closed gate books the second seat.
//
// So the tests below come in two shapes. fieldValue asserts the canonical
// token for one spelling, and sameValue asserts that a whole set of spellings
// collapse onto one token — which is the property the comparator actually
// depends on.

// fields runs the extractor and fails the test if it errors.
func fields(t *testing.T, text string) Fields {
	t.Helper()
	f, err := GenericFields("", text)
	if err != nil {
		t.Fatalf("GenericFields(%q): unexpected error: %v", text, err)
	}
	return f
}

// fieldValue reads one deciding field, or "" when it is absent.
func fieldValue(f Fields, name string) string {
	v, _ := f.value(name)
	return v
}

func TestGenericNumbersNormalise(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"2 seats", "2"},
		{"two seats", "2"},
		{"TWO seats", "2"},
		{"a single seat", "1"},
		{"one seat", "1"},
		{"three people", "3"},
		{"twelve items", "12"},
		{"twenty five items", "25"},
		{"five hundred units", "500"},
		{"two thousand units", "2000"},
		{"one hundred and fifty units", "150"},
		{"two thousand five hundred units", "2500"},
		{"1,500 units", "1500"},
		{"500.00 units", "500"},
		{"500.50 units", "500.5"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := fieldValue(fields(t, c.text), GenericFieldNum); got != c.want {
				t.Errorf("number from %q = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

// TestGenericSpelledAndDigitAgree is the single most important assertion in
// this file. If it ever fails, a planner that spells a count out has invented
// a second booking.
func TestGenericSpelledAndDigitAgree(t *testing.T) {
	groups := [][]string{
		{"book 2 seats", "book two seats", "book TWO seats"},
		{"order 12 units", "order twelve units"},
		{"refund 500 units", "refund five hundred units"},
	}
	for _, g := range groups {
		t.Run(g[0], func(t *testing.T) {
			sameValue(t, GenericFieldNum, g...)
		})
	}
}

func TestGenericMoneyNormalises(t *testing.T) {
	cases := []struct {
		text         string
		want, wantCy string
	}{
		{"pay $500 now", "500", "usd"},
		{"pay $ 500 now", "500", "usd"},
		{"pay 500 USD now", "500", "usd"},
		{"pay 500 dollars now", "500", "usd"},
		{"pay USD 500 now", "500", "usd"},
		{"pay five hundred dollars now", "500", "usd"},
		{"pay Rs 2000 now", "2000", "inr"},
		{"pay Rs2000 now", "2000", "inr"},
		{"pay 2000 rupees now", "2000", "inr"},
		{"pay INR 2,199 now", "2199", "inr"},
		{"pay \u20b92000 now", "2000", "inr"},
		{"pay \u20ac40 now", "40", "eur"},
		{"pay 40 EUR now", "40", "eur"},
		{"pay \u00a320 now", "20", "gbp"},
		{"pay 1,500.50 USD now", "1500.5", "usd"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			f := fields(t, c.text)
			if got := fieldValue(f, GenericFieldMoney); got != c.want {
				t.Errorf("money from %q = %q, want %q (fields %s)", c.text, got, c.want, f.Signature())
			}
			if got := fieldValue(f, GenericFieldCurrency); got != c.wantCy {
				t.Errorf("currency from %q = %q, want %q", c.text, got, c.wantCy)
			}
		})
	}
}

func TestGenericMoneySpellingsAgree(t *testing.T) {
	sameValue(t, GenericFieldMoney,
		"send $500 to the vendor",
		"send 500 USD to the vendor",
		"send 500 dollars to the vendor",
		"send five hundred dollars to the vendor",
		"send USD 500 to the vendor",
	)
	sameValue(t, GenericFieldCurrency,
		"send $500 to the vendor",
		"send 500 USD to the vendor",
		"send 500 dollars to the vendor",
		"send five hundred dollars to the vendor",
	)
	sameValue(t, GenericFieldMoney,
		"refund Rs 2000 to the buyer",
		"refund 2000 rupees to the buyer",
		"refund INR 2000 to the buyer",
		"refund \u20b92000 to the buyer",
	)
}

// TestGenericMoneyIsNotJustANumber guards the boundary that keeps every
// quantity in every domain out of the money field.
func TestGenericMoneyIsNotJustANumber(t *testing.T) {
	f := fields(t, "book 2 seats")
	if v := fieldValue(f, GenericFieldMoney); v != "" {
		t.Errorf("a bare count was read as money %q; fields %s", v, f.Signature())
	}
	if v := fieldValue(f, GenericFieldNum); v != "2" {
		t.Errorf("the count was not read as a number: got %q", v)
	}
}

func TestGenericDatesNormalise(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"on friday", "friday"},
		{"on Friday", "friday"},
		{"on FRIDAY", "friday"},
		{"on fri", "friday"},
		{"on this friday", "friday"},
		{"on next friday", "friday+1w"},
		{"on next tuesday", "tuesday+1w"},
		{"on tues", "tuesday"},
		{"on 2026-03-12", "2026-03-12"},
		{"on 2026/03/12", "2026-03-12"},
		{"on 12/03/2026", "2026-03-12"},
		{"on 12-03-2026", "2026-03-12"},
		{"on 12.03.2026", "2026-03-12"},
		{"on 12 March 2026", "2026-03-12"},
		{"on March 12 2026", "2026-03-12"},
		{"on 12th March 2026", "2026-03-12"},
		{"on 12 March", "03-12"},
		{"on March 12", "03-12"},
		{"on 12/03", "03-12"},
		{"on tomorrow", "tomorrow"},
		{"on today", "today"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := fieldValue(fields(t, c.text), GenericFieldDate); got != c.want {
				t.Errorf("date from %q = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

func TestGenericDateSpellingsAgree(t *testing.T) {
	sameValue(t, GenericFieldDate,
		"deliver on 2026-03-12",
		"deliver on 12/03/2026",
		"deliver on 12 March 2026",
		"deliver on March 12 2026",
		"deliver on 12th March 2026",
	)
	sameValue(t, GenericFieldDate,
		"meet on friday",
		"meet on Friday",
		"meet on fri",
		"meet on this friday",
	)
}

// TestGenericNextWeekIsADifferentDate is the failure the whole hybrid
// comparator exists to prevent, in its generic form: one word changes the
// identity of the action and the extractor has to say so.
func TestGenericNextWeekIsADifferentDate(t *testing.T) {
	a := fields(t, "meet on friday")
	b := fields(t, "meet on next friday")
	name, av, bv, differs := FirstDifference(a, b)
	if !differs {
		t.Fatalf("friday and next friday extracted identically: %s", a.Signature())
	}
	if name != GenericFieldDate {
		t.Errorf("the difference was reported on %q (%q vs %q), want the date", name, av, bv)
	}
}

func TestGenericClockTimesNormalise(t *testing.T) {
	cases := []struct {
		text              string
		want, wantDaypart string
	}{
		{"at 07:40", "07:40", "am"},
		{"at 7:40am", "07:40", "am"},
		{"at 7:40 am", "07:40", "am"},
		{"at 9am", "09:00", "am"},
		{"at 9 am", "09:00", "am"},
		{"at 9AM", "09:00", "am"},
		{"at 9pm", "21:00", "pm"},
		{"at 21:00", "21:00", "pm"},
		{"at 12am", "00:00", "am"},
		{"at 12pm", "12:00", "pm"},
		{"at 14:30", "14:30", "afternoon"},
		{"at 9 o'clock", "09:00", "am"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			f := fields(t, c.text)
			if got := fieldValue(f, GenericFieldClock); got != c.want {
				t.Errorf("time from %q = %q, want %q (fields %s)", c.text, got, c.want, f.Signature())
			}
			if got := fieldValue(f, FieldDaypart); got != c.wantDaypart {
				t.Errorf("daypart from %q = %q, want %q", c.text, got, c.wantDaypart)
			}
		})
	}
}

func TestGenericClockSpellingsAgree(t *testing.T) {
	sameValue(t, GenericFieldClock,
		"call at 09:00",
		"call at 9am",
		"call at 9 am",
		"call at 9:00am",
	)
	sameValue(t, GenericFieldClock,
		"call at 21:15",
		"call at 9:15pm",
		"call at 9:15 pm",
	)
}

// TestGenericBareNumberIsNotATime guards the rule that keeps counts out of
// the clock field: without it, "2 seats" would be two o'clock.
func TestGenericBareNumberIsNotATime(t *testing.T) {
	for _, text := range []string{"book 2 seats", "refund 9 units", "invite 12 people"} {
		f := fields(t, text)
		if v := fieldValue(f, GenericFieldClock); v != "" {
			t.Errorf("%q produced the time %q; fields %s", text, v, f.Signature())
		}
	}
}

func TestGenericDaypartsNormalise(t *testing.T) {
	cases := []struct{ text, want string }{
		{"friday morning", "am"},
		{"friday AM", "am"},
		{"friday before noon", "am"},
		{"friday afternoon", "afternoon"},
		{"friday evening", "pm"},
		{"friday night", "pm"},
		{"friday PM", "pm"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := fieldValue(fields(t, c.text), FieldDaypart); got != c.want {
				t.Errorf("daypart from %q = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

func TestGenericIdentifiers(t *testing.T) {
	cases := []struct{ text, field, want string }{
		{"refund ORD-123", GenericFieldCode, "ord123"},
		{"refund ord-123", GenericFieldCode, "ord123"},
		{"refund ORD/123", GenericFieldCode, "ord123"},
		{"refund INV/2026/88", GenericFieldCode, "inv202688"},
		{"refund 550e8400-e29b-41d4-a716-446655440000",
			GenericFieldCode, "550e8400e29b41d4a716446655440000"},

		// A '#' in front of nothing but digits is typography, not a
		// different kind of value: both spellings have to land on one field.
		{"refund order #4417", GenericFieldNum, "4417"},
		{"refund order 4417", GenericFieldNum, "4417"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := fieldValue(fields(t, c.text), c.field); got != c.want {
				t.Errorf("%s from %q = %q, want %q (fields %s)",
					c.field, c.text, got, c.want, fields(t, c.text).Signature())
			}
		})
	}
}

func TestGenericIdentifierSpellingsAgree(t *testing.T) {
	sameValue(t, GenericFieldCode,
		"refund ORD-123 please",
		"refund ord-123 please",
		"refund ORD/123 please",
		"refund ORD123 please",
	)
	sameValue(t, GenericFieldNum,
		"refund order #4417 please",
		"refund order 4417 please",
	)
}

func TestGenericEmailAndPhone(t *testing.T) {
	cases := []struct{ text, field, want string }{
		{"email alice@example.com", GenericFieldEmail, "alice@example.com"},
		{"email Alice@Example.COM", GenericFieldEmail, "alice@example.com"},
		{"call 9876543210", GenericFieldPhone, "9876543210"},
		{"call 98765-43210", GenericFieldPhone, "9876543210"},
		{"call 98765 43210", GenericFieldPhone, "9876543210"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			f := fields(t, c.text)
			if got := fieldValue(f, c.field); got != c.want {
				t.Errorf("%s from %q = %q, want %q (fields %s)", c.field, c.text, got, c.want, f.Signature())
			}
		})
	}
}

func TestGenericEmailSpellingsAgree(t *testing.T) {
	sameValue(t, GenericFieldEmail,
		"send it to alice@example.com",
		"send it to Alice@Example.com",
		"send it to ALICE@EXAMPLE.COM",
	)
}

// TestGenericProperNounHeuristic pins down both halves of the one heuristic
// rule: what it accepts, and what it deliberately refuses.
func TestGenericProperNounHeuristic(t *testing.T) {
	t.Run("a mid-sentence capital is a name", func(t *testing.T) {
		if got := fieldValue(fields(t, "send $500 to Alice"), GenericFieldName); got != "alice" {
			t.Errorf("name = %q, want alice", got)
		}
	})
	t.Run("an all-caps code is a name", func(t *testing.T) {
		if got := fieldValue(fields(t, "fly to DEL"), GenericFieldName); got != "del" {
			t.Errorf("name = %q, want del", got)
		}
	})
	t.Run("values are sorted, and a reversed route is therefore invisible", func(t *testing.T) {
		// This pins a KNOWN FAILURE rather than a feature, so that it cannot
		// be forgotten. Values of one field are sorted, because the order in
		// which a planner mentions two values is a property of the sentence
		// — "a P1 ticket on TCK-219" and "TCK-219 as P1" are one ticket. The
		// price is paid here: this extractor has no way to know which place
		// is the origin, so an outbound booking and its return leg extract
		// identically and the gate cannot separate them. The flight domain's
		// reversed-direction negatives measure what that costs.
		got := fieldValue(fields(t, "fly from Bangalore to Delhi"), GenericFieldName)
		if got != "bangalore>delhi" {
			t.Errorf("name = %q, want bangalore>delhi (sorted)", got)
		}
		back := fieldValue(fields(t, "fly from Delhi to Bangalore"), GenericFieldName)
		if back != got {
			t.Errorf("the reversed route extracted as %q and the forward one as %q; the sort "+
				"is supposed to make them equal, and the cost of that is documented", back, got)
		}
	})
	t.Run("the same two values in either order are one value", func(t *testing.T) {
		sameValue(t, GenericFieldCode,
			"open a P1 ticket against TCK-219",
			"file TCK-219 as P1",
		)
	})
	t.Run("a sentence-initial capital is refused", func(t *testing.T) {
		f := fields(t, "Book a seat")
		if v := fieldValue(f, GenericFieldName); v != "" {
			t.Errorf("the opening verb was read as the name %q; fields %s", v, f.Signature())
		}
	})
	t.Run("a capital after a full stop is refused", func(t *testing.T) {
		f := fields(t, "pay Alice. Send the receipt")
		if got := fieldValue(f, GenericFieldName); got != "alice" {
			t.Errorf("name = %q, want alice alone: the word after the full stop opened a "+
				"sentence and is not a value", got)
		}
	})
	t.Run("lower case carries no name", func(t *testing.T) {
		// This is the case that matters on this engine: app/agent lowercases
		// every intent through CanonicalIntent, so the heuristic is inert on
		// its tool calls. The test exists so that fact is asserted rather
		// than discovered in a result file.
		f := fields(t, "send $500 to alice")
		if v := fieldValue(f, GenericFieldName); v != "" {
			t.Errorf("a name was extracted from lower-case text: %q", v)
		}
	})
}

// TestGenericRecognisedOnlyWhenSomethingWasFound pins the rule that stops a
// field set of zero fields from making every call a duplicate of every other.
func TestGenericRecognisedOnlyWhenSomethingWasFound(t *testing.T) {
	t.Run("no value at all", func(t *testing.T) {
		f := fields(t, "please send the usual thing to the usual place")
		if f.Recognised {
			t.Errorf("Recognised with no deciding field; fields %s", f.Signature())
		}
		if len(f.Deciding) != 0 {
			t.Errorf("deciding fields on an unrecognised intent: %s", f.Signature())
		}
	})
	t.Run("one value is enough", func(t *testing.T) {
		f := fields(t, "please send 2 of the usual thing")
		if !f.Recognised {
			t.Errorf("not Recognised although a count was found; fields %s", f.Signature())
		}
	})
}

// TestGenericSurvivesTheJSONEnvelope checks the form the engine actually
// hands an extractor: IntentText wraps the arguments in JSON, so the keys and
// the braces arrive as part of the text.
func TestGenericSurvivesTheJSONEnvelope(t *testing.T) {
	text := `SendPayment({"args":"pay 500 usd to the vendor on friday","tool":"SendPayment"})`
	f, err := GenericFields("SendPayment", text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := fieldValue(f, GenericFieldMoney); got != "500" {
		t.Errorf("money = %q, want 500 (fields %s)", got, f.Signature())
	}
	if got := fieldValue(f, GenericFieldCurrency); got != "usd" {
		t.Errorf("currency = %q, want usd", got)
	}
	if got := fieldValue(f, GenericFieldDate); got != "friday" {
		t.Errorf("date = %q, want friday", got)
	}
	// The envelope's own words must not reach the free text, or every pair of
	// calls would share them and the similarity would be diluted by constants.
	for _, w := range []string{"args", "tool", "sendpayment", "send", "payment"} {
		if strings.Contains(f.Remainder, w) {
			t.Errorf("the remainder %q still contains the scaffolding word %q", f.Remainder, w)
		}
	}
}

// TestGenericIsPure asserts the contract FieldExtractor requires: the same
// text must extract the same way every time, on every node, forever. An
// extractor that read a clock would fail this tomorrow.
func TestGenericIsPure(t *testing.T) {
	const text = "pay Rs 2000 to Alice on 12 March 2026 at 9am, ref ORD-123"
	first := fields(t, text)
	for i := 0; i < 50; i++ {
		again := fields(t, text)
		if again.Signature() != first.Signature() || again.Remainder != first.Remainder {
			t.Fatalf("extraction %d differed:\n  %s | %q\n  %s | %q",
				i, first.Signature(), first.Remainder, again.Signature(), again.Remainder)
		}
	}
}

// TestGenericMultiDomainShapes is a readability check rather than a
// behavioural one: it records, in one place, what the extractor makes of one
// phrasing from each domain the evaluation covers. A change that alters any
// of these is a change to the measured result, and should have to be stated.
func TestGenericMultiDomainShapes(t *testing.T) {
	cases := []struct{ text, want string }{
		{"blr to del friday morning for 2",
			"date=friday|daypart=am|num=2"},
		{"pay 500 usd to the vendor",
			"currency=usd|money=500"},
		{"refund order ORD-4417 as damaged",
			"id=ord4417"},
		{"schedule 30 minutes with the team on tuesday at 14:00",
			"date=tuesday|daypart=afternoon|num=30|time=14:00"},
		{"email alice@example.com about the invoice",
			"email=alice@example.com"},
		// num is "12>3" and not "3>12": values are sorted as strings, which
		// is enough for the equality the gate performs and avoids pretending
		// the extractor knows which number is a quantity and which a flat.
		{"order 3 units of SKU-88 to flat 12",
			"id=sku88|num=12>3"},
		{"open a ticket for TCK-9 about the broken sensor",
			"id=tck9"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := fields(t, c.text).Signature(); got != c.want {
				t.Errorf("signature of %q:\n  got  %s\n  want %s", c.text, got, c.want)
			}
		})
	}
}

// sameValue asserts that every spelling given produces the identical value
// for one field. It is the assertion the comparator's correctness rests on.
func sameValue(t *testing.T, name string, texts ...string) {
	t.Helper()
	if len(texts) < 2 {
		t.Fatalf("sameValue needs at least two spellings")
	}
	want := fieldValue(fields(t, texts[0]), name)
	if want == "" {
		t.Fatalf("%q produced no %s at all, so there is nothing to agree on (fields %s)",
			texts[0], name, fields(t, texts[0]).Signature())
	}
	for _, text := range texts[1:] {
		f := fields(t, text)
		if got := fieldValue(f, name); got != want {
			t.Errorf("%s differs between two spellings of one value:\n  %q -> %q\n  %q -> %q\n"+
				"  (second signature %s)", name, texts[0], want, text, got, f.Signature())
		}
	}
}

// ---------------------------------------------------------------------------
// THE TWO FAILURES THE LIVE RUN FOUND
// ---------------------------------------------------------------------------
//
// docs/results/rephrase-live-model.txt measured the hand-written extractor
// losing two of five genuine rewordings in the only domain it could read, and
// both losses were gate artefacts rather than two different actions:
//
//	daypart "am>am" vs "am"        the same value, rendered twice
//	daypart "am"    vs "(absent)"  the second sentence did not restate it
//
// These two tests are those two cases, pinned so they cannot come back.

// TestGenericRepeatedValueRendersOnce is the first of them. A sentence that
// says the same thing twice must not extract a different value from one that
// says it once.
func TestGenericRepeatedValueRendersOnce(t *testing.T) {
	cases := []struct{ twice, once string }{
		{"friday morning, AM", "friday AM"},
		{"friday morning flight in the morning", "friday morning"},
		{"pay 500 usd, that is 500 usd", "pay 500 usd"},
		{"on friday, this friday", "on friday"},
	}
	for _, c := range cases {
		t.Run(c.twice, func(t *testing.T) {
			twice := fields(t, c.twice)
			once := fields(t, c.once)
			name, av, bv, differs := FirstDifference(twice, once)
			if differs {
				t.Errorf("saying it twice changed the fields: %q differs from %q on %s "+
					"(%q vs %q)\n  twice: %s\n  once : %s",
					c.twice, c.once, name, av, bv, twice.Signature(), once.Signature())
			}
		})
	}
}

// TestAbsentIsNotAConflict is the second. Under the strict gate a field only
// one side stated closes the gate; under the neutral gate it must not, and
// two values that genuinely contradict still must.
func TestAbsentIsNotAConflict(t *testing.T) {
	stated := fields(t, "book a seat to del on friday morning")
	silent := fields(t, "book a seat to del on friday")
	other := fields(t, "book a seat to del on friday evening")

	t.Run("the strict rule calls silence a difference", func(t *testing.T) {
		if _, _, _, differs := FirstDifference(stated, silent); !differs {
			t.Fatalf("FirstDifference no longer separates these; the strict rule is what "+
				"every published hybrid number was measured under and must not change.\n"+
				"  stated: %s\n  silent: %s", stated.Signature(), silent.Signature())
		}
	})
	t.Run("the neutral rule does not", func(t *testing.T) {
		if name, av, bv, differs := FirstConflict(stated, silent); differs {
			t.Errorf("silence was read as a conflict on %s (%q vs %q)\n  stated: %s\n  silent: %s",
				name, av, bv, stated.Signature(), silent.Signature())
		}
	})
	t.Run("the neutral rule still catches a real contradiction", func(t *testing.T) {
		name, av, bv, differs := FirstConflict(stated, other)
		if !differs {
			t.Fatalf("morning and evening passed the neutral gate\n  a: %s\n  b: %s",
				stated.Signature(), other.Signature())
		}
		if name != FieldDaypart {
			t.Errorf("the conflict was reported on %q (%q vs %q), want the daypart", name, av, bv)
		}
	})
	t.Run("agreement is counted, not assumed", func(t *testing.T) {
		if got := CountAgreeing(stated, silent); got < 1 {
			t.Errorf("CountAgreeing = %d; these two agree on the date and the destination, "+
				"so a MinAgreeing of 1 must not reject them\n  a: %s\n  b: %s",
				got, stated.Signature(), silent.Signature())
		}
		none := fields(t, "pay 500 usd")
		if got := CountAgreeing(stated, none); got != 0 {
			t.Errorf("CountAgreeing = %d between two intents with no field in common", got)
		}
	})
}

// TestValueSubsetIsNotAConflict pins the neutral gate's value-level rule: a
// field holding several values is not a contradiction when one side's values
// are contained in the other's. The sentence said less, not something else.
func TestValueSubsetIsNotAConflict(t *testing.T) {
	cases := []struct {
		name     string
		x, y     string
		conflict bool
	}{
		{"one side named a value the other did not", "10>30", "30", false},
		{"identical", "30", "30", false},
		{"two different amounts", "4500", "5400", true},
		{"neither contains the other", "p1>tck219", "p3>tck219", true},
		{"disjoint sets", "chennai>kolkata", "maa>ccu", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Fields{Deciding: []Field{{Name: "f", Value: c.x}}, Recognised: true}
			b := Fields{Deciding: []Field{{Name: "f", Value: c.y}}, Recognised: true}
			_, _, _, got := FirstConflict(a, b)
			if got != c.conflict {
				t.Errorf("FirstConflict(%q, %q) reported conflict=%t, want %t", c.x, c.y, got, c.conflict)
			}
			// The strict rule must be unmoved by any of this: it is the rule
			// every published hybrid figure was measured under.
			_, _, _, strict := FirstDifference(a, b)
			if strict != (c.x != c.y) {
				t.Errorf("FirstDifference(%q, %q) = %t; the strict rule is plain string "+
					"inequality and must not have changed", c.x, c.y, strict)
			}
		})
	}
}
