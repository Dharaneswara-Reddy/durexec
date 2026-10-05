package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// NodeInfo is one engine node as this tool found it.
type NodeInfo struct {
	BaseURL     string
	NodeID      string
	OwnedShards int
	Healthy     bool
	Err         string
}

// Cluster is the HTTP half of the tool: a handful of base URLs, the node id
// behind each one, and the routing retry that makes a request land on the node
// that actually owns the shard.
type Cluster struct {
	Nodes   []*NodeInfo
	byID    map[string]*NodeInfo
	hc      *http.Client
	maxHops int
	cursor  uint64
}

// MaxRoutingHops bounds the 409 chase.
//
// Four is more than the routing can legitimately need: a correct answer is one
// hop, and a second hop only happens if ownership moved between the refusal
// and the retry. Anything beyond that is two nodes disagreeing about who owns
// a shard, and looping on it would hide exactly the bug worth reporting.
const MaxRoutingHops = 4

func NewCluster(baseURLs []string, hc *http.Client) *Cluster {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	c := &Cluster{hc: hc, maxHops: MaxRoutingHops, byID: map[string]*NodeInfo{}}
	for _, raw := range baseURLs {
		u := strings.TrimRight(strings.TrimSpace(raw), "/")
		if u == "" {
			continue
		}
		c.Nodes = append(c.Nodes, &NodeInfo{BaseURL: u})
	}
	return c
}

// health is the shape of GET /healthz.
type health struct {
	Status string `json:"status"`
	Node   string `json:"node"`
	Shards int    `json:"owned_shards"`
}

// Discover asks every base URL who it is. The node id is what a 409 names as
// the current owner, so without this map the routing retry has a name and
// nowhere to send it.
func (c *Cluster) Discover(ctx context.Context) {
	for _, n := range c.Nodes {
		var h health
		resp, err := c.get(ctx, n.BaseURL, "/healthz")
		if err != nil {
			n.Err = err.Error()
			continue
		}
		if resp.Status != http.StatusOK {
			n.Err = fmt.Sprintf("/healthz answered %d", resp.Status)
			continue
		}
		if err := json.Unmarshal(resp.Body, &h); err != nil {
			n.Err = "unreadable /healthz body: " + err.Error()
			continue
		}
		n.Healthy = h.Status == "ok"
		n.NodeID = h.Node
		n.OwnedShards = h.Shards
		if n.NodeID != "" {
			c.byID[n.NodeID] = n
		}
	}
}

// HealthyNodes returns the nodes that answered /healthz.
func (c *Cluster) HealthyNodes() []*NodeInfo {
	var out []*NodeInfo
	for _, n := range c.Nodes {
		if n.Healthy {
			out = append(out, n)
		}
	}
	return out
}

// Resolve maps a node id to its base URL, which is what the 409 chase needs.
func (c *Cluster) Resolve(nodeID string) (string, bool) {
	n, ok := c.byID[nodeID]
	if !ok || n.BaseURL == "" {
		return "", false
	}
	return n.BaseURL, true
}

// pick chooses a starting node, round-robin over the healthy ones so that a
// sequence of requests does not all land on the same node and quietly test
// only one third of the cluster.
func (c *Cluster) pick() (string, bool) {
	healthy := c.HealthyNodes()
	if len(healthy) == 0 {
		return "", false
	}
	i := atomic.AddUint64(&c.cursor, 1)
	return healthy[int(i-1)%len(healthy)].BaseURL, true
}

// Response is one HTTP answer, fully read.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
	// BaseURL is the node that produced this response, and Hops is every node
	// the request visited on its way there. Both go into the report: "which
	// node answered" is half of what a routing claim is claiming.
	BaseURL string
	Hops    []string
}

// apiError is the engine's error body. Only the fields the routing retry needs
// are decoded; the rest is reported as-is if it matters.
type apiError struct {
	Error        string `json:"error"`
	Message      string `json:"message"`
	CurrentOwner string `json:"current_owner"`
	ShardID      *int32 `json:"shard_id"`
	Retry        bool   `json:"retry"`
}

// Do sends a request, following a 409 NotOwner to the node it names.
//
// At the time of writing a node that does not own a shard rejects the request
// rather than proxying it, so following the refusal is the client's job. That
// is a deliberate design choice in the engine — a proxying node would have to
// hold a request open across a failover — and it means any tool that talks to
// this cluster has to implement this retry or it will report routing failures
// that are really just the caller knocking on the wrong door.
func (c *Cluster) Do(ctx context.Context, method, path string, body any) (*Response, error) {
	start, ok := c.pick()
	if !ok {
		return nil, fmt.Errorf("no healthy node among %d configured", len(c.Nodes))
	}
	return c.DoFrom(ctx, start, method, path, body)
}

