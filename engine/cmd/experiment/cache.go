package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// The disk cache.
// ---------------------------------------------------------------------------

// A real encoder takes tens of milliseconds per sentence, and this harness
// embeds both sides of every pair in the corpus. Without a cache, changing one
// number in a threshold sweep means paying for every one of those calls again,
// which turns a two-second iteration into a half-minute one and quietly
// discourages re-running the experiment at all.
//
// Two properties are load-bearing:
//
//  1. THE MODEL NAME IS PART OF THE KEY. Vectors from two different encoders
//     live in two different spaces; a cosine between them is a number with no
//     meaning. Serving a vector computed by a different model would not fail
//     loudly, it would silently produce plausible-looking similarity scores
//     that are simply wrong. So the model name goes into the hash AND is
//     written into the file and checked on the way back out. A hash collision,
//     a copied directory, or a hand-edited file all have to get past both.
//
//  2. A DAMAGED ENTRY IS A MISS, NOT AN ERROR. The cache is an optimisation.
//     If a file is truncated, has the wrong magic, or holds the wrong number
//     of components, the right answer is to recompute and overwrite, not to
//     fail a run that would otherwise have succeeded.

// cacheMagic identifies the file format and its version. A future change to
// the layout bumps the last byte, which makes every old entry a clean miss
// rather than a misread.
const cacheMagic = "DUREXVEC\x01"

// cacheStats is what the report says about how much of the run was recomputed.
type cacheStats struct {
	Hits   int
	Misses int
	Writes int
	// Errors counts entries that were present but unusable. A non-zero value
	// is not fatal, but it is worth printing: a cache that never hits because
	// every entry is corrupt looks exactly like a cache that is working.
	Errors int
}

// embedCache is a content-addressed directory of embedding vectors.
//
// The zero value is a disabled cache: every method is a no-op and every lookup
// is a miss, so -cache-dir="" needs no special case at the call sites.
type embedCache struct {
	dir   string
	mu    sync.Mutex
	stats cacheStats
}

// newEmbedCache prepares dir. An empty dir disables the cache entirely.
func newEmbedCache(dir string) (*embedCache, error) {
	c := &embedCache{dir: dir}
	if dir == "" {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the embedding cache directory %s: %w", dir, err)
	}
	return c, nil
}

func (c *embedCache) Enabled() bool { return c != nil && c.dir != "" }

func (c *embedCache) Stats() cacheStats {
	if c == nil {
		return cacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// path is where (model, text) is stored.
//
// The model name is hashed in rather than only used as a subdirectory name so
// that a model name containing a slash, a colon or a path traversal cannot
// escape the cache directory. It is ALSO used as a subdirectory, in a sanitised
// form, purely so a human can see at a glance which encoders a directory holds.
func (c *embedCache) path(model, text string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + text))
	name := hex.EncodeToString(sum[:])
	// Two hex characters of fan-out keeps any one directory to a few hundred
	// entries even for a corpus far larger than this one.
	return filepath.Join(c.dir, sanitizeModelName(model), name[:2], name+".vec")
}

// sanitizeModelName reduces a model name to something safe as one path
// element. It is cosmetic: the hash above already carries the real identity.
func sanitizeModelName(model string) string {
	var b strings.Builder
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "model"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// Get returns the cached vector for (model, text), or ok=false on any miss.
//
// dim is checked because a deployment can keep a model's name and change its
// width; a vector of the wrong width would be rejected by Cosine as coming
// from a different model, which is true but would show up as a silent run of
// zero similarities rather than as a cache problem.
func (c *embedCache) Get(model, text string, dim int) ([]float32, bool) {
	if !c.Enabled() {
		return nil, false
	}
	raw, err := os.ReadFile(c.path(model, text))
	if err != nil {
		c.count(func(s *cacheStats) {
			if errors.Is(err, os.ErrNotExist) {
				s.Misses++
			} else {
				s.Errors++
				s.Misses++
			}
		})
		return nil, false
	}
	vec, gotModel, err := decodeCacheEntry(raw)
	if err != nil || gotModel != model || len(vec) != dim {
		// Anything unexpected is a miss. Recomputing is cheap next to the
		// possibility of comparing vectors from two different encoders.
		c.count(func(s *cacheStats) { s.Errors++; s.Misses++ })
		return nil, false
	}
	c.count(func(s *cacheStats) { s.Hits++ })
	return vec, true
}

// Put stores a vector. A failure to write is deliberately not an error: the
// run can complete perfectly well with a cache it could not populate, and
// failing here would turn a full disk into a failed experiment.
func (c *embedCache) Put(model, text string, vec []float32) {
	if !c.Enabled() {
		return
	}
	p := c.path(model, text)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		c.count(func(s *cacheStats) { s.Errors++ })
		return
	}
	// Write to a temporary file in the same directory and rename, so a run
	// interrupted mid-write leaves no half-written entry for the next run to
	// read back as a vector.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		c.count(func(s *cacheStats) { s.Errors++ })
		return
	}
	_, werr := tmp.Write(encodeCacheEntry(model, vec))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		c.count(func(s *cacheStats) { s.Errors++ })
		return
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		c.count(func(s *cacheStats) { s.Errors++ })
		return
	}
	c.count(func(s *cacheStats) { s.Writes++ })
}

func (c *embedCache) count(f func(*cacheStats)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.stats)
}

