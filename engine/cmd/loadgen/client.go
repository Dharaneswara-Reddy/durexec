package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// Wire shapes.
//
// These mirror the JSON that internal/api emits, but they are declared here
// rather than imported from it. The API package is being changed by another
// agent while this tool is being written, and a measurement harness that stops
// compiling because a server-side struct gained a field is a harness that
// cannot be run when it is needed. What this tool actually depends on is the
// JSON contract, so the JSON contract is what it states.
// ---------------------------------------------------------------------------

// WorkflowResponse is the body of a start or get.
type WorkflowResponse struct {
	WorkflowID   string               `json:"workflow_id"`
	RunID        core.RunID           `json:"run_id"`
	ShardID      core.ShardID         `json:"shard_id"`
	WorkflowType string               `json:"workflow_type,omitempty"`
	Status       core.ExecutionStatus `json:"status"`
	LastSeq      core.Seq             `json:"last_seq"`
	AlreadyOpen  bool                 `json:"already_open,omitempty"`
}

// ShardView is one row of GET /v1/shards.
type ShardView struct {
	ShardID   core.ShardID      `json:"shard_id"`
	Owner     string            `json:"owner,omitempty"`
	Token     core.FencingToken `json:"fencing_token"`
	ExpiresAt time.Time         `json:"expires_at"`
	Expired   bool              `json:"expired"`
	Self      bool              `json:"self"`
}

// ShardsResponse is the body of GET /v1/shards.
type ShardsResponse struct {
	Node   string      `json:"node"`
	Count  int         `json:"count"`
	Shards []ShardView `json:"shards"`
	// ObservedAt is filled in by this client, not by the server. Recovery
	// timing is measured against the moment the observation was taken locally.
	ObservedAt time.Time `json:"-"`
}

// errorBody is the engine's single error shape.
type errorBody struct {
	Error        string `json:"error"`
	Message      string `json:"message"`
	CurrentOwner string `json:"current_owner"`
	Retry        bool   `json:"retry"`
}

// startBody is POST /v1/workflows. The server rejects unknown fields, so this
// struct must not gain any the API does not declare.
type startBody struct {
	WorkflowID   string          `json:"workflow_id"`
	WorkflowType string          `json:"workflow_type"`
	Input        json.RawMessage `json:"input,omitempty"`
}

// ---------------------------------------------------------------------------
// Client.
// ---------------------------------------------------------------------------

// ClientStats counts the things a reader needs in order to trust the numbers.
//
// NotOwnerFollows in particular is not diagnostic noise. At the time this tool
// was written the nodes refuse work for a shard they do not own instead of
// forwarding it, so a client that treats a 409 as a failure measures its own
// inability to route and nothing else. Printing the follow count is how a
// reader confirms that the redirects were being followed rather than counted
// as errors.
type ClientStats struct {
	Requests        int64
	NotOwnerFollows int64
	NotOwnerUnowned int64
	Unavailable     int64
	TransportErrors int64
}

type clientCounters struct {
	requests        atomic.Int64
	notOwnerFollows atomic.Int64
	notOwnerUnowned atomic.Int64
	unavailable     atomic.Int64
	transportErrors atomic.Int64
}

// Client is an HTTP client that knows how this cluster routes.
type Client struct {
	hc        *http.Client
	bases     []string
	maxFollow int
	retryWait time.Duration

	mu     sync.RWMutex
	byNode map[string]string

	rr    atomic.Uint64
	stats clientCounters
}

// NewClient builds a client over the given entry points.
func NewClient(bases []string, seed map[string]string, timeout time.Duration, maxFollow int) *Client {
	byNode := make(map[string]string, len(seed))
	for k, v := range seed {
		byNode[k] = v
	}
	if maxFollow < 1 {
		maxFollow = 1
	}
	return &Client{
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// The load generator opens a lot of short-lived requests at a
				// small number of hosts. The stdlib default of two idle
				// connections per host would serialise them behind a handshake
				// and make this tool the bottleneck instead of the engine.
				MaxIdleConns:        512,
				MaxIdleConnsPerHost: 256,
				IdleConnTimeout:     90 * time.Second,
				DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			},
		},
		bases:     bases,
		maxFollow: maxFollow,
		retryWait: 250 * time.Millisecond,
		byNode:    byNode,
	}
}

// Discover asks each base URL who it is, so a 409 naming "node-b" can be
// followed to the URL node-b actually listens on.
//
// A node that does not answer is skipped rather than fatal. During a failover
// run one node is supposed to be dead, and refusing to start because of it
// would make the tool unusable in exactly the situation it exists for.
func (c *Client) Discover(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	found := 0
	for _, base := range c.bases {
		var h struct {
			Node string `json:"node"`
		}
		if err := c.getOnce(ctx, base, "/healthz", &h); err != nil || h.Node == "" {
			continue
		}
		c.byNode[h.Node] = base
		found++
	}
	if found == 0 && len(c.byNode) == 0 {
		return fmt.Errorf("no base URL answered GET /healthz (tried %s)", strings.Join(c.bases, ", "))
	}
	return nil
}

// Describe renders the discovered topology for the run header.
func (c *Client) Describe() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.byNode))
	for n := range c.byNode {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+c.byNode[n])
	}
	return strings.Join(parts, " ")
}

// Stats snapshots the counters.
func (c *Client) Stats() ClientStats {
	return ClientStats{
		Requests:        c.stats.requests.Load(),
		NotOwnerFollows: c.stats.notOwnerFollows.Load(),
		NotOwnerUnowned: c.stats.notOwnerUnowned.Load(),
		Unavailable:     c.stats.unavailable.Load(),
		TransportErrors: c.stats.transportErrors.Load(),
	}
}