// DoFrom is Do with an explicit starting node.
func (c *Cluster) DoFrom(ctx context.Context, start, method, path string, body any) (*Response, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
	}
	send := func(ctx context.Context, baseURL string) (*Response, error) {
		return c.send(ctx, baseURL, method, path, payload)
	}
	return FollowNotOwner(ctx, start, c.Resolve, c.maxHops, send)
}

// FollowNotOwner is the routing retry, separated from the HTTP client so it
// can be tested against a fake sender as well as against httptest.
//
// resolve turns the node id named in a 409 into a base URL. send performs one
// attempt. A node is never visited twice: if the cluster hands back a name we
// have already asked, the disagreement is real and the 409 is returned so the
// report can say so, rather than being smoothed over by another lap.
func FollowNotOwner(
	ctx context.Context,
	start string,
	resolve func(nodeID string) (string, bool),
	maxHops int,
	send func(ctx context.Context, baseURL string) (*Response, error),
) (*Response, error) {
	if maxHops < 1 {
		maxHops = 1
	}
	visited := make([]string, 0, maxHops)
	at := start

	for hop := 0; hop < maxHops; hop++ {
		visited = append(visited, at)
		resp, err := send(ctx, at)
		if err != nil {
			return nil, err
		}
		resp.Hops = append([]string(nil), visited...)
		if resp.Status != http.StatusConflict {
			return resp, nil
		}

		owner := ownerFromRefusal(resp)
		if owner == "" {
			return resp, nil
		}
		next, ok := resolve(owner)
		if !ok {
			// The cluster named an owner this tool was never told about.
			// That is a configuration finding, not a routing failure, and
			// the caller should see the 409 with the name in it.
			return resp, nil
		}
		if contains(visited, next) {
			return resp, nil
		}
		at = next
	}
	// Ran out of hops: return the last refusal rather than an error, so the
	// check that asked for it can report "ownership never settled".
	resp, err := send(ctx, at)
	if err != nil {
		return nil, err
	}
	resp.Hops = append(append([]string(nil), visited...), at)
	return resp, nil
}

// ownerFromRefusal reads the owner out of a 409, preferring the body's
// current_owner and falling back to the X-Owner-Node header, which the API
// sets on the refusal path for exactly this purpose.
func ownerFromRefusal(resp *Response) string {
	var ae apiError
	if err := json.Unmarshal(resp.Body, &ae); err == nil && ae.Error == "NotOwner" && ae.CurrentOwner != "" {
		return ae.CurrentOwner
	}
	if resp.Header != nil {
		return resp.Header.Get("X-Owner-Node")
	}
	return ""
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (c *Cluster) send(ctx context.Context, baseURL, method, path string, payload []byte) (*Response, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// The body is read in full because the routing retry has to parse it and
	// the report has to quote it; a streamed body would be gone by then.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return &Response{Status: resp.StatusCode, Body: b, Header: resp.Header, BaseURL: baseURL}, nil
}

func (c *Cluster) get(ctx context.Context, baseURL, path string) (*Response, error) {
	return c.send(ctx, baseURL, http.MethodGet, path, nil)
}

// ---- typed calls the checks use -------------------------------------------

// ShardView mirrors the /v1/shards response.
type ShardView struct {
	ShardID   int32     `json:"shard_id"`
	Owner     string    `json:"owner"`
	Token     int64     `json:"fencing_token"`
	ExpiresAt time.Time `json:"expires_at"`
	Expired   bool      `json:"expired"`
	Self      bool      `json:"self"`
}

type shardsResponse struct {
	Node   string      `json:"node"`
	Count  int         `json:"count"`
	Shards []ShardView `json:"shards"`
}

// Shards reads one node's view of shard ownership.
func (c *Cluster) Shards(ctx context.Context, baseURL string) ([]ShardView, string, error) {
	resp, err := c.get(ctx, baseURL, "/v1/shards")
	if err != nil {
		return nil, "", err
	}
	if resp.Status != http.StatusOK {
		return nil, "", fmt.Errorf("GET /v1/shards on %s answered %d", baseURL, resp.Status)
	}
	var sr shardsResponse
	if err := json.Unmarshal(resp.Body, &sr); err != nil {
		return nil, "", fmt.Errorf("unreadable /v1/shards body from %s: %w", baseURL, err)
	}
	return sr.Shards, sr.Node, nil
}

// Metrics fetches one node's Prometheus exposition text.
func (c *Cluster) Metrics(ctx context.Context, baseURL string) (string, error) {
	resp, err := c.get(ctx, baseURL, "/metrics")
	if err != nil {
		return "", err
	}
	if resp.Status != http.StatusOK {
		return "", fmt.Errorf("GET /metrics on %s answered %d", baseURL, resp.Status)
	}
	return string(resp.Body), nil
}

// WorkflowView mirrors the workflow representation the API returns.
type WorkflowView struct {
	WorkflowID   string `json:"workflow_id"`
	RunID        string `json:"run_id"`
	ShardID      int32  `json:"shard_id"`
	WorkflowType string `json:"workflow_type"`
	Status       string `json:"status"`
	LastSeq      int64  `json:"last_seq"`
	AlreadyOpen  bool   `json:"already_open"`
}

type historyResponse struct {
	WorkflowID string     `json:"workflow_id"`
	RunID      string     `json:"run_id"`
	ShardID    int32      `json:"shard_id"`
	Count      int        `json:"count"`
	Events     []EventRow `json:"events"`
}

type startBody struct {
	WorkflowID   string          `json:"workflow_id"`
	WorkflowType string          `json:"workflow_type"`
	Input        json.RawMessage `json:"input,omitempty"`
}

// StartWorkflow opens a workflow through the API, following a 409 to the owner.
func (c *Cluster) StartWorkflow(ctx context.Context, id, wfType string, input any) (WorkflowView, *Response, error) {
	var raw json.RawMessage
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return WorkflowView{}, nil, err
		}
		raw = b
	}
	resp, err := c.Do(ctx, http.MethodPost, "/v1/workflows",
		startBody{WorkflowID: id, WorkflowType: wfType, Input: raw})
	if err != nil {
		return WorkflowView{}, nil, err
	}
	if resp.Status != http.StatusCreated && resp.Status != http.StatusOK {
		return WorkflowView{}, resp, fmt.Errorf("POST /v1/workflows answered %d: %s",
			resp.Status, firstLine(resp.Body))
	}
	var wf WorkflowView
	if err := json.Unmarshal(resp.Body, &wf); err != nil {
		return WorkflowView{}, resp, fmt.Errorf("unreadable start response: %w", err)
	}
	return wf, resp, nil
}

