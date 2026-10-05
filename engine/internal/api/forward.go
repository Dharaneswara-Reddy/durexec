package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/obs"
)

// This file turns the gateway from a doorman into a router.
//
// The design document says a non-owning node replies NotOwner and the gateway
// refreshes and retries. Only the first half existed: every node refused work
// for shards it did not own, which pushed cluster routing onto every client.
// With sixteen shards over three nodes, two requests in three hit the wrong
// door, and a plain curl was a coin toss.
//
// So a node that does not own a shard now asks the node that does, on the
// caller's behalf, and relays the answer back. Correctness is still enforced
// at the write: the owner re-verifies its fencing token inside the commit
// transaction, exactly as before. Forwarding is a convenience layer, and it is
// allowed to be wrong — a forward that lands on a node which has just lost the
// shard simply comes back as a NotOwner, which is the case the retry handles.
//
// The dangerous failure mode of any such layer is a routing loop: node A
// thinks B owns the shard, B thinks A does, and the request bounces until
// something times out. Two independent bounds stop that here.
//
// The first is the forwarded-by header. A request that arrives already bearing
// it is served locally or refused, never passed on again. That alone caps any
// chain at two nodes, no matter how badly the cluster disagrees with itself.
//
// The second is MaxForwards. One node will make at most two outbound attempts
// for one inbound request, so even a node whose lease table keeps naming new
// owners gives up quickly rather than walking the cluster.

// HeaderForwardedBy names the node that relayed a request. Its presence is the
// loop guard: a node that receives a request carrying it will answer — with a
// result or with a 409 — but will not forward it a second time.
const HeaderForwardedBy = "X-Durexec-Forwarded-By"

// MaxForwards is how many outbound attempts one node will make for a single
// inbound request. Two is enough to absorb one ownership change that lands
// mid-flight, and small enough that a confused cluster cannot turn one client
// request into a sweep of every node.
//
// It is a constant rather than a Deps field on purpose. It is a safety bound,
// not a tuning knob, and a deployment that could raise it would eventually
// raise it.
const MaxForwards = 2

// defaultForwardTimeout bounds one hop. It has to cover a real request on the
// owning node — opening a workflow writes to PostgreSQL — while still being
// short enough that a dead peer is reported rather than waited on. Deps.
// ForwardTimeout overrides it.
const defaultForwardTimeout = 10 * time.Second

// hopResult is what one forwarding attempt concluded.
type hopResult int

const (
	// hopServed: the caller's response has been written and we are finished.
	hopServed hopResult = iota
	// hopRetry: the peer refused because ownership moved again; try the node
	// it named instead.
	hopRetry
	// hopUnreachable: the peer could not be contacted at all.
	hopUnreachable
)

// route is what every shard-routed handler calls instead of resolving
// ownership itself. It returns the routing, and a bool that reports whether
// the handler should carry on and serve the request locally. When that bool is
// false the response has already been written — either relayed from the node
// that owns the shard, or as the 409 the client gets when nobody can be
// reached.
//
// Handlers never learn whether a request was forwarded, which is the point:
// the decision lives in one place, so there is no handler that can forget it.
func (s *Server) route(w http.ResponseWriter, r *http.Request, workflowID string) (Routing, bool) {
	rt, err := s.gw.Resolve(r.Context(), workflowID)
	setRouting(w, rt, s.deps.NodeID)
	if err == nil {
		return rt, true
	}
	if s.forward(w, r, rt) {
		return rt, false
	}
	writeError(w, err)
	return rt, false
}

// forward relays a request to the node that owns its shard. It reports whether
// it wrote a response; false means "I did not forward", and the caller falls
// back to the 409 the engine has always returned.
//
// Every reason not to forward is a quiet one. A missing PeerAddr, an unknown
// node name, an unowned shard, a request that has already been forwarded once
// — all of them end in the ordinary NotOwner refusal, which names the owner
// and is retryable. None of them is an error in its own right, because none of
// them threatens correctness: the client simply does the routing itself, as it
// had to before this existed.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, rt Routing) bool {
	if s.deps.PeerAddr == nil {
		return false
	}
	// The loop guard, checked before anything else. A request that was
	// forwarded to us and still cannot be served here has reached the end of
	// the line: two nodes disagreeing must terminate in a refusal the client
	// can act on, not in a third hop.
	if r.Header.Get(HeaderForwardedBy) != "" {
		return false
	}
	// A request whose body we did not buffer cannot be replayed to a peer, and
	// forwarding half a body is worse than not forwarding at all.
	body, buffered := bufferedBody(r.Context())
	if !buffered {
		return false
	}

	owner := rt.Owner
	for attempt := 0; attempt < MaxForwards; attempt++ {
		// An empty owner means the lease is unowned or lapsed; our own name
		// means the shard came back to us between the refusal and now. Neither
		// is somewhere to send a request.
		if owner == "" || owner == s.deps.NodeID {
			return false
		}
		base, ok := s.deps.PeerAddr(owner)
		if !ok || base == "" {
			// The integrator cannot reach that node. Say so as a 409 naming
			// the owner rather than inventing an address or hanging on one.
			return false
		}

		// Only the attempts that have another one behind them may treat a
		// refusal as a hint. On the last one the peer's 409 is relayed
		// straight through, which is both the bound and the better answer:
		// it names a fresher owner than the lease we read before we started.
		canRetry := attempt+1 < MaxForwards
		result, next := s.hop(w, r, base, body, rt.ShardID, owner, canRetry)
		switch result {
		case hopServed:
			return true
		case hopUnreachable:
			writePeerUnavailable(w, rt.ShardID, owner)
			return true
		case hopRetry:
			owner = next
		}
	}
	// Out of attempts. Fall back to the 409, which still names an owner the
	// client can try for itself.
	return false
}

