package semantic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// DefaultModel and DefaultDim describe the sentence-transformer the evaluation
// was run against. The dimension is fixed in the schema too — activity_intents
// declares VECTOR(384) — so changing the model means a migration, not a config
// flag. That rigidity is deliberate: vectors from two different models are not
// comparable, and silently mixing them would corrupt every verdict in the run.
const (
	DefaultModel = "all-MiniLM-L6-v2"
	DefaultDim   = 384

	// DefaultTimeout bounds one embed call. The semantic check sits on the
	// workflow path, so a model server that hangs would otherwise hold a
	// workflow task lock open until it expired. Failing fast into the
	// DISTINCT fallback is strictly better than stalling the shard.
	DefaultTimeout = 2 * time.Second
)

// HTTPEmbedder calls a small embedding service over HTTP: it POSTs
// {"text": "..."} and expects {"embedding": [...]} back.
//
// It is stdlib net/http on purpose. The engine already carries a database
// driver and a metrics client; an inference SDK would add a dependency whose
// only job is to marshal one field.
type HTTPEmbedder struct {
	endpoint string
	client   *http.Client
	model    string
	dim      int
}

var _ core.Embedder = (*HTTPEmbedder)(nil)

// NewHTTPEmbedder builds an embedder pointed at endpoint. A zero timeout means
// DefaultTimeout.
func NewHTTPEmbedder(endpoint string, timeout time.Duration) *HTTPEmbedder {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &HTTPEmbedder{
		endpoint: endpoint,
		client:   &http.Client{Timeout: timeout},
		model:    DefaultModel,
		dim:      DefaultDim,
	}
}

// WithModel overrides the advertised model name and dimension, for a
// deployment that has migrated the schema to a different encoder.
func (e *HTTPEmbedder) WithModel(name string, dim int) *HTTPEmbedder {
	e.model = name
	e.dim = dim
	return e
}

func (e *HTTPEmbedder) Model() string { return e.model }
func (e *HTTPEmbedder) Dim() int      { return e.dim }

type embedRequest struct {
	Text string `json:"text"`
}

type embedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed returns the vector for text.
//
// Every transport-level failure — refused connection, timeout, non-200 status,
// unparseable body, wrong dimension — collapses to core.ErrEmbeddingUnavailable.
// The caller has exactly one sensible response to all of them (degrade to
// DISTINCT and journal that it did), so distinguishing them here would only
// invite a caller to handle one case differently and get it wrong.
func (e *HTTPEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Text: text})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", core.ErrEmbeddingUnavailable, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", core.ErrEmbeddingUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrEmbeddingUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", core.ErrEmbeddingUnavailable, resp.StatusCode)
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", core.ErrEmbeddingUnavailable, err)
	}
	if len(out.Embedding) != e.dim {
		// A vector of the wrong width cannot be compared with the ones already
		// stored, and the database column would reject it anyway.
		return nil, fmt.Errorf("%w: expected %d dimensions, got %d",
			core.ErrEmbeddingUnavailable, e.dim, len(out.Embedding))
	}
	return out.Embedding, nil
}

// FakeEmbedder produces deterministic pseudo-embeddings from a hash of the
// text, with no model server anywhere.
//
// It lives in a normal source file rather than a _test.go file on purpose. Its
// job is not only to serve unit tests: it lets the entire engine — API,
// dispatcher, workers, the semantic path included — be started and demonstrated
// end to end on a laptop with nothing else running. A reviewer can watch a
// DUPLICATE verdict get journaled and an activity get skipped without first
// deploying an inference service.
//
// What it faithfully reproduces is that identical intent text yields an
// identical vector, so an exact repeat scores 1.0 and is classified DUPLICATE.
// What it cannot reproduce is meaning: two differently worded requests that
// mean the same thing hash to unrelated vectors and score near zero. So it
// exercises the plumbing and the boundaries, and only a real encoder
// demonstrates the contribution itself.
type FakeEmbedder struct {
	dim int
}

var _ core.Embedder = (*FakeEmbedder)(nil)

// NewFakeEmbedder builds a fake embedder. A dim of zero means DefaultDim, so
// its vectors fit the same VECTOR(384) column as the real ones.
func NewFakeEmbedder(dim int) *FakeEmbedder {
	if dim <= 0 {
		dim = DefaultDim
	}
	return &FakeEmbedder{dim: dim}
}

func (f *FakeEmbedder) Model() string { return "fake-hash-" + DefaultModel }
func (f *FakeEmbedder) Dim() int      { return f.dim }

// Embed hashes the text into a unit-length vector.
//
// The hash is expanded by counter mode — sha256(text || i) for i = 0, 1, 2 …
// — because one sha256 gives 32 bytes and 384 float32s need far more. Each
// 32-bit word is mapped into [-1, 1) so the components are centred on zero,
// which keeps two unrelated texts near-orthogonal rather than crowded into one
// corner of the space. The result is unit-normalised so that Cosine reduces to
// a plain dot product, exactly as it does for a real sentence encoder.
func (f *FakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, f.dim)
	var counter [4]byte
	i := 0
	for block := uint32(0); i < f.dim; block++ {
		binary.BigEndian.PutUint32(counter[:], block)
		h := sha256.Sum256(append([]byte(text), counter[:]...))
		for off := 0; off+4 <= len(h) && i < f.dim; off += 4 {
			u := binary.BigEndian.Uint32(h[off : off+4])
			vec[i] = float32(float64(u)/float64(math.MaxUint32)*2 - 1)
			i++
		}
	}

	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		// Unreachable for any real hash output, but a zero vector would make
		// every similarity 0 and quietly disable dedup, so refuse it loudly.
		return nil, fmt.Errorf("semantic: fake embedder produced a zero vector")
	}
	for j := range vec {
		vec[j] = float32(float64(vec[j]) / norm)
	}
	return vec, nil
}
