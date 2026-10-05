package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Gathering and judging are kept apart on purpose. Gather is the only code
// that talks to the cluster or the database; it records what it saw, including
// every error, in an Evidence value. Evaluate (checks.go) then decides each
// row from that value alone. The split is what lets every verdict rule be
// tested against hand-built evidence, with no cluster and no database.

const signalApproval = "HumanApproval"

// Options is everything the command line decides.
type Options struct {
	BaseURLs       []string
	DSN            string
	StandbyDSN     string
	ComposePath    string
	GoModPath      string
	Drive          bool
	Inject         bool
	WorkflowBudget time.Duration
	PollInterval   time.Duration
	TimerSleep     time.Duration
	EventLimit     int
	CompMap        map[string]string
	RunTag         string
}

// TokenSample is one reading of every shard's fencing token.
type TokenSample struct {
	Source string
	At     time.Time
	Tokens map[int32]int64
}

// RoutingProbe is one node asked directly, with no 409 chase, for a workflow
// it may or may not own.
type RoutingProbe struct {
	Node          string
	Status        int
	NamedOwner    string
	ExpectedOwner string
	OK            bool
	Detail        string
}

// Evidence is everything Gather observed. A field left empty with its Err set
// is a prerequisite that was missing, and the rows that need it go BLOCKED.
type Evidence struct {
	Opts  Options
	Nodes []*NodeInfo

	ShardViews     []ShardView
	ShardViewsErr  string
	ShardViewsFrom string
	TokenSamples   []TokenSample

	StoreErr string
	// StoreRead is set only once readStore has actually run. An empty result
	// with no error must never be mistaken for a query that found nothing.
	StoreRead       bool
	Events          []EventRow
	EventsTruncated bool
	EventsErr       string
	Execs           []ExecRow
	ExecsErr        string
	Leases          []LeaseRow
	LeasesAt        time.Time
	LeasesErr       string
	Orphans         []Orphan
	OrphansErr      string
	SeqDups         []string
	SeqDupsErr      string
	ReplStreams     int
	ReplErr         string
	Standby         StandbyStatus
	StandbyErr      string

	DriveNote      string
	Delivery       *Driven
	Agent          *Driven
	AgentSignalErr string
	Compensated    *Driven
	InjectErr      string

	DeliveryAPIHistory []EventRow
	DeliveryAPIErr     string
	DeliveryDBHistory  []EventRow
	DeliveryDBErr      string
	AgentHistory       []EventRow
	AgentHistoryErr    string

	Routing []RoutingProbe

	Metrics    map[string]MetricSet
	MetricsErr map[string]string

	ExpectedMetrics []string
	VectorMetrics   map[string]bool

	Compose ComposeScan
	GoMod   ModScan
}

func newEvidence(opts Options) *Evidence {
	return &Evidence{
		Opts:       opts,
		Metrics:    map[string]MetricSet{},
		MetricsErr: map[string]string{},
	}
}

func (ev *Evidence) healthy() []*NodeInfo {
	var out []*NodeInfo
	for _, n := range ev.Nodes {
		if n.Healthy {
			out = append(out, n)
		}
	}
	return out
}

// dbWhy explains why a database-backed value is unavailable.
func (ev *Evidence) dbWhy(specific string) string {
	switch {
	case specific != "":
		return specific
	case ev.StoreErr != "":
		return ev.StoreErr
	}
	return "the database was not read"
}

// clusterReason says why no node could be used, naming each URL's error.
func (ev *Evidence) clusterReason() string {
	if len(ev.Nodes) == 0 {
		return "no -cluster URL was given"
	}
	parts := make([]string, 0, len(ev.Nodes))
	for _, n := range ev.Nodes {
		msg := n.Err
		if msg == "" {
			msg = "not healthy"
		}
		parts = append(parts, n.BaseURL+": "+msg)
	}
	return strings.Join(parts, "; ")
}

