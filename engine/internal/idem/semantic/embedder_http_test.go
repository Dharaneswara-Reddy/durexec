package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// TestHTTPEmbedderHappyPath uses httptest rather than a real model server: the
// contract being tested is the JSON shape and the error mapping, not inference.
func TestHTTPEmbedderHappyPath(t *testing.T) {
	var gotBody embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		vec := make([]float32, DefaultDim)
		vec[0] = 1
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: vec})
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, time.Second)
	if e.Model() != DefaultModel || e.Dim() != DefaultDim {
		t.Fatalf("advertised %s/%d, want %s/%d", e.Model(), e.Dim(), DefaultModel, DefaultDim)
	}

	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Text != "hello" {
		t.Fatalf("server received text %q, want %q", gotBody.Text, "hello")
	}
	if len(vec) != DefaultDim {
		t.Fatalf("got %d dimensions, want %d", len(vec), DefaultDim)
	}
}

// TestHTTPEmbedderFailuresAreAllUnavailable is the point of the type: every
// way the model can let us down has to arrive at the caller as the one error
// it knows how to degrade on.
func TestHTTPEmbedderFailuresAreAllUnavailable(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"not found", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"unparseable body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}},
		{"wrong dimension", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float32{1, 2, 3}})
		}},
		{"empty embedding", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(embedResponse{})
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			_, err := NewHTTPEmbedder(srv.URL, time.Second).Embed(context.Background(), "x")
			if !errors.Is(err, core.ErrEmbeddingUnavailable) {
				t.Fatalf("got %v, want it to wrap ErrEmbeddingUnavailable", err)
			}
		})
	}
}

func TestHTTPEmbedderUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	_, err := NewHTTPEmbedder(url, 200*time.Millisecond).Embed(context.Background(), "x")
	if !errors.Is(err, core.ErrEmbeddingUnavailable) {
		t.Fatalf("got %v, want it to wrap ErrEmbeddingUnavailable", err)
	}
}

// TestHTTPEmbedderDegradesInsideTheChecker joins the two halves: a model server
// that is down produces a journaled DISTINCT verdict rather than an error.
func TestHTTPEmbedderDegradesInsideTheChecker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewChecker(NewHTTPEmbedder(srv.URL, time.Second), &memIntents{})
	ev, err := c.Check(context.Background(), baseRequest(`{"q":"BLR to DEL"}`))
	if err != nil {
		t.Fatalf("a workflow must not fail because the model is down: %v", err)
	}
	if ev.Verdict != core.VerdictDistinct || !IsFallback(ev) {
		t.Fatalf("got %s / model %q, want a recorded DISTINCT fallback", ev.Verdict, ev.Model)
	}
}

func TestHTTPEmbedderWithModel(t *testing.T) {
	e := NewHTTPEmbedder("http://example.invalid", 0).WithModel("bge-small-en", 512)
	if e.Model() != "bge-small-en" || e.Dim() != 512 {
		t.Fatalf("WithModel did not take effect: %s/%d", e.Model(), e.Dim())
	}
}
