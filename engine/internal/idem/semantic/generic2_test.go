package semantic

import (
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

func TestGenericV2Basics(t *testing.T) {
	cases := []struct {
		act, a, b string
		want      core.DedupVerdict
	}{
		{"SendPayment", "pay acme supplies 4500 inr", "send inr 4,500 to acme supplies", core.VerdictDuplicate},
		{"SendPayment", "pay acme supplies 4500 inr", "pay acme supplies 5400 inr", core.VerdictDistinct},
		{"SendPayment", "pay acme supplies 4500 inr", "pay borzoi freight 4500 inr", core.VerdictDistinct},
		{"BookFlight", "blr to del friday morning", "del to blr friday morning", core.VerdictDistinct},
		{"BookFlight", "blr to del friday morning", "blr to del next friday morning", core.VerdictDistinct},
		{"BookFlight", "blr to del friday morning", "blr to del friday evening", core.VerdictDistinct},
		{"ScheduleMeeting", "schedule 30 minutes with priya on thursday at 10:00", "put a half hour with priya in the diary for thursday 10am", core.VerdictDuplicate},
		{"DispatchParcel", "dispatch pcl-3380 to pune by express", "dispatch pcl-3380 to pune by standard", core.VerdictDistinct},
		{"BookFlight", "book flight 6e-2113 from goa to mumbai on sunday morning for three people", "book flight 6e-2113 from goa to mumbai on sunday morning for 3 passengers", core.VerdictDuplicate},
	}
	for _, c := range cases {
		got := GenericV2Compare(c.act, c.a, c.b)
		if got.Verdict != c.want {
			t.Errorf("%q vs %q: got %s (%s), want %s", c.a, c.b, got.Verdict, got.Reason, c.want)
		}
	}
	if v := GenericV2PlacesCompare("BookFlight", "maa to ccu tuesday morning", "chennai to kolkata, tuesday am"); v.Verdict != core.VerdictDuplicate {
		t.Errorf("places: got %s (%s)", v.Verdict, v.Reason)
	}
	if v := GenericV2Compare("BookFlight", "maa to ccu tuesday morning", "chennai to kolkata, tuesday am"); v.Verdict == core.VerdictDuplicate {
		t.Errorf("without places the alias cannot be known; got DUPLICATE")
	}
}

func TestGenericV2IsSymmetricOnCorpusShapes(t *testing.T) {
	pairs := [][3]string{
		{"IssueRefund", "refund ord-4417 because the goods arrived damaged", "refund ord-4417 because it was delivered late"},
		{"BookFlight", "one seat bengaluru to new delhi, friday before noon", "one seat new delhi to bengaluru, friday before noon"},
	}
	for _, p := range pairs {
		x := GenericV2Compare(p[0], p[1], p[2])
		y := GenericV2Compare(p[0], p[2], p[1])
		if x.Verdict != y.Verdict {
			t.Errorf("asymmetric: %s vs %s", x.Verdict, y.Verdict)
		}
	}
}