// Gather runs every observation. It never panics on an unreachable
// dependency: each failure is written into the evidence and the run goes on.
func Gather(ctx context.Context, opts Options, c *Cluster,
	open func(context.Context, string) (Store, error), logw io.Writer) *Evidence {

	ev := newEvidence(opts)
	logf := func(format string, args ...any) {
		fmt.Fprintf(logw, "acceptance: "+format+"\n", args...)
	}

	logf("asking %d node(s) for /healthz", len(c.Nodes))
	c.Discover(ctx)
	ev.Nodes = c.Nodes
	logf("%d of %d node(s) healthy", len(ev.healthy()), len(ev.Nodes))

	var store Store
	if opts.DSN == "" {
		ev.StoreErr = "no -dsn was given"
	} else {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := open(sctx, opts.DSN)
		cancel()
		if err != nil || st == nil {
			ev.StoreErr = fmt.Sprintf("could not connect: %v", err)
			logf("database unavailable: %s", ev.StoreErr)
		} else {
			store = st
			defer st.Close()
		}
	}

	sample := func(label string) {
		if store != nil {
			if leases, err := store.Leases(ctx); err == nil {
				ev.TokenSamples = append(ev.TokenSamples, TokenSample{
					Source: "db/" + label, At: time.Now(), Tokens: leaseTokens(leases)})
			}
		}
		for _, n := range ev.healthy() {
			if views, _, err := c.Shards(ctx, n.BaseURL); err == nil {
				ev.TokenSamples = append(ev.TokenSamples, TokenSample{
					Source: n.BaseURL + "/" + label, At: time.Now(), Tokens: viewTokens(views)})
			}
		}
	}

	sample("start")
	driveWorkflows(ctx, c, ev, sample, logf)
	sample("after-drive")

	if h := ev.healthy(); len(h) > 0 {
		for _, n := range h {
			views, _, err := c.Shards(ctx, n.BaseURL)
			if err == nil {
				ev.ShardViews, ev.ShardViewsFrom, ev.ShardViewsErr = views, n.BaseURL, ""
				break
			}
			ev.ShardViewsErr = err.Error()
		}
	} else {
		ev.ShardViewsErr = ev.clusterReason()
	}

	if ev.Delivery != nil && ev.Delivery.Started {
		ev.Routing = probeRouting(ctx, c, ev, ev.Delivery)
		h, _, err := c.History(ctx, ev.Delivery.ID)
		if err != nil {
			ev.DeliveryAPIErr = err.Error()
		} else {
			ev.DeliveryAPIHistory = h.Events
		}
	}

	if store != nil {
		logf("reading the event log (up to %d rows)", opts.EventLimit)
		readStore(ctx, store, ev)
	} else {
		for _, p := range []*string{&ev.EventsErr, &ev.ExecsErr, &ev.LeasesErr, &ev.OrphansErr,
			&ev.SeqDupsErr, &ev.ReplErr, &ev.DeliveryDBErr, &ev.AgentHistoryErr} {
			*p = ev.StoreErr
		}
	}
	if len(ev.AgentHistory) == 0 && ev.Agent != nil && ev.Agent.Started {
		if h, _, err := c.History(ctx, ev.Agent.ID); err == nil {
			ev.AgentHistory, ev.AgentHistoryErr = h.Events, ""
		}
	}

	if opts.StandbyDSN != "" {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := ProbeStandby(sctx, opts.StandbyDSN)
		cancel()
		ev.Standby = st
		if err != nil {
			ev.StandbyErr = err.Error()
		}
	} else {
		ev.StandbyErr = "no -standby-dsn was given"
	}

	for _, n := range ev.healthy() {
		text, err := c.Metrics(ctx, n.BaseURL)
		if err != nil {
			ev.MetricsErr[n.BaseURL] = err.Error()
			continue
		}
		ev.Metrics[n.BaseURL] = ParseMetrics(text)
	}
	ev.ExpectedMetrics, ev.VectorMetrics = ExpectedMetricNames(), vectorMetricNames()

	ev.Compose = ScanCompose(opts.ComposePath)
	ev.GoMod = ScanGoMod(opts.GoModPath)
	sample("end")
	return ev
}

