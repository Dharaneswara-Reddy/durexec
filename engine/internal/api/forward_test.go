package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Nothing in this file talks to a real cluster. A "peer" is an httptest server
// that records what it was asked and answers whatever the test wants, which is
// enough to pin down every branch of the forwarding logic — including the two
// that matter most, the hop bound and the loop guard.

// ---- test doubles ---------------------------------------------------------

type recordedRequest struct {
	Method   string
	Path     string
	RawQuery string
	Body     string
	Header   http.Header
}

// peer is an httptest server that counts and records the requests it receives.
type peer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

func newPeer(t *testing.T, h http.HandlerFunc) *peer {
	t.Helper()
	p := &peer{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.reqs = append(p.reqs, recordedRequest{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Body:     string(body),
			Header:   r.Header.Clone(),
		})
		p.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *peer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *peer) last(t *testing.T) recordedRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) == 0 {
		t.Fatal("the peer was never called")
	}
	return p.reqs[len(p.reqs)-1]
}

// refuses answers exactly as a real non-owning node does, by going through the
// same writeError the engine uses, so the test cannot pass against a response
// shape the engine does not actually produce.
func refuses(shard core.ShardID, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(HeaderShardID, itoa(int64(shard)))
		w.Header().Set(HeaderOwnerNode, owner)
		writeError(w, &core.NotOwnerError{ShardID: shard, CurrentOwner: owner})
	}
}

// peerTable is the PeerAddr an integrator would supply, built from a map.
func peerTable(m map[string]string) func(string) (string, bool) {
	return func(node string) (string, bool) {
		addr, ok := m[node]
		return addr, ok
	}
}

// scriptedLeases hands out a different owner on each Get, which is how a
// mid-flight ownership change is reproduced without a database. The final
// entry repeats for every call after it.
type scriptedLeases struct {
	mu     sync.Mutex
	owners []string
	calls  int
}

func (f *scriptedLeases) Get(_ context.Context, shard core.ShardID) (core.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i >= len(f.owners) {
		i = len(f.owners) - 1
	}
	return core.Lease{
		ShardID:   shard,
		Owner:     f.owners[i],
		Token:     core.FencingToken(i + 1),
		ExpiresAt: routingNow.Add(time.Minute),
	}, nil
}

func (f *scriptedLeases) Acquire(context.Context, core.ShardID, string, int) (core.FencingToken, error) {
	return 0, nil
}
func (f *scriptedLeases) Renew(context.Context, core.ShardID, string, core.FencingToken, int) error {
	return nil
}
func (f *scriptedLeases) Release(context.Context, core.ShardID, string, core.FencingToken) error {
	return nil
}
func (f *scriptedLeases) List(context.Context) ([]core.Lease, error) { return nil, nil }

// ownedBy points a node at a live lease held by owner.
func ownedBy(owner string) *fakeLeases {
	return &fakeLeases{lease: core.Lease{Owner: owner, Token: 9, ExpiresAt: routingNow.Add(time.Minute)}}
}

// send is do() with control over request headers, which the loop guard needs.
func send(t *testing.T, d Deps, method, target, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	New(d).ServeHTTP(w, r)
	return w
}

const startBody = `{"workflow_id":"trip-42","workflow_type":"BookingWorkflow"}`

// ---- the local case -------------------------------------------------------

// TestOwnedRequestIsNeverForwarded is the first rule. A node that owns the
// shard must serve it, and must not so much as ask where the owner lives.
func TestOwnedRequestIsNeverForwarded(t *testing.T) {
	p := newPeer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the owning node forwarded a request it should have served itself")
		w.WriteHeader(http.StatusTeapot)
	})

	var lookups []string
	d := ownedDeps() // this node owns shard 3
	d.PeerAddr = func(node string) (string, bool) {
		lookups = append(lookups, node)
		return p.URL, true
	}

	w := do(t, d, http.MethodPost, "/v1/workflows", startBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201\nbody: %s", w.Code, w.Body)
	}
	if p.count() != 0 {
		t.Fatalf("the peer saw %d requests, want 0", p.count())
	}
	if len(lookups) != 0 {
		t.Fatalf("PeerAddr was consulted for %v; an owned request must not look up a peer at all", lookups)
	}
	if got := w.Header().Get(HeaderOwnerNode); got != selfNode {
		t.Fatalf("X-Owner-Node = %q, want %q", got, selfNode)
	}
}