func (c *Client) baseForNode(node string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.byNode[node]
	return b, ok
}

func (c *Client) nextBase() string {
	return c.bases[int(c.rr.Add(1)-1)%len(c.bases)]
}

// getOnce is a plain request with no routing logic, used by Discover.
func (c *Client) getOnce(ctx context.Context, base, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s%s: status %d", base, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// do sends one logical request, following the cluster's routing rules.
//
// Three responses are not final answers:
//
//	409 NotOwner       this node does not own the shard; the body names who
//	                   does, so re-send there
//	409 NotOwner, ""   nobody owns the shard right now — a failover is in
//	                   progress; wait and try another entry point
//	503 LostOwnership  ownership moved mid-request and nothing was committed
//
// Everything else is returned to the caller as it stands.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	base := c.nextBase()
	var last error

	for attempt := 0; attempt <= c.maxFollow; attempt++ {
		status, hdr, respBody, err := c.send(ctx, base, method, path, body)
		if err != nil {
			c.stats.transportErrors.Add(1)
			last = fmt.Errorf("%s %s%s: %w", method, base, path, err)
			base = c.nextBase()
			if !sleepCtx(ctx, c.retryWait) {
				return 0, ctx.Err()
			}
			continue
		}

		switch {
		case status == http.StatusConflict:
			owner := notOwnerFrom(respBody)
			if owner == "" {
				// The lease has lapsed and no node has taken it yet. There is
				// no better address to try, so wait out a slice of the lease
				// TTL and come back.
				c.stats.notOwnerUnowned.Add(1)
				last = fmt.Errorf("%s %s: 409 NotOwner, shard currently unowned", method, path)
				base = c.nextBase()
				if !sleepCtx(ctx, c.retryWait) {
					return status, ctx.Err()
				}
				continue
			}
			next, ok := c.baseForNode(owner)
			if !ok {
				// One re-discovery, in case a node joined after start-up.
				_ = c.Discover(ctx)
				if next, ok = c.baseForNode(owner); !ok {
					return status, fmt.Errorf("409 NotOwner names owner %q but no base URL is known for it; "+
						"pass -node %s=http://host:port", owner, owner)
				}
			}
			c.stats.notOwnerFollows.Add(1)
			base = next
			continue

		case status == http.StatusServiceUnavailable:
			c.stats.unavailable.Add(1)
			last = fmt.Errorf("%s %s: 503 %s", method, path, snippet(respBody))
			if !sleepCtx(ctx, retryAfter(hdr, c.retryWait)) {
				return status, ctx.Err()
			}
			continue

		case status/100 == 2:
			if out == nil {
				return status, nil
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return status, fmt.Errorf("%s %s: undecodable body: %w", method, path, err)
			}
			return status, nil

		default:
			return status, fmt.Errorf("%s %s: status %d: %s", method, path, status, snippet(respBody))
		}
	}
	if last == nil {
		last = fmt.Errorf("%s %s: gave up after %d routing attempts", method, path, c.maxFollow)
	}
	return 0, fmt.Errorf("after %d routing attempts: %w", c.maxFollow, last)
}

func (c *Client) send(ctx context.Context, base, method, path string, body []byte) (int, http.Header, []byte, error) {
	// A fresh reader per attempt: a retried request must be able to send the
	// same body again, which a consumed io.Reader cannot do.
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.stats.requests.Add(1)
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, b, nil
}

// notOwnerFrom pulls the owner out of a 409 body.
//
// Only the body is consulted, never the X-Owner-Node header. The header is set
// unconditionally by middleware to the id of the node that is answering, and
// on a 409 for an unowned shard it is never overwritten — so trusting it would
// send the retry straight back to the node that just refused, forever.
func notOwnerFrom(body []byte) string {
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	if e.Error != "NotOwner" {
		return ""
	}
	return e.CurrentOwner
}

func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	if h == nil {
		return fallback
	}
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return fallback
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// sleepCtx waits, reporting false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---------------------------------------------------------------------------
// Endpoints.
// ---------------------------------------------------------------------------

// StartWorkflow opens one workflow.
func (c *Client) StartWorkflow(ctx context.Context, id, wfType string, input json.RawMessage) (WorkflowResponse, error) {
	body, err := json.Marshal(startBody{WorkflowID: id, WorkflowType: wfType, Input: input})
	if err != nil {
		return WorkflowResponse{}, err
	}
	var out WorkflowResponse
	_, err = c.do(ctx, http.MethodPost, "/v1/workflows", body, &out)
	return out, err
}

// GetWorkflow reads one workflow's current status.
func (c *Client) GetWorkflow(ctx context.Context, id string) (WorkflowResponse, error) {
	var out WorkflowResponse
	_, err := c.do(ctx, http.MethodGet, "/v1/workflows/"+id, nil, &out)
	return out, err
}

// Shards reads the cluster ownership table.
//
// It tries each entry point in turn rather than going through do(), because
// during a failover the first node it asks may be the one that just died, and
// this call is the tool's only way to notice that anything happened at all.
func (c *Client) Shards(ctx context.Context) (ShardsResponse, error) {
	var lastErr error
	for i := 0; i < len(c.bases); i++ {
		base := c.nextBase()
		var out ShardsResponse
		c.stats.requests.Add(1)
		at := time.Now()
		if err := c.getOnce(ctx, base, "/v1/shards", &out); err != nil {
			c.stats.transportErrors.Add(1)
			lastErr = err
			continue
		}
		out.ObservedAt = at
		return out, nil
	}
	return ShardsResponse{}, fmt.Errorf("no node answered GET /v1/shards: %w", lastErr)
}