// driveWorkflows starts the tool's own workflows and waits for them.
func driveWorkflows(ctx context.Context, c *Cluster, ev *Evidence, sample func(string), logf func(string, ...any)) {
	opts := ev.Opts
	switch {
	case !opts.Drive:
		ev.DriveNote = "driving was switched off with -drive=false"
		return
	case len(ev.healthy()) == 0:
		ev.DriveNote = "no engine node answered /healthz (" + ev.clusterReason() + ")"
		return
	}

	ev.Delivery = &Driven{ID: "acceptance-delivery-" + opts.RunTag, Type: wfDelivery}
	logf("starting %s %s (durable sleep %s)", wfDelivery, ev.Delivery.ID, opts.TimerSleep)
	c.Start(ctx, ev.Delivery, deliveryOrder(ev.Delivery.ID, opts.TimerSleep))

	// The approval is sent straight after the start, while the agent is
	// still planning, so it lands before the workflow reaches its wait. That
	// is the case FR-5 is about.
	ev.Agent = &Driven{ID: "acceptance-agent-" + opts.RunTag, Type: wfAgent}
	logf("starting %s %s and sending %s immediately", wfAgent, ev.Agent.ID, signalApproval)
	began := time.Now()
	c.Start(ctx, ev.Agent, agentRequest(ev.Agent.ID))
	if ev.Agent.Started {
		err := c.SendSignal(ctx, ev.Agent, began, signalApproval, map[string]any{
			"approved": true,
			"reviewer": "acceptance-tool",
			"note":     "sent before the workflow waits, to test buffering",
		})
		if err != nil {
			ev.AgentSignalErr = err.Error()
		}
	}

	if opts.Inject {
		ev.Compensated = &Driven{ID: "acceptance-compensate-" + opts.RunTag, Type: wfDelivery}
		logf("starting %s and arming a failure on DispatchDriver (-inject)", ev.Compensated.ID)
		c.Start(ctx, ev.Compensated, deliveryOrder(ev.Compensated.ID, opts.TimerSleep))
		if ev.Compensated.Started {
			if err := c.ArmFailure(ctx, ev.Compensated.ID, "DispatchDriver"); err != nil {
				ev.InjectErr = err.Error()
			}
		}
	}

	sample("after-start")
	logf("waiting up to %s for the driven workflows to finish", opts.WorkflowBudget)
	var wg sync.WaitGroup
	for _, d := range []*Driven{ev.Delivery, ev.Agent, ev.Compensated} {
		if d == nil || !d.Started {
			continue
		}
		wg.Add(1)
		go func(d *Driven) {
			defer wg.Done()
			c.WaitTerminal(ctx, d, opts.WorkflowBudget, opts.PollInterval)
		}(d)
	}
	wg.Wait()
}

func readStore(ctx context.Context, store Store, ev *Evidence) {
	ev.StoreRead = true
	var err error
	if ev.Events, ev.EventsTruncated, err = store.Events(ctx, ev.Opts.EventLimit); err != nil {
		ev.EventsErr = err.Error()
	}
	if ev.Execs, err = store.Executions(ctx); err != nil {
		ev.ExecsErr = err.Error()
	}
	ev.LeasesAt = time.Now()
	if ev.Leases, err = store.Leases(ctx); err != nil {
		ev.LeasesErr = err.Error()
	}
	if ev.Orphans, err = store.Orphans(ctx); err != nil {
		ev.OrphansErr = err.Error()
	}
	if ev.SeqDups, err = store.SeqKeyDuplicates(ctx); err != nil {
		ev.SeqDupsErr = err.Error()
	}
	if ev.ReplStreams, err = store.ReplicationStreams(ctx); err != nil {
		ev.ReplErr = err.Error()
	}
	if ev.Delivery != nil && ev.Delivery.Started {
		if ev.DeliveryDBHistory, err = store.WorkflowEvents(ctx, ev.Delivery.ID); err != nil {
			ev.DeliveryDBErr = err.Error()
		}
	} else {
		ev.DeliveryDBErr = "no workflow was driven"
	}
	if ev.Agent != nil && ev.Agent.Started {
		if ev.AgentHistory, err = store.WorkflowEvents(ctx, ev.Agent.ID); err != nil {
			ev.AgentHistoryErr = err.Error()
		}
	} else {
		ev.AgentHistoryErr = "no workflow was driven"
	}
}