// ---- the forwarding case --------------------------------------------------

// TestForwardsToOwnerAndRelaysResponse is the defect this work exists to fix:
// a request for somebody else's shard used to come back 409, and now comes
// back with the owner's answer.
func TestForwardsToOwnerAndRelaysResponse(t *testing.T) {
	p := newPeer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(HeaderShardID, "2")
		w.Header().Set(HeaderOwnerNode, "node-b")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"workflow_id":"trip-42","run_id":"run-7"}`))
	})

	d := ownedDeps()
	d.Leases = ownedBy("node-b")
	d.PeerAddr = peerTable(map[string]string{"node-b": p.URL})

	h := http.Header{}
	h.Set(HeaderIdempotencyKey, "key-123")
	w := send(t, d, http.MethodPost, "/v1/workflows", startBody, h)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want the owner's 201\nbody: %s", w.Code, w.Body)
	}
	if got := w.Body.String(); got != `{"workflow_id":"trip-42","run_id":"run-7"}` {
		t.Fatalf("body = %q, want the owner's body verbatim", got)
	}
	// These two headers are the whole point of relaying rather than
	// re-deriving: the caller sees the node that did the work.
	if got := w.Header().Get(HeaderOwnerNode); got != "node-b" {
		t.Fatalf("X-Owner-Node = %q, want the real owner %q", got, "node-b")
	}
	if got := w.Header().Get(HeaderShardID); got != "2" {
		t.Fatalf("X-Shard-Id = %q, want the owner's %q", got, "2")
	}

	got := p.last(t)
	if got.Method != http.MethodPost || got.Path != "/v1/workflows" {
		t.Fatalf("peer saw %s %s, want POST /v1/workflows", got.Method, got.Path)
	}
	if got.Body != startBody {
		t.Fatalf("peer saw body %q, want the original %q", got.Body, startBody)
	}
	if got.Header.Get(HeaderForwardedBy) != selfNode {
		t.Fatalf("%s = %q, want %q", HeaderForwardedBy, got.Header.Get(HeaderForwardedBy), selfNode)
	}
	// The owner does its own de-duplication, so the caller's key has to reach it.
	if got.Header.Get(HeaderIdempotencyKey) != "key-123" {
		t.Fatalf("Idempotency-Key = %q, want it carried across the hop", got.Header.Get(HeaderIdempotencyKey))
	}
}

// TestForwardsEveryShardRoutedRoute checks the decision lives in one place
// rather than in each handler, by exercising all of them.
func TestForwardsEveryShardRoutedRoute(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/workflows", startBody},
		{http.MethodGet, "/v1/workflows/trip-42", ""},
		{http.MethodGet, "/v1/workflows/trip-42/history", ""},
		{http.MethodPost, "/v1/workflows/trip-42/signals", `{"name":"x"}`},
		{http.MethodPost, "/v1/workflows/trip-42/cancel", ""},
		{http.MethodPost, "/v1/admin/fail-step", `{"workflow_id":"trip-42"}`},
		{http.MethodGet, "/v1/dedup/verdicts?workflow_id=trip-42&limit=5", ""},
	}
	for _, tc := range routes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			p := newPeer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(HeaderOwnerNode, "node-b")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"served_by":"node-b"}`))
			})
			d := ownedDeps()
			d.Leases = ownedBy("node-b")
			d.PeerAddr = peerTable(map[string]string{"node-b": p.URL})

			w := do(t, d, tc.method, tc.path, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want the owner's 200\nbody: %s", w.Code, w.Body)
			}
			if p.count() != 1 {
				t.Fatalf("peer saw %d requests, want exactly 1", p.count())
			}
			got := p.last(t)
			if got.Method != tc.method {
				t.Fatalf("peer saw method %s, want %s", got.Method, tc.method)
			}
			// The query string carries the caller's filters; losing it would
			// silently change the answer rather than fail.
			wantQuery := ""
			if i := strings.IndexByte(tc.path, '?'); i >= 0 {
				wantQuery = tc.path[i+1:]
			}
			if got.RawQuery != wantQuery {
				t.Fatalf("peer saw query %q, want %q", got.RawQuery, wantQuery)
			}
		})
	}
}

