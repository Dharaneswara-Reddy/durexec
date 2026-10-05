package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNode is one engine node: it answers /healthz with its own id, and hands
// everything else to a per-test handler.
type fakeNode struct {
	id   string
	srv  *httptest.Server
	hits atomic.Int64
}

func newFakeNode(t *testing.T, id string, h http.HandlerFunc) *fakeNode {
	t.Helper()
	n := &fakeNode{id: id}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The real server sets this unconditionally from middleware, including
		// on a 409 for a shard nobody owns. Setting it here keeps the test
		// honest about why the client must not trust it.
		w.Header().Set("X-Owner-Node", id)
		if r.URL.Path == "/healthz" {
			writeTestJSON(w, http.StatusOK, map[string]any{"status": "ok", "node": id, "owned_shards": 5})
			return
		}
		n.hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func writeTestJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// notOwner writes the engine's real 409 body.
func notOwner(w http.ResponseWriter, shard int, owner string) {
	writeTestJSON(w, http.StatusConflict, map[string]any{
		"error":         "NotOwner",
		"message":       "shard is owned elsewhere",
		"shard_id":      shard,
		"current_owner": owner,
		"retry":         true,
	})
}

func started(w http.ResponseWriter, id string, shard int) {
	writeTestJSON(w, http.StatusCreated, map[string]any{
		"workflow_id": id, "run_id": "11111111-1111-1111-1111-111111111111",
		"shard_id": shard, "status": "RUNNING", "last_seq": 0,
	})
}

// newTestClient builds a client with a negligible retry delay, so a test that
// exercises the back-off path does not have to wait for it.
func newTestClient(nodes ...*fakeNode) *Client {
	bases := make([]string, 0, len(nodes))
	for _, n := range nodes {
		bases = append(bases, n.srv.URL)
	}
	c := NewClient(bases, nil, 5*time.Second, 6)
	c.retryWait = time.Millisecond
	return c
}

// TestClientFollows409 is the behaviour the whole throughput measurement rests
// on. The nodes reject work for shards they do not own instead of forwarding
// it, so a client that treats a 409 as a failure measures nothing but its own
// inability to route.
func TestClientFollows409(t *testing.T) {
	t.Run("re-sends to the node named in the body", func(t *testing.T) {
		var b *fakeNode
		a := newFakeNode(t, "node-a", func(w http.ResponseWriter, r *http.Request) {
			notOwner(w, 3, "node-b")
		})
		b = newFakeNode(t, "node-b", func(w http.ResponseWriter, r *http.Request) {
			started(w, "wf-1", 3)
		})

		c := newTestClient(a, b)
		if err := c.Discover(context.Background()); err != nil {
			t.Fatalf("Discover: %v", err)
		}

		// Both entry points must work, whichever the round-robin picks first.
		for i := 0; i < 4; i++ {
			got, err := c.StartWorkflow(context.Background(), "wf-1", "DeliveryOrder", nil)
			if err != nil {
				t.Fatalf("StartWorkflow: %v", err)
			}
			if got.ShardID != 3 {
				t.Fatalf("shard = %d, want 3", got.ShardID)
			}
		}
		if b.hits.Load() != 4 {
			t.Fatalf("node-b served %d requests, want 4", b.hits.Load())
		}
		if s := c.Stats(); s.NotOwnerFollows == 0 {
			t.Fatal("no 409 follow was counted, so the redirect path never ran")
		}
	})

	t.Run("waits out a shard nobody owns", func(t *testing.T) {
		var calls atomic.Int64
		a := newFakeNode(t, "node-a", func(w http.ResponseWriter, r *http.Request) {
			// An empty current_owner is what the gateway returns while a lease
			// has lapsed and no node has taken it yet.
			if calls.Add(1) <= 2 {
				notOwner(w, 3, "")
				return
			}
			started(w, "wf-2", 3)
		})

		c := newTestClient(a)
		if err := c.Discover(context.Background()); err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if _, err := c.StartWorkflow(context.Background(), "wf-2", "DeliveryOrder", nil); err != nil {
			t.Fatalf("StartWorkflow: %v", err)
		}
		if got := c.Stats().NotOwnerUnowned; got != 2 {
			t.Fatalf("unowned 409s counted = %d, want 2", got)
		}
	})

	t.Run("never trusts the X-Owner-Node header on an unowned 409", func(t *testing.T) {
		// node-a refuses forever with no owner named, while its X-Owner-Node
		// header says "node-a". A client that followed the header would come
		// straight back here and spin until the context died.
		a := newFakeNode(t, "node-a", func(w http.ResponseWriter, r *http.Request) {
			notOwner(w, 3, "")
		})
		c := newTestClient(a)
		if err := c.Discover(context.Background()); err != nil {
			t.Fatalf("Discover: %v", err)
		}
		_, err := c.StartWorkflow(context.Background(), "wf-3", "DeliveryOrder", nil)
		if err == nil {
			t.Fatal("expected the attempt budget to run out, got success")
		}
		if !strings.Contains(err.Error(), "unowned") {
			t.Fatalf("error should name the unowned shard, got: %v", err)
		}
	})

	t.Run("an owner with no known address is an actionable error", func(t *testing.T) {
		a := newFakeNode(t, "node-a", func(w http.ResponseWriter, r *http.Request) {
			notOwner(w, 3, "node-ghost")
		})
		c := newTestClient(a)
		if err := c.Discover(context.Background()); err != nil {
			t.Fatalf("Discover: %v", err)
		}
		_, err := c.StartWorkflow(context.Background(), "wf-4", "DeliveryOrder", nil)
		if err == nil {
			t.Fatal("expected an error naming the unreachable owner")
		}
		for _, want := range []string{"node-ghost", "-node"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error should mention %q so the operator knows what to do, got: %v", want, err)
			}
		}
	})

	t.Run("retries a 503 LostOwnership", func(t *testing.T) {
		var calls atomic.Int64
		a := newFakeNode(t, "node-a", func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				writeTestJSON(w, http.StatusServiceUnavailable, map[string]any{
					"error": "LostOwnership", "message": "ownership changed", "retry": true,
				})
				return
			}
			started(w, "wf-5", 3)
		})
		c := newTestClient(a)
		if err := c.Discover(context.Background()); err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if _, err := c.StartWorkflow(context.Background(), "wf-5", "DeliveryOrder", nil); err != nil {
			t.Fatalf("StartWorkflow: %v", err)
		}
		if got := c.Stats().Unavailable; got != 1 {
			t.Fatalf("503s counted = %d, want 1", got)
		}
	})

	t.Run("a 400 is final and is not retried", func(t *testing.T) {
		a := newFakeNode(t, "node-a", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{
				"error": "BadRequest", "message": "workflow_id and workflow_type are required",
			})
		})
		c := newTestClient(a)
		if err := c.Discover(context.Background()); err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if _, err := c.StartWorkflow(context.Background(), "", "", nil); err == nil {
			t.Fatal("expected an error")
		}
		if got := a.hits.Load(); got != 1 {
			t.Fatalf("node-a was called %d times; a 400 must not be retried", got)
		}
	})
}