// GetWorkflow reads one workflow's status.
func (c *Cluster) GetWorkflow(ctx context.Context, id string) (WorkflowView, *Response, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/v1/workflows/"+id, nil)
	if err != nil {
		return WorkflowView{}, nil, err
	}
	if resp.Status != http.StatusOK {
		return WorkflowView{}, resp, fmt.Errorf("GET /v1/workflows/%s answered %d: %s",
			id, resp.Status, firstLine(resp.Body))
	}
	var wf WorkflowView
	if err := json.Unmarshal(resp.Body, &wf); err != nil {
		return WorkflowView{}, resp, fmt.Errorf("unreadable workflow response: %w", err)
	}
	return wf, resp, nil
}

// History reads one workflow's full history through the query API.
func (c *Cluster) History(ctx context.Context, id string) (historyResponse, *Response, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/v1/workflows/"+id+"/history", nil)
	if err != nil {
		return historyResponse{}, nil, err
	}
	if resp.Status != http.StatusOK {
		return historyResponse{}, resp, fmt.Errorf("GET /v1/workflows/%s/history answered %d: %s",
			id, resp.Status, firstLine(resp.Body))
	}
	var h historyResponse
	if err := json.Unmarshal(resp.Body, &h); err != nil {
		return historyResponse{}, resp, fmt.Errorf("unreadable history response: %w", err)
	}
	return h, resp, nil
}

type signalBody struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Signal delivers an external signal into a running workflow.
func (c *Cluster) Signal(ctx context.Context, id, name string, payload any) (*Response, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return c.Do(ctx, http.MethodPost, "/v1/workflows/"+id+"/signals",
		signalBody{Name: name, Payload: raw})
}

// ListWorkflows reads executions from one node, which answers for its own
// shards only.
type listResponse struct {
	Count      int            `json:"count"`
	Executions []WorkflowView `json:"executions"`
}

func (c *Cluster) ListWorkflows(ctx context.Context, baseURL, status string, limit int) ([]WorkflowView, error) {
	path := fmt.Sprintf("/v1/workflows?limit=%d", limit)
	if status != "" {
		path += "&status=" + status
	}
	resp, err := c.get(ctx, baseURL, path)
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("GET %s on %s answered %d", path, baseURL, resp.Status)
	}
	var lr listResponse
	if err := json.Unmarshal(resp.Body, &lr); err != nil {
		return nil, fmt.Errorf("unreadable list response: %w", err)
	}
	return lr.Executions, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