// ---- the loop guard -------------------------------------------------------

// TestAlreadyForwardedIsNeverForwardedAgain is the loop guard. A request that
// has been relayed once and still cannot be served must stop here.
func TestAlreadyForwardedIsNeverForwardedAgain(t *testing.T) {
	p := newPeer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a forwarded request was forwarded a second time")
		w.WriteHeader(http.StatusTeapot)
	})

	d := ownedDeps()
	d.Leases = ownedBy("node-c")
	d.PeerAddr = peerTable(map[string]string{"node-c": p.URL})

	h := http.Header{}
	h.Set(HeaderForwardedBy, "node-b")
	w := send(t, d, http.MethodPost, "/v1/workflows", startBody, h)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\nbody: %s", w.Code, w.Body)
	}
	if p.count() != 0 {
		t.Fatalf("peer saw %d requests, want 0", p.count())
	}
	if got := decode[ErrorResponse](t, w); got.Error != "NotOwner" || got.CurrentOwner != "node-c" {
		t.Fatalf("body = %s, want a NotOwner naming node-c", w.Body)
	}
}

// TestTwoNodesDisagreeingTerminates wires two real servers that each believe
// the other owns the shard — the exact configuration that would ping-pong
// forever without the guard — and proves the exchange ends after one hop.
func TestTwoNodesDisagreeingTerminates(t *testing.T) {
	var bURL, aURL string

	nodeB := ownedDeps()
	nodeB.NodeID = "node-b"
	nodeB.Leases = ownedBy("node-a")
	nodeB.PeerAddr = func(node string) (string, bool) {
		if node == "node-a" {
			return aURL, true
		}
		return "", false
	}
	bSrv := httptest.NewServer(New(nodeB))
	defer bSrv.Close()
	bURL = bSrv.URL

	nodeA := ownedDeps()
	nodeA.NodeID = "node-a"
	nodeA.Leases = ownedBy("node-b")
	nodeA.PeerAddr = func(node string) (string, bool) {
		if node == "node-b" {
			return bURL, true
		}
		return "", false
	}
	aSrv := httptest.NewServer(New(nodeA))
	defer aSrv.Close()
	aURL = aSrv.URL

	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(aSrv.URL+"/v1/workflows", "application/json", strings.NewReader(startBody))
		if err != nil {
			done <- nil
			return
		}
		done <- resp
	}()

	select {
	case resp := <-done:
		if resp == nil {
			t.Fatal("the request failed outright; it should have ended in a clean refusal")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409 — two disagreeing nodes must end in a refusal", resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request never came back: the two nodes are forwarding to each other")
	}
}

// ---- unreachable owners ---------------------------------------------------

// TestPeerAddrUnknownIs409 covers the integrator's "I cannot reach that node".
// It must degrade to the old behaviour, not to a panic or a guess.
func TestPeerAddrUnknownIs409(t *testing.T) {
	d := ownedDeps()
	d.Leases = ownedBy("node-c")
	d.PeerAddr = func(string) (string, bool) { return "", false }

	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\nbody: %s", w.Code, w.Body)
	}
	got := decode[ErrorResponse](t, w)
	if got.Error != "NotOwner" || got.CurrentOwner != "node-c" {
		t.Fatalf("body = %s, want a NotOwner naming node-c so the client can route itself", w.Body)
	}
	if !got.Retry {
		t.Fatal("retry = false; a routing refusal is retryable")
	}
	if h := w.Header().Get(HeaderOwnerNode); h != "node-c" {
		t.Fatalf("X-Owner-Node = %q, want node-c", h)
	}
}

