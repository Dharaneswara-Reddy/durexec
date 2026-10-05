package exact

import (
	"encoding/json"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty input is null", "", "null"},
		{"whitespace only is null", "   \n\t ", "null"},
		{"null literal", "null", "null"},
		{"true", "true", "true"},
		{"object keys are sorted", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"nested keys are sorted", `{"z":{"y":1,"x":2}}`, `{"z":{"x":2,"y":1}}`},
		{"whitespace is dropped", "{\n  \"a\" : 1 ,\n  \"b\" : 2\n}", `{"a":1,"b":2}`},
		{"array order is preserved", `[3,1,2]`, `[3,1,2]`},
		{"objects inside arrays are sorted", `[{"b":1,"a":2}]`, `[{"a":2,"b":1}]`},
		{"trailing zero float becomes int", `{"n":1.0}`, `{"n":1}`},
		{"exponent becomes int", `{"n":1e3}`, `{"n":1000}`},
		{"redundant float digits collapse", `{"n":1.50}`, `{"n":1.5}`},
		{"large integers keep full precision", `{"id":9007199254740993}`, `{"id":9007199254740993}`},
		{"negative zero float", `{"n":-0.0}`, `{"n":0}`},
		{"empty object", `{}`, `{}`},
		{"empty array", `[]`, `[]`},
		{"unicode string", `{"s":"BLR → DEL"}`, `{"s":"BLR → DEL"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonicalize(json.RawMessage(tc.input))
			if err != nil {
				t.Fatalf("Canonicalize(%q) returned error: %v", tc.input, err)
			}
			if string(got) != tc.want {
				t.Fatalf("Canonicalize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestCanonicalizeStableUnderReordering is the property the whole package
// exists for: two spellings of the same arguments must produce the same bytes.
func TestCanonicalizeStableUnderReordering(t *testing.T) {
	pairs := [][2]string{
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`},
		{`{"a":1,"b":2}`, "{ \"b\" : 2 ,\n\"a\" : 1 }"},
		{`{"q":"BLR to DEL","limit":5}`, `{"limit":5,"q":"BLR to DEL"}`},
		{`{"amount":450.00}`, `{"amount":450}`},
		{`{"o":{"x":1,"y":[1,{"b":1,"a":2}]}}`, `{"o":{"y":[1,{"a":2,"b":1}],"x":1}}`},
	}
	for _, p := range pairs {
		a, err := Canonicalize(json.RawMessage(p[0]))
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", p[0], err)
		}
		b, err := Canonicalize(json.RawMessage(p[1]))
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", p[1], err)
		}
		if string(a) != string(b) {
			t.Fatalf("%q and %q canonicalised differently: %q vs %q", p[0], p[1], a, b)
		}
	}
}

func TestCanonicalizeErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"not json", `{`},
		{"bare word", `banana`},
		{"two documents", `{"a":1}{"b":2}`},
		{"trailing garbage", `1 2`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Canonicalize(json.RawMessage(tc.input)); err == nil {
				t.Fatalf("Canonicalize(%q) should have failed", tc.input)
			}
		})
	}
}

func TestKeyStableUnderReordering(t *testing.T) {
	a, err := Key("search_flights", json.RawMessage(`{"q":"BLR to DEL","limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Key("search_flights", json.RawMessage("{ \"limit\" : 5, \"q\":\"BLR to DEL\" }"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("reordered keys gave different hashes: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected a 64-character sha256 hex digest, got %d characters", len(a))
	}
}

// TestKeySeparatesActivityType guards the NUL separator: the same arguments
// under a different activity must never collide.
func TestKeySeparatesActivityType(t *testing.T) {
	charge, err := Key("charge_payment", json.RawMessage(`{"id":7}`))
	if err != nil {
		t.Fatal(err)
	}
	refund, err := Key("refund_payment", json.RawMessage(`{"id":7}`))
	if err != nil {
		t.Fatal(err)
	}
	if charge == refund {
		t.Fatal("two different activity types produced the same key")
	}

	// The classic splice attack on a concatenated hash: "ab"+"c" vs "a"+"bc".
	// The NUL byte cannot occur in canonical JSON, so these must differ.
	x, err := Key("ab", json.RawMessage(`"c"`))
	if err != nil {
		t.Fatal(err)
	}
	y, err := Key("a", json.RawMessage(`"bc"`))
	if err != nil {
		t.Fatal(err)
	}
	if x == y {
		t.Fatal("activity type and arguments can be re-split into the same key")
	}
}

func TestKeyDiffersOnArrayOrder(t *testing.T) {
	a, err := Key("book", json.RawMessage(`{"legs":[1,2]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Key("book", json.RawMessage(`{"legs":[2,1]}`))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("array order must be significant: [1,2] and [2,1] are different arguments")
	}
}
