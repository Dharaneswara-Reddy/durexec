package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// Canonical JSON, and why the replay signature needs it.
//
// THE DEFECT THIS CLOSES. The replay signature used to be "activity:" plus the
// activity type and nothing else. New code that called the SAME activity type
// at the SAME k with DIFFERENT arguments therefore matched the recorded
// signature, replay returned the old result, and the workflow carried on
// believing the new call had happened. ChargePayment(100) replayed under code
// that now asks for ChargePayment(200) resolved as if 200 had been charged, and
// nothing in the log showed it. That was silent and unbounded, and strictly
// worse than the loud divergence the engine already detected.
//
// WHY THE BYTES CANNOT BE COMPARED DIRECTLY. The obvious fix — store the input
// bytes and compare them — is unsafe, and the codebase already says so in two
// places: re-marshalling a decoded value is not guaranteed to produce the same
// bytes on a later Go version, and Go's map iteration order is not part of its
// JSON output contract for arbitrary values. One side of the comparison is
// always a fresh marshal of a live Go value, so a byte comparison would raise
// false divergences on perfectly correct code.
//
// WHAT CANONICALISE DOES, EXACTLY. Each side is parsed into a generic value and
// re-serialised by these rules:
//
//   - OBJECT KEYS are sorted by byte order, so {"b":1,"a":2} and {"a":2,"b":1}
//     canonicalise identically. Duplicate keys cannot survive parsing; the last
//     occurrence wins, which is what encoding/json does.
//   - ARRAY ORDER IS PRESERVED. Order in a list is meaning, not formatting.
//   - NUMBERS are normalised through math/big.Rat, so 1, 1.0, 1e0 and 0.1e1 all
//     canonicalise to "1", and 0.5 and 5e-1 both to "1/2". Using big.Rat rather
//     than float64 is deliberate: a float64 round trip silently merges values
//     that differ past the 17th significant digit, and an input is allowed to
//     be an account number or an amount in minor units. A literal big.Rat
//     cannot parse — which JSON does not permit anyway — is kept verbatim so
//     the function stays total.
//   - STRINGS are re-encoded by encoding/json, whose escaping is fixed, so two
//     spellings of the same string ("A" and "A") agree.
//   - true, false and null are literals.
//   - AN ABSENT FIELD IS NOT EQUAL TO AN EXPLICIT NULL. {"a":null} and {} get
//     different digests. This is the one place the rules are deliberately
//     strict rather than clever: whether a missing field means "null", "zero"
//     or "use the default" is a property of the workflow's own Go type, which
//     this package cannot see. Treating them as equal could silently reuse a
//     result for a call that is genuinely different, which is the defect being
//     fixed; treating them as different can only raise a divergence the
//     operator will see. False alarm beats silent reuse.
//
// WHAT IS NOT PROMISED. Two inputs that a workflow's own Go type would consider
// equal may still digest differently — a struct field that gained a
// `json:",omitempty"` tag is the common case, because the field disappears from
// the encoding. That is a genuine code change at a k whose result is already
// recorded, and the engine reporting it rather than guessing is the intended
// behaviour.
//
// The digest is NEVER PERSISTED. It is recomputed from the stored input bytes
// on every replay, and both sides of a comparison are always produced by the
// same binary, so these rules may be changed in a later version without
// invalidating any history already written.

// inputDigest returns a short, stable digest of an activity input.
//
// The empty input and the JSON literal null digest the same, because a workflow
// that passes nil and one that passes no argument are asking for the same call:
// marshalInput already turns both into "null" before anything is stored.
func inputDigest(raw json.RawMessage) string {
	canon := canonicalise(raw)
	sum := sha256.Sum256([]byte(canon))
	// Half of SHA-256 is far more than enough to make an accidental collision
	// between two inputs of one workflow impossible in practice, and it keeps
	// the signature short enough to read in a log line.
	return hex.EncodeToString(sum[:16])
}

// canonicalise renders raw in the canonical form documented above. It is total:
// anything it cannot parse is returned as its own bytes, prefixed so that
// unparseable input can never collide with a canonical rendering of valid JSON.
func canonicalise(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "null"
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	// UseNumber keeps every number as the literal text that was written, so the
	// normalisation below works on what the input actually said rather than on
	// a float64 the decoder chose for it.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "raw:" + trimmed
	}
	// Trailing content means this was not one JSON value, so it is not
	// something the rules above describe.
	if dec.More() {
		return "raw:" + trimmed
	}
	var b strings.Builder
	writeCanonical(&b, v)
	return b.String()
}

func writeCanonical(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(canonicalNumber(t))
	case string:
		// encoding/json's string escaping is fixed, so this is deterministic.
		// The error cannot happen for a string, and ignoring it would hide a
		// future change, so it is rendered rather than dropped.
		enc, err := json.Marshal(t)
		if err != nil {
			b.WriteString(fmt.Sprintf("raw:%q", t))
			return
		}
		b.Write(enc)
	case []any:
		b.WriteByte('[')
		for i, el := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, el)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// Byte order, not locale order: the result must not depend on where the
		// process is running.
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, k)
			b.WriteByte(':')
			writeCanonical(b, t[k])
		}
		b.WriteByte('}')
	default:
		// Unreachable for anything encoding/json produces with UseNumber, but
		// the function stays total rather than panicking inside replay.
		b.WriteString(fmt.Sprintf("raw:%v", t))
	}
}

// canonicalNumber renders a JSON number as an exact rational in lowest terms,
// so every spelling of one value gives one string. A literal big.Rat refuses —
// which valid JSON does not contain — keeps its own text, so the function is
// total and two different literals can never be merged by accident.
func canonicalNumber(n json.Number) string {
	r, ok := new(big.Rat).SetString(n.String())
	if !ok {
		return "num:" + n.String()
	}
	return r.RatString()
}
