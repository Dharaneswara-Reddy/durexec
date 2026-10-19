package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// The embedding cache, in cmd/experiment's format on purpose
// ---------------------------------------------------------------------------
//
// Same directory, same file naming, same byte layout as cmd/experiment's
// cache. That is not tidiness: the flight corpus has already been embedded by
// that harness, and nine comparators over three corpora re-embed the same few
// hundred sentences many times over. Sharing the cache means those vectors
// are read from disk instead of bought again, and — more importantly — that a
// similarity this harness computes is bit-identical to the one the published
// experiment computed, because it is the same stored vector.
//
// The format is documented in cmd/experiment/cache.go. It is duplicated here
// rather than imported because both harnesses are package main; the layout is
// pinned by cacheMagic, and a change on either side would be caught by the
// magic mismatch rather than silently producing wrong vectors.

// cacheMagic must match cmd/experiment's exactly, or entries written by one
// harness are unreadable by the other.
const cacheMagic = "DUREXVEC\x01"

type embedCache struct {
	dir   string
	model string
	dim   int
}

func newEmbedCache(dir, model string) (*embedCache, error) {
	if dir == "" {
		return &embedCache{}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create embedding cache %s: %w", dir, err)
	}
	return &embedCache{dir: dir, model: model}, nil
}

func (c *embedCache) enabled() bool { return c != nil && c.dir != "" }

func (c *embedCache) path(text string) string {
	sum := sha256.Sum256([]byte(c.model + "\x00" + text))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(c.dir, sanitizeModelName(c.model), name[:2], name+".vec")
}

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

func (c *embedCache) Get(text string) ([]float32, bool) {
	if !c.enabled() {
		return nil, false
	}
	raw, err := os.ReadFile(c.path(text))
	if err != nil {
		return nil, false
	}
	vec, model, err := decodeCacheEntry(raw)
	if err != nil || model != c.model {
		// A mismatch is a miss, never an error: the cache is an optimisation
		// and a corrupt entry must cost one model call, not the run.
		return nil, false
	}
	return vec, true
}

func (c *embedCache) Put(text string, vec []float32) {
	if !c.enabled() || len(vec) == 0 {
		return
	}
	p := c.path(text)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	// Write then rename, so a cache file is never half a vector: a reader
	// that saw one would compute a similarity against nonsense.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, encodeCacheEntry(c.model, vec), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

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