// encodeCacheEntry lays out one entry:
//
//	magic          9 bytes
//	model length   2 bytes, big endian
//	model          that many bytes
//	component count  4 bytes, big endian
//	components     4 bytes each, IEEE-754 bits, big endian
//
// float32 bits are stored rather than decimal text so a vector read back is
// bit-identical to the one the model returned. A similarity computed before a
// restart then equals one computed after it, which is the same property the
// pgvector store guarantees for the real path.
func encodeCacheEntry(model string, vec []float32) []byte {
	out := make([]byte, 0, len(cacheMagic)+2+len(model)+4+4*len(vec))
	out = append(out, cacheMagic...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(model)))
	out = append(out, model...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(vec)))
	for _, v := range vec {
		out = binary.BigEndian.AppendUint32(out, math.Float32bits(v))
	}
	return out
}

func decodeCacheEntry(raw []byte) ([]float32, string, error) {
	if len(raw) < len(cacheMagic)+2 || string(raw[:len(cacheMagic)]) != cacheMagic {
		return nil, "", fmt.Errorf("not a cache entry")
	}
	p := len(cacheMagic)
	modelLen := int(binary.BigEndian.Uint16(raw[p : p+2]))
	p += 2
	if len(raw) < p+modelLen+4 {
		return nil, "", fmt.Errorf("truncated header")
	}
	model := string(raw[p : p+modelLen])
	p += modelLen
	n := int(binary.BigEndian.Uint32(raw[p : p+4]))
	p += 4
	if len(raw) != p+4*n {
		return nil, "", fmt.Errorf("body is %d bytes, header declares %d components", len(raw)-p, n)
	}
	vec := make([]float32, n)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.BigEndian.Uint32(raw[p : p+4]))
		p += 4
	}
	return vec, model, nil
}

// ---------------------------------------------------------------------------
// The embedder wrappers.
// ---------------------------------------------------------------------------

// cachingEmbedder serves Embed from disk when it can.
//
// It wraps a core.Embedder rather than replacing one, so the thing under test
// is still the engine's own embedder and the cache is visibly a harness
// concern that the engine knows nothing about.
type cachingEmbedder struct {
	inner core.Embedder
	cache *embedCache

	// mu guards missLatencies, which is written from Embed and read once at
	// the end of the run.
	mu            sync.Mutex
	missLatencies []time.Duration
}

var _ core.Embedder = (*cachingEmbedder)(nil)

func newCachingEmbedder(inner core.Embedder, cache *embedCache) *cachingEmbedder {
	return &cachingEmbedder{inner: inner, cache: cache}
}

func (e *cachingEmbedder) Model() string { return e.inner.Model() }
func (e *cachingEmbedder) Dim() int      { return e.inner.Dim() }

func (e *cachingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	model := e.inner.Model()
	if vec, ok := e.cache.Get(model, text, e.inner.Dim()); ok {
		return vec, nil
	}
	start := time.Now()
	vec, err := e.inner.Embed(ctx, text)
	elapsed := time.Since(start)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.missLatencies = append(e.missLatencies, elapsed)
	e.mu.Unlock()

	e.cache.Put(model, text, vec)
	return vec, nil
}

// MissLatencies returns how long the underlying embedder took on the calls the
// cache could not serve. These are the only real model round-trips in the run,
// so they are the honest answer to "what does one embedding cost?" — separate
// from the per-check latency, which after warm-up reads from the cache.
func (e *cachingEmbedder) MissLatencies() []time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]time.Duration(nil), e.missLatencies...)
}

// retryEmbedder retries a transient embedding failure with exponential backoff.
//
// It sits BELOW the cache in the stack, so a retry only ever happens on a call
// that genuinely has to reach the service.
//
// Only core.ErrEmbeddingUnavailable is retried, because that is the one error
// the HTTP embedder raises for every transport-level problem — a refused
// connection, a timeout, a 503 from a model that is still loading. A context
// that the caller cancelled is not retried: the caller has already said to
// stop, and sleeping afterwards would only delay the exit.
type retryEmbedder struct {
	inner    core.Embedder
	attempts int
	base     time.Duration
	// sleep is injected so a test can exercise the backoff without waiting.
	sleep func(time.Duration)

	mu       sync.Mutex
	retries  int
	failures int
}

var _ core.Embedder = (*retryEmbedder)(nil)

func newRetryEmbedder(inner core.Embedder, attempts int, base time.Duration) *retryEmbedder {
	if attempts < 1 {
		attempts = 1
	}
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	return &retryEmbedder{inner: inner, attempts: attempts, base: base, sleep: time.Sleep}
}

func (e *retryEmbedder) Model() string { return e.inner.Model() }
func (e *retryEmbedder) Dim() int      { return e.inner.Dim() }

func (e *retryEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	var lastErr error
	for attempt := 0; attempt < e.attempts; attempt++ {
		if attempt > 0 {
			// 1x, 2x, 4x … of the base delay, capped so a long sweep cannot
			// stall for minutes on one dead endpoint.
			wait := e.base << (attempt - 1)
			if wait > 5*time.Second {
				wait = 5 * time.Second
			}
			e.count(func() { e.retries++ })
			e.sleep(wait)
		}
		vec, err := e.inner.Embed(ctx, text)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		if !errors.Is(err, core.ErrEmbeddingUnavailable) || ctx.Err() != nil {
			break
		}
	}
	e.count(func() { e.failures++ })
	return nil, lastErr
}

func (e *retryEmbedder) count(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f()
}

// Counters reports how many retries were spent and how many calls still failed
// after all of them. A run with failures is an incomplete run.
func (e *retryEmbedder) Counters() (retries, failures int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.retries, e.failures
}
