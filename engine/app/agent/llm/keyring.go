package llm

import (
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// SEVERAL KEYS, ROTATED — AND WHY THAT IS NOT CHEATING
// ---------------------------------------------------------------------------
//
// The provider's free tier limits TOKENS PER MINUTE, not requests: the
// headers report 8,000 tokens a minute against a roughly 650-token extraction
// prompt, which is about eleven intents a minute per key. Requests were never
// the constraint — the same headers reported 999 requests remaining.
//
// A measurement that takes thirty hours is a measurement nobody gets, so the
// quota is widened the only honest way available: by using more than one
// account's quota, each with its own independent limit. Nothing about the
// MEASUREMENT changes. The model, the prompt, the temperature and the text
// are identical whichever key carries the request, and the extraction a model
// returns does not depend on whose quota paid for it. The rate is not a
// variable under test; it is a property of the harness, and the result file
// says so explicitly.
//
// WHAT THIS IS NOT. It is not a way around a per-user limit on one account.
// The keys belong to separate accounts whose quotas are genuinely separate,
// which the headers confirm by reporting each key's remaining budget
// independently.
//
// # The rotation rule
//
// Round-robin, with a cooling-off list. A key that answers 429 is set aside
// for as long as the provider's Retry-After says, and the NEXT key is tried
// immediately with no sleep at all. Only when every key is cooling does the
// caller wait, and then only until the first one is free. That is what turns
// a rate limit from a stall into a queue.
//
// # Keys never appear anywhere
//
// A key is identified in logs and errors by its POSITION ONLY — "key #2" —
// and the material is never formatted, logged, or written to disk. Label() is
// the only way to name a key and it cannot return key material.

// keyring holds every configured API key and hands them out in turn.
//
// It is safe for concurrent use: the field extractor is called from whatever
// goroutine the engine's check is running on, and a measurement harness may
// have several in flight.
type keyring struct {
	mu   sync.Mutex
	keys []string

	// next is the round-robin cursor.
	next int

	// coolUntil[i] is the time key i may be used again. The zero value means
	// the key is available now.
	coolUntil []time.Time
}

// keyEnvPrefix is the environment variable prefix the keys are read from.
const keyEnvPrefix = "GROQ_API_KEY"

// newKeyring reads every GROQ_API_KEY* variable in the environment.
//
//	GROQ_API_KEY      the original, and the one the engine documents
//	GROQ_API_KEY_2    any further keys, in any suffix, from other accounts
//	GROQ_API_KEY_FOO  accepted too: the suffix is a name, not an index
//
// Order is deterministic — GROQ_API_KEY first, then the rest sorted by
// variable name — so two runs rotate in the same order and a reader of a log
// can tell which key "key #2" was.
//
// Duplicates are dropped. Two variables holding the same key would otherwise
// look like twice the quota and produce a rate the provider will refuse.
func newKeyring() *keyring {
	seen := map[string]bool{}
	var primary string
	var named []struct{ name, key string }

	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name, value := kv[:eq], strings.TrimSpace(kv[eq+1:])
		if value == "" || !strings.HasPrefix(name, keyEnvPrefix) {
			continue
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		if name == keyEnvPrefix {
			primary = value
			continue
		}
		named = append(named, struct{ name, key string }{name, value})
	}
	sort.Slice(named, func(i, j int) bool { return named[i].name < named[j].name })

	kr := &keyring{}
	if primary != "" {
		kr.keys = append(kr.keys, primary)
	}
	for _, n := range named {
		kr.keys = append(kr.keys, n.key)
	}
	kr.coolUntil = make([]time.Time, len(kr.keys))
	return kr
}

// newKeyringOf wraps one key, for a caller that was handed a key directly
// rather than reading the environment.
func newKeyringOf(key string) *keyring {
	return &keyring{keys: []string{key}, coolUntil: make([]time.Time, 1)}
}

// Len is how many distinct keys are configured. It is the only number about
// the keys that is ever reported.
func (k *keyring) Len() int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.keys)
}

// Label names a key by position. It is the ONLY way to identify a key in a
// message, and it cannot leak material.
func (k *keyring) Label(idx int) string {
	return "key #" + itoaSmall(idx+1)
}

// take hands out the next usable key.
//
// It returns the key's index and material when one is free. When every key is
// cooling it returns ok=false and how long until the first becomes
// available, so the caller can wait exactly that long instead of guessing.
func (k *keyring) take(now time.Time) (idx int, key string, wait time.Duration, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.keys) == 0 {
		return 0, "", 0, false
	}

	soonest := time.Duration(1 << 62)
	for i := 0; i < len(k.keys); i++ {
		j := (k.next + i) % len(k.keys)
		if !k.coolUntil[j].After(now) {
			k.next = (j + 1) % len(k.keys)
			return j, k.keys[j], 0, true
		}
		if d := k.coolUntil[j].Sub(now); d < soonest {
			soonest = d
		}
	}
	return 0, "", soonest, false
}

// penalise sets a key aside for d, because the provider refused it.
//
// A zero or negative d is replaced by a short default: a 429 with no
// Retry-After still means "not this key, not right now", and treating it as
// "available immediately" would spin.
func (k *keyring) penalise(idx int, d time.Duration) {
	if d <= 0 {
		d = 5 * time.Second
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if idx >= 0 && idx < len(k.coolUntil) {
		until := time.Now().Add(d)
		if until.After(k.coolUntil[idx]) {
			k.coolUntil[idx] = until
		}
	}
}

// itoaSmall formats a small positive integer without pulling in strconv for
// one call site.
func itoaSmall(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 && i > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
