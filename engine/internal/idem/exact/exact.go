// Package exact is the default idempotency strategy: two activity invocations
// are the same invocation when their arguments are literally the same data.
//
// "Literally the same data" is doing real work here. Two JSON documents can
// describe exactly the same arguments and still be different byte strings —
// the keys can be in a different order, there can be extra whitespace, and a
// number can be written as 1, 1.0 or 1e0. None of those differences mean
// anything to the activity, but all of them would change a hash taken over the
// raw bytes. So before hashing we rewrite the document into one agreed form:
// object keys sorted, whitespace removed, numbers written one way. That
// rewritten form is called the canonical form, and hashing it gives a key that
// is the same every time the same arguments are passed, no matter how the
// caller happened to spell them.
//
// This package is pure: no database, no clock, no network. That matters
// because the key it produces is written into history and must be reproducible
// during replay, potentially years later on a different machine.
package exact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Canonicalize rewrites a JSON document into its canonical byte form.
//
// The rules are:
//
//   - object keys are sorted, so {"b":1,"a":2} and {"a":2,"b":1} agree;
//   - array order is preserved, because in an array the order is part of the
//     data — [1,2] and [2,1] are genuinely different arguments;
//   - all insignificant whitespace is dropped;
//   - numbers are normalised, so 1, 1.0 and 1e0 all become "1".
//
// An empty or nil input canonicalises to the JSON literal null. Activities
// that take no arguments are common, and treating that as an error would push
// a pointless special case onto every caller.
func Canonicalize(input json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return []byte("null"), nil
	}

	// UseNumber keeps the number as the text the caller wrote instead of
	// letting encoding/json convert it to float64 first. Without it a large
	// integer id would silently lose precision on the way in, and the key we
	// derive would collide with a neighbouring id.
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("exact: parse input: %w", err)
	}
	// Anything after the first value means the caller handed us two documents
	// glued together. Accepting it would make the canonical form depend on
	// which half we happened to read.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("exact: input contains more than one JSON value")
	}

	var buf bytes.Buffer
	if err := writeValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Key is the content hash that identifies one activity invocation: the
// activity type plus its canonical arguments.
//
// The type is part of the hash because the same arguments mean different
// things to different activities — charge_payment({"id":7}) and
// refund_payment({"id":7}) must never share a key. The two halves are joined
// with a NUL byte, which cannot occur in a Go identifier or in canonical JSON,
// so no pair of (type, args) can be re-split into a different pair and produce
// the same hash.
func Key(activityType string, input json.RawMessage) (string, error) {
	canon, err := Canonicalize(input)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(activityType))
	h.Write([]byte{0})
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeValue emits one value in canonical form. It walks the decoded tree
// rather than doing text surgery on the original bytes, because the decoder
// has already resolved the awkward parts — escape sequences, duplicate keys,
// nesting — and re-encoding from the tree is the only way to be sure two
// equivalent documents converge on the same output.
func writeValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		return writeNumber(buf, t)
	case string:
		return writeString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeValue(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("exact: unsupported JSON value of type %T", v)
	}
	return nil
}

// writeString delegates escaping to encoding/json so that the escaping rules
// are the standard library's and not a second, subtly different set of ours.
func writeString(buf *bytes.Buffer, s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("exact: encode string: %w", err)
	}
	buf.Write(b)
	return nil
}

// maxExactFloatInt is 2^53: above it a float64 can no longer represent every
// whole number, so we must not claim a float is an integer.
const maxExactFloatInt = float64(1 << 53)

// writeNumber picks one spelling for every number.
//
// Integers are written as plain decimal, which keeps ids exact all the way to
// the full 64-bit range. Anything else goes through float64 and is printed
// with the shortest text that reads back as the same float64, so 1.50 and 1.5
// converge. A value that is whole after that round trip is printed without the
// decimal point, which is what makes 1.0 and 1 agree.
func writeNumber(buf *bytes.Buffer, n json.Number) error {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("exact: normalise number %q: %w", s, err)
	}
	if f == math.Trunc(f) && math.Abs(f) < maxExactFloatInt {
		buf.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	}
	buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}