func TestNotOwnerFrom(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"names the owner", `{"error":"NotOwner","current_owner":"node-b"}`, "node-b"},
		{"unowned shard", `{"error":"NotOwner","current_owner":""}`, ""},
		{"owner field absent", `{"error":"NotOwner"}`, ""},
		{"a different error with an owner field", `{"error":"Internal","current_owner":"node-b"}`, ""},
		{"not JSON at all", `<html>502 bad gateway</html>`, ""},
		{"empty body", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := notOwnerFrom([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	fallback := 250 * time.Millisecond
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", fallback},
		{"two seconds", "2", 2 * time.Second},
		{"zero", "0", 0},
		{"an HTTP date, which we do not parse", "Wed, 21 Oct 2026 07:28:00 GMT", fallback},
		{"negative", "-5", fallback},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Retry-After", tc.header)
			}
			if got := retryAfter(h, fallback); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestShardsTriesEveryNode covers the failover case: the first node the tool
// asks may be the one that just died, and /v1/shards is how it notices.
func TestShardsTriesEveryNode(t *testing.T) {
	dead := newFakeNode(t, "node-a", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	alive := newFakeNode(t, "node-b", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{
			"node": "node-b", "count": 1,
			"shards": []map[string]any{
				{"shard_id": 3, "owner": "node-b", "fencing_token": 7,
					"expires_at": time.Now().Add(10 * time.Second), "expired": false, "self": true},
			},
		})
	})

	c := newTestClient(dead, alive)
	if err := c.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	resp, err := c.Shards(context.Background())
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(resp.Shards) != 1 || resp.Shards[0].Token != 7 {
		t.Fatalf("unexpected shard view: %+v", resp.Shards)
	}
	if resp.ObservedAt.IsZero() {
		t.Fatal("ObservedAt must be stamped locally; recovery timing depends on it")
	}
}

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"password removed", "postgres://durexec:durexec@localhost:5433/durexec", "postgres://***@localhost:5433/durexec"},
		{"no credentials", "postgres://localhost:5433/durexec", "postgres://localhost:5433/durexec"},
		{"key value form is left alone", "host=localhost dbname=durexec", "host=localhost dbname=durexec"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactDSN(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