// probeRouting asks every healthy node directly for the driven workflow.
//
// FR-6's "gateway routes correctly" has two acceptable answers from a node:
// the workflow itself (it owns the shard, or it forwarded the request), or a
// 409 naming the node that really does own the shard. Anything else — a 404,
// a 409 naming the wrong node, the right id on the wrong shard — is a routing
// fault. Each node is asked once, with no retry, because a retry would hide
// the very answer being tested.
func probeRouting(ctx context.Context, c *Cluster, ev *Evidence, d *Driven) []RoutingProbe {
	owner := ""
	for _, v := range ev.ShardViews {
		if v.ShardID == d.ShardID {
			owner = v.Owner
		}
	}
	var out []RoutingProbe
	for _, n := range ev.healthy() {
		p := RoutingProbe{Node: n.BaseURL, ExpectedOwner: owner}
		resp, err := c.send(ctx, n.BaseURL, http.MethodGet, "/v1/workflows/"+d.ID, nil)
		if err != nil {
			p.Detail = err.Error()
			out = append(out, p)
			continue
		}
		p.Status = resp.Status
		p.OK, p.NamedOwner, p.Detail = judgeRoutingAnswer(resp, d.ID, d.ShardID, owner)
		out = append(out, p)
	}
	return out
}

// judgeRoutingAnswer is the pure half of probeRouting.
func judgeRoutingAnswer(resp *Response, id string, shard int32, owner string) (ok bool, named, detail string) {
	switch resp.Status {
	case http.StatusOK:
		var wf WorkflowView
		if err := json.Unmarshal(resp.Body, &wf); err != nil {
			return false, "", "200 with an unreadable body"
		}
		if wf.WorkflowID != id || wf.ShardID != shard {
			return false, "", fmt.Sprintf("200 but returned %s on shard %d, expected %s on shard %d",
				wf.WorkflowID, wf.ShardID, id, shard)
		}
		return true, "", "200, correct workflow and shard"
	case http.StatusConflict:
		named = ownerFromRefusal(resp)
		if owner == "" {
			return false, named, "409 naming " + named + ", but the real owner is unknown"
		}
		if named != owner {
			return false, named, fmt.Sprintf("409 naming %s, but /v1/shards says %s owns shard %d", named, owner, shard)
		}
		return true, named, "409 naming the correct owner " + named
	}
	return false, "", fmt.Sprintf("answered %d: %s", resp.Status, firstLine(resp.Body))
}

func leaseTokens(leases []LeaseRow) map[int32]int64 {
	out := make(map[int32]int64, len(leases))
	for _, l := range leases {
		out[l.ShardID] = l.Token
	}
	return out
}

func viewTokens(views []ShardView) map[int32]int64 {
	out := make(map[int32]int64, len(views))
	for _, v := range views {
		out[v.ShardID] = v.Token
	}
	return out
}

// tokenSeries turns samples into one series per shard, in time order.
func tokenSeries(samples []TokenSample) map[int32][]int64 {
	ordered := append([]TokenSample(nil), samples...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })
	out := map[int32][]int64{}
	for _, s := range ordered {
		for shard, tok := range s.Tokens {
			out[shard] = append(out[shard], tok)
		}
	}
	return out
}
