package activity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"github.com/dharaneswar339/durexec/internal/core"
)

// dedupScheme versions the key format. It is part of the hashed input as well
// as the printed prefix, so if the recipe ever changes, old and new keys can
// never collide and an operator can tell at a glance which one they are looking
// at. Changing it changes every key, so it must not be changed for a workflow
// that is already running.
const dedupScheme = "dx1"

// DedupKey is the value the engine hands to an external service as its
// Idempotency-Key.
//
// The problem it solves is the one that makes exactly-once delivery impossible
// and exactly-once *effect* possible. A worker calls a payment API, the call
// succeeds, and the worker dies before it can write that down. The task lease
// expires, another worker picks the task up, and calls the payment API again.
// Nothing the engine does on its own can prevent that second call: the engine
// cannot know whether the first one was received. What it can do is make the
// second call carry the same key as the first, so the payment provider
// recognises it as a repeat and returns the original result instead of charging
// the customer twice. The guarantee is therefore not "we call at most once", it
// is "however many times we call, the effect happens once" — and it is a
// contract with the downstream service, not a property of the engine alone.
//
// For that to work the key must be identical on every retry, on every replay,
// and after every crash. So it is built from exactly three things:
//
//	workflowID — the business key, chosen by the caller
//	runID      — this attempt at that business key
//	k          — the program-order index of this call within the run
//
// and from nothing else. Not the time, because a retry happens at a different
// time. Not the attempt number, because retry number two must look identical to
// retry number one. Not a random value, obviously. Not the input, because two
// calls with the same input at different points of a workflow are genuinely
// different calls, and because re-serialising an input is not guaranteed to
// produce the same bytes twice. Because k is assigned by deterministic replay,
// it is the same integer on the first execution and on the hundredth, which is
// what makes the whole key stable across a crash.
//
// The output is a fixed-length hex digest rather than the raw fields joined
// together, because a workflow ID is caller-supplied and can be long, contain
// anything, or be commercially sensitive, and it is about to be put in an HTTP
// header on somebody else's server.
func DedupKey(workflowID string, runID core.RunID, k int) string {
	h := sha256.New()
	// Every field is written with its length in front. Without that, a
	// separator byte appearing inside a workflow ID would let two different
	// runs produce the same preimage, and two different payments would share an
	// idempotency key. Length prefixing makes the encoding unambiguous.
	writeField(h, []byte(dedupScheme))
	writeField(h, []byte(workflowID))
	writeField(h, []byte(runID))
	var kb [8]byte
	binary.BigEndian.PutUint64(kb[:], uint64(int64(k)))
	writeField(h, kb[:])

	return dedupScheme + ":" + hex.EncodeToString(h.Sum(nil))
}

type byteWriter interface{ Write([]byte) (int, error) }

func writeField(h byteWriter, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	// hash.Hash.Write never returns an error, which is why these are ignored.
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}