// hop performs one forwarding attempt.
//
// The response is streamed straight through on every status but 409. A 409 has
// to be read before it can be relayed, because it is the one answer that might
// mean "ownership moved again, ask somebody else" rather than "here is your
// result" — and by the time a body has been streamed to the caller it is far
// too late to change our mind about retrying.
func (s *Server) hop(w http.ResponseWriter, r *http.Request, base string, body []byte, shard core.ShardID, owner string, canRetry bool) (hopResult, string) {
	// The caller's context is the parent, so a client that gives up cancels
	// the forwarded call too rather than leaving it running against the peer.
	// The timeout is layered on top of it: a peer that accepts a connection
	// and then says nothing must not hold a request open indefinitely.
	ctx, cancel := context.WithTimeout(r.Context(), s.forwardTimeout())
	defer cancel()

	target, err := peerURL(base, r.URL)
	if err != nil {
		s.logError(r, "forward: unusable peer address", err)
		return hopUnreachable, ""
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		s.logError(r, "forward: could not build request", err)
		return hopUnreachable, ""
	}
	copyRequestHeaders(req.Header, r.Header)
	// Stamping our own name replaces any inbound value, so the header always
	// names the immediate previous hop and can never be spoofed into looking
	// like a first-time request.
	req.Header.Set(HeaderForwardedBy, s.deps.NodeID)
	req.ContentLength = int64(len(body))

	resp, err := s.forwardClient().Do(req)
	if err != nil {
		// A refused connection, a DNS failure and a timeout all land here, and
		// all of them mean the same thing to the caller: the owner is not
		// answering right now.
		s.logError(r, "forward: peer did not answer", err)
		return hopUnreachable, ""
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		refusal, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if canRetry {
			if peerOwner, isNotOwner := notOwnerFromBody(refusal); isNotOwner {
				if next := s.nextOwner(r.Context(), shard, owner, peerOwner); next != "" {
					return hopRetry, next
				}
			}
		}
		relayHeaders(w, resp.Header)
		w.Header().Set("Content-Length", itoa(int64(len(refusal))))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(refusal)
		return hopServed, ""
	}

	relayHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	// A copy that fails partway has already sent a status line, so there is
	// nothing useful left to tell the client; the request id in the log is how
	// a truncated response gets diagnosed.
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.logError(r, "forward: relaying the response body failed", err)
	}
	return hopServed, ""
}

// nextOwner decides who to try after a peer refused, and returns "" for "stop
// forwarding".
//
// Our own lease store is asked first, because it is the same table the peer
// read and it is the closest thing to an authority. The peer's own answer is
// the fallback: if it named a node we have not tried, it saw something our
// read did not.
func (s *Server) nextOwner(ctx context.Context, shard core.ShardID, tried, peerOwner string) string {
	next := ""
	if s.deps.Leases != nil {
		rt, err := s.gw.ResolveShard(ctx, shard)
		if err == nil {
			// The shard has landed on us while the forward was in flight.
			// Rather than unwinding the request and serving it here, relay the
			// peer's refusal; it is retryable, and the client's retry will
			// reach this node and be served locally.
			return ""
		}
		next = rt.Owner
	}
	if next == "" || next == tried {
		next = peerOwner
	}
	if next == "" || next == tried || next == s.deps.NodeID {
		return ""
	}
	return next
}

// notOwnerFromBody reads a peer's refusal. The second return distinguishes
// "this is our NotOwner shape" from any other 409, so an unrelated conflict is
// relayed rather than treated as a routing hint.
func notOwnerFromBody(body []byte) (string, bool) {
	var er ErrorResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return "", false
	}
	if er.Error != "NotOwner" {
		return "", false
	}
	return er.CurrentOwner, true
}

// ---- HTTP plumbing --------------------------------------------------------