// TestUnreachablePeerIs503 kills the peer before the request, so the dial
// fails. The answer must be a clean 503 that says nothing about sockets.
func TestUnreachablePeerIs503(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	addr := dead.URL
	dead.Close() // nothing is listening on that port any more

	d := ownedDeps()
	d.Leases = ownedBy("node-c")
	d.PeerAddr = peerTable(map[string]string{"node-c": addr})
	d.ForwardTimeout = 500 * time.Millisecond

	start := time.Now()
	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")
	elapsed := time.Since(start)

	if w.Code != http.StatusServiceUnavailable && w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 503 or 409\nbody: %s", w.Code, w.Body)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the request took %s; an unreachable peer must not hang", elapsed)
	}
	body := w.Body.String()
	for _, leak := range []string{"connection refused", "dial tcp", "127.0.0.1", "Transport"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the response leaks transport detail (%q): %s", leak, body)
		}
	}
	got := decode[ErrorResponse](t, w)
	if got.CurrentOwner != "node-c" {
		t.Fatalf("current_owner = %q, want node-c", got.CurrentOwner)
	}
	if !got.Retry {
		t.Fatal("retry = false; an unreachable peer is exactly the retryable case")
	}
}

// TestPeerTimeoutIs503 covers the worse failure: the peer accepts the
// connection and then says nothing. Without a deadline this is the hang.
func TestPeerTimeoutIs503(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := newPeer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	})

	d := ownedDeps()
	d.Leases = ownedBy("node-c")
	d.PeerAddr = peerTable(map[string]string{"node-c": p.URL})
	d.ForwardTimeout = 80 * time.Millisecond

	start := time.Now()
	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")
	elapsed := time.Since(start)

	if w.Code != http.StatusServiceUnavailable && w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 503 or 409\nbody: %s", w.Code, w.Body)
	}
	if w.Code == http.StatusInternalServerError {
		t.Fatal("a slow peer must never surface as a 500")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the request took %s; ForwardTimeout was 80ms", elapsed)
	}
	if strings.Contains(w.Body.String(), "context deadline exceeded") {
		t.Fatalf("the response leaks the internal error: %s", w.Body)
	}
}

// ---- the hop bound --------------------------------------------------------

// TestAtMostTwoForwards is the bound, counted rather than asserted. Every node
// in the chain refuses and names a fresh owner, so an unbounded gateway would
// walk the whole cluster. This one stops at two.
func TestAtMostTwoForwards(t *testing.T) {
	b := newPeer(t, refuses(3, "node-c"))
	c := newPeer(t, refuses(3, "node-d"))
	dNode := newPeer(t, refuses(3, "node-e"))
	e := newPeer(t, refuses(3, "node-f"))

	deps := ownedDeps()
	// Every lease read names a node we have not tried, which is the worst case
	// for a retry loop: there is always somewhere else to go.
	deps.Leases = &scriptedLeases{owners: []string{"node-b", "node-c", "node-d", "node-e", "node-f"}}
	deps.PeerAddr = peerTable(map[string]string{
		"node-b": b.URL, "node-c": c.URL, "node-d": dNode.URL, "node-e": e.URL,
	})

	w := do(t, deps, http.MethodGet, "/v1/workflows/trip-42", "")

	hops := b.count() + c.count() + dNode.count() + e.count()
	if hops != MaxForwards {
		t.Fatalf("made %d outbound forwards, want exactly %d (b=%d c=%d d=%d e=%d)",
			hops, MaxForwards, b.count(), c.count(), dNode.count(), e.count())
	}
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 once the hop budget is spent\nbody: %s", w.Code, w.Body)
	}
	if got := decode[ErrorResponse](t, w); got.Error != "NotOwner" {
		t.Fatalf("body = %s, want a NotOwner the client can act on", w.Body)
	}
}