// hopByHopHeaders belong to a single TCP connection and must not be relayed
// onto another one. This is the list from RFC 9110, and getting it wrong is
// how a proxy ends up advertising a peer's chunked encoding on a response it
// is about to write with a content length.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyRequestHeaders carries the caller's headers to the peer. Idempotency-Key
// and the request id matter most: the owner's de-duplication and both nodes'
// log lines depend on them surviving the hop.
func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		if http.CanonicalHeaderKey(k) == "Host" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// relayHeaders puts the owner's response headers onto our own response.
//
// X-Shard-Id and X-Owner-Node come through this way rather than being
// re-derived, which is the whole reason forwarding is worth doing: the caller
// sees the node that actually did the work, not the node it happened to dial.
// Content-Length is dropped so that net/http recomputes it for whatever we
// actually write, and the request id is restored afterwards so a caller's
// correlation id is never replaced by a peer's.
func relayHeaders(w http.ResponseWriter, src http.Header) {
	dst := w.Header()
	requestID := dst.Get(obs.RequestIDHeader)
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if hopByHopHeaders[ck] || ck == "Content-Length" {
			continue
		}
		dst.Del(ck)
		for _, v := range vs {
			dst.Add(ck, v)
		}
	}
	if requestID != "" {
		dst.Set(obs.RequestIDHeader, requestID)
	}
}

// peerURL joins a peer's base address to the path and query of the request we
// are relaying.
//
// The escaped path is reused verbatim rather than re-encoded, because a
// workflow id is a caller-chosen business key and may well contain characters
// that do not survive a decode-and-re-encode round trip unchanged.
func peerURL(base string, u *url.URL) (string, error) {
	raw := strings.TrimSpace(base)
	if raw == "" {
		return "", fmt.Errorf("empty peer address")
	}
	// A bare "host:port" is the most natural thing for an integrator to put in
	// a config file, so assume http rather than rejecting it. The check runs
	// before any trimming, or "http://" would be mistaken for a bare host and
	// quietly turned into a URL pointing at a machine called "http".
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	b, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("peer address %q is not a URL: %w", base, err)
	}
	if b.Host == "" {
		return "", fmt.Errorf("peer address %q has no host", base)
	}
	out := b.Scheme + "://" + b.Host + strings.TrimSuffix(b.EscapedPath(), "/") + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, nil
}

func (s *Server) forwardTimeout() time.Duration {
	if s.deps.ForwardTimeout > 0 {
		return s.deps.ForwardTimeout
	}
	return defaultForwardTimeout
}

func (s *Server) forwardClient() *http.Client {
	if s.deps.ForwardClient != nil {
		return s.deps.ForwardClient
	}
	return s.fwdClient
}

// newForwardClient builds the default peer client. It is built once per server
// so connections to the other nodes are pooled; a client per request would
// open a fresh TCP connection for every forwarded call.
//
// It carries no Timeout of its own. The bound is applied per hop as a context
// deadline instead, so it covers reading the response body as well and so the
// caller's own cancellation still propagates.
func newForwardClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   2 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		// A peer that answers with a redirect is misconfigured, and chasing it
		// would be one more unbounded hop. Relay it and let the caller decide.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// writePeerUnavailable is the answer when the owner exists but will not
// answer. It is deliberately a 503 and not a 500: nothing was attempted, so
// nothing is in doubt, and the request is safe to repeat. It names the owner
// and leaks nothing about why the call failed — that detail is in the log,
// under the request id.
func writePeerUnavailable(w http.ResponseWriter, shard core.ShardID, owner string) {
	w.Header().Set("Retry-After", retryAfterSeconds)
	if owner != "" {
		w.Header().Set(HeaderOwnerNode, owner)
	}
	writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
		Error:        "PeerUnavailable",
		Message:      "the node that owns this shard did not answer; retry",
		ShardID:      &shard,
		CurrentOwner: owner,
		Retry:        true,
	})
}

// ---- request body buffering -----------------------------------------------

// bodyKey keys the buffered request body in the request context.
type bodyKey struct{}

// withBufferedBody reads the request body into memory and hands the handlers a
// replayable copy.
//
// This exists because of an ordering problem. A handler must decode the body
// to learn the workflow id, and it must know the workflow id to learn the
// shard, and it must know the shard to learn whether this node owns it. By the
// time forwarding turns out to be necessary, the body has already been
// consumed and an http.Request body is read-once. Buffering up front is the
// simplest fix, and the cap is the same one the handlers already enforce, so
// nothing becomes unbounded that was not already.
func (s *Server) withBufferedBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, []byte(nil))))
			return
		}
		buf, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeBadRequest(w, "unreadable request body: "+err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(buf))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, buf)))
	})
}

// bufferedBody returns the replayable copy of the request body. The bool is
// false when the middleware did not run, which is the signal not to forward.
func bufferedBody(ctx context.Context) ([]byte, bool) {
	b, ok := ctx.Value(bodyKey{}).([]byte)
	return b, ok
}