// TestRetriesOnceWhenOwnershipMovesMidFlight is the case the budget exists
// for: the first owner has already lost the shard by the time we reach it.
func TestRetriesOnceWhenOwnershipMovesMidFlight(t *testing.T) {
	b := newPeer(t, refuses(3, "node-c"))
	c := newPeer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(HeaderOwnerNode, "node-c")
		w.Header().Set(HeaderShardID, "3")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"workflow_id":"trip-42","status":"RUNNING"}`))
	})

	d := ownedDeps()
	d.Leases = &scriptedLeases{owners: []string{"node-b", "node-c"}}
	d.PeerAddr = peerTable(map[string]string{"node-b": b.URL, "node-c": c.URL})

	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the second owner's 200\nbody: %s", w.Code, w.Body)
	}
	if b.count() != 1 || c.count() != 1 {
		t.Fatalf("hops: node-b=%d node-c=%d, want one each", b.count(), c.count())
	}
	if got := w.Header().Get(HeaderOwnerNode); got != "node-c" {
		t.Fatalf("X-Owner-Node = %q, want node-c — the node that actually served it", got)
	}
	// Each hop must be stamped, or the second node would be free to forward on.
	if got := b.last(t).Header.Get(HeaderForwardedBy); got != selfNode {
		t.Fatalf("first hop %s = %q", HeaderForwardedBy, got)
	}
	if got := c.last(t).Header.Get(HeaderForwardedBy); got != selfNode {
		t.Fatalf("second hop %s = %q", HeaderForwardedBy, got)
	}
}

// ---- node-local endpoints -------------------------------------------------

// TestNodeLocalEndpointsAreNeverForwarded guards the endpoints whose whole
// purpose is to describe THIS process. Forwarding /healthz would make a dying
// node report somebody else's health, which is worse than no health check.
func TestNodeLocalEndpointsAreNeverForwarded(t *testing.T) {
	for _, path := range []string{"/healthz", "/metrics", "/v1/shards", "/v1/workflows"} {
		t.Run(path, func(t *testing.T) {
			p := newPeer(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Errorf("%s was forwarded; it is node-local by definition", path)
				w.WriteHeader(http.StatusTeapot)
			})
			d := ownedDeps()
			// This node owns nothing at all, which is the state in which a
			// careless implementation would forward everything.
			d.Leases = ownedBy("node-c")
			d.Gatherer = prometheus.NewRegistry()
			d.PeerAddr = peerTable(map[string]string{"node-c": p.URL})

			w := do(t, d, http.MethodGet, path, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 served locally\nbody: %s", w.Code, w.Body)
			}
			if p.count() != 0 {
				t.Fatalf("peer saw %d requests, want 0", p.count())
			}
		})
	}
}

// TestForwardingIsOffWithoutPeerAddr proves the feature is additive: a node
// wired the old way behaves exactly the old way.
func TestForwardingIsOffWithoutPeerAddr(t *testing.T) {
	d := ownedDeps()
	d.Leases = ownedBy("node-c")
	d.PeerAddr = nil

	w := do(t, d, http.MethodPost, "/v1/workflows", startBody)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\nbody: %s", w.Code, w.Body)
	}
	if got := decode[ErrorResponse](t, w); got.CurrentOwner != "node-c" {
		t.Fatalf("body = %s", w.Body)
	}
}

// TestUnownedShardIsNotForwarded: when the lease has lapsed there is no owner
// to forward to, and inventing one would send the client to a node that is
// about to refuse it again.
func TestUnownedShardIsNotForwarded(t *testing.T) {
	p := newPeer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("forwarded a request for a shard nobody owns")
		w.WriteHeader(http.StatusTeapot)
	})
	d := ownedDeps()
	d.Leases = &fakeLeases{lease: core.Lease{Owner: "node-c", ExpiresAt: routingNow.Add(-time.Minute)}}
	d.PeerAddr = peerTable(map[string]string{"node-c": p.URL})

	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\nbody: %s", w.Code, w.Body)
	}
	if p.count() != 0 {
		t.Fatalf("peer saw %d requests, want 0", p.count())
	}
}

// ---- the pure parts -------------------------------------------------------

func TestPeerURL(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		target string
		want   string
	}{
		{"plain path", "http://node-b:8080", "/v1/workflows", "http://node-b:8080/v1/workflows"},
		{"trailing slash on the base", "http://node-b:8080/", "/v1/workflows", "http://node-b:8080/v1/workflows"},
		{"bare host and port", "node-b:8080", "/healthz", "http://node-b:8080/healthz"},
		{"query is carried", "http://node-b:8080", "/v1/dedup/verdicts?workflow_id=a&limit=5", "http://node-b:8080/v1/dedup/verdicts?workflow_id=a&limit=5"},
		{"base path prefix", "http://gw/engine", "/v1/workflows", "http://gw/engine/v1/workflows"},
		{"https is kept", "https://node-b", "/v1/workflows", "https://node-b/v1/workflows"},
		{
			// A workflow id is a caller-chosen business key, so the escaping
			// it arrived with is what must be relayed.
			name: "escaped path is preserved", base: "http://node-b",
			target: "/v1/workflows/order%2F42/history",
			want:   "http://node-b/v1/workflows/order%2F42/history",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			got, err := peerURL(tc.base, r.URL)
			if err != nil {
				t.Fatalf("peerURL: %v", err)
			}
			if got != tc.want {
				t.Fatalf("peerURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPeerURLRejectsNonsense(t *testing.T) {
	for _, base := range []string{"", "   ", "http://", "://x"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/workflows", nil)
		if got, err := peerURL(base, r.URL); err == nil {
			t.Fatalf("peerURL(%q) = %q, want an error rather than a URL we would then dial", base, got)
		}
	}
}

func TestNotOwnerFromBody(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantOwner string
		wantIs    bool
	}{
		{"a real refusal", `{"error":"NotOwner","current_owner":"node-c"}`, "node-c", true},
		{"a refusal with no owner", `{"error":"NotOwner"}`, "", true},
		{"some other conflict", `{"error":"SomethingElse","current_owner":"node-c"}`, "", false},
		{"not JSON at all", `<html>409</html>`, "", false},
		{"empty", ``, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner, is := notOwnerFromBody([]byte(tc.body))
			if owner != tc.wantOwner || is != tc.wantIs {
				t.Fatalf("notOwnerFromBody = (%q, %v), want (%q, %v)", owner, is, tc.wantOwner, tc.wantIs)
			}
		})
	}
}

// TestForeignConflictIsRelayedNotRetried: a 409 that is not our NotOwner shape
// is somebody else's answer, and retrying it would turn one conflict into two.
func TestForeignConflictIsRelayedNotRetried(t *testing.T) {
	p := newPeer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"SomeOtherConflict"}`))
	})
	d := ownedDeps()
	d.Leases = &scriptedLeases{owners: []string{"node-b", "node-c"}}
	d.PeerAddr = peerTable(map[string]string{"node-b": p.URL, "node-c": p.URL})

	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want the peer's 409", w.Code)
	}
	if p.count() != 1 {
		t.Fatalf("peer saw %d requests, want 1: an unrelated conflict is not a routing hint", p.count())
	}
	if got := w.Body.String(); !strings.Contains(got, "SomeOtherConflict") {
		t.Fatalf("body = %q, want the peer's own conflict relayed", got)
	}
}

// TestForwardClientIsConfigurable proves Deps.ForwardClient is actually used,
// which is what lets an integrator terminate TLS between nodes.
func TestForwardClientIsConfigurable(t *testing.T) {
	used := false
	d := ownedDeps()
	d.Leases = ownedBy("node-c")
	d.PeerAddr = peerTable(map[string]string{"node-c": "http://unused.invalid"})
	d.ForwardClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{HeaderOwnerNode: []string{"node-c"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    r,
		}, nil
	})}

	w := do(t, d, http.MethodGet, "/v1/workflows/trip-42", "")
	if !used {
		t.Fatal("Deps.ForwardClient was ignored")
	}
	if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
		t.Fatalf("status = %d body = %q", w.Code, w.Body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
