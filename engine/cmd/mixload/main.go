// Command mixload drives the engine with a MIX of workflow types at once and
// reports each type separately.
//
//	-mix  DeliveryOrder=30,MixPipeline=15,MixFanOut=15,MixApproval=15,MixRetry=10,DomainAgent=15
//
// Closed loop (-concurrency clients, each starting a new workflow when its
// last one ends) or open loop (-rate starts per second, at most -max-inflight
// in flight). Every workflow is started, followed to a terminal status, and
// timed from start request to observed completion.
//
// MixApproval runs are approved by this tool: -approve-delay after the start
// it sends the approve signal, as a person would.
//
// DomainAgent runs replay the evaluation corpora's pairs (forty kinds of task)
// through the engine. After each such run the tool reads the run's history
// and records the engine's own journaled duplicate verdict and how many times
// the commit tool was actually scheduled (1 = the repeat was suppressed,
// 2 = it ran). That is the end-to-end check: the verdict as the engine
// reached it, on the engine's path, under load.
//
// Output: <out>-types.csv (one row per workflow type), <out>-agent.csv (one
// row per DomainAgent run) and a summary on stdout.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/evalcorpus"
)

type cfg struct {
	nodes        []string
	mix          []weighted
	concurrency  int
	rate         float64
	maxInflight  int
	duration     time.Duration
	warmup       time.Duration
	poll         time.Duration
	approveDelay time.Duration
	workMS       int
	steps        int
	fanout       int
	failTimes    int
	prepMS       int
	corpora      string
	prefix       string
	out          string
	seed         int64
	timeout      time.Duration
}

type weighted struct {
	name   string
	weight int
}

func main() {
	var c cfg
	var nodes, mix string
	flag.StringVar(&nodes, "nodes", "http://localhost:8080", "comma-separated engine base URLs")
	flag.StringVar(&mix, "mix", "DeliveryOrder=30,MixPipeline=15,MixFanOut=15,MixApproval=15,MixRetry=10,DomainAgent=15", "workflow type weights")
	flag.IntVar(&c.concurrency, "concurrency", 32, "closed-loop clients (ignored when -rate > 0)")
	flag.Float64Var(&c.rate, "rate", 0, "open-loop starts per second (0 = closed loop)")
	flag.IntVar(&c.maxInflight, "max-inflight", 2000, "open-loop cap on workflows in flight")
	flag.DurationVar(&c.duration, "duration", 120*time.Second, "measured window")
	flag.DurationVar(&c.warmup, "warmup", 15*time.Second, "unmeasured lead-in")
	flag.DurationVar(&c.poll, "poll", 100*time.Millisecond, "status poll interval")
	flag.DurationVar(&c.approveDelay, "approve-delay", time.Second, "MixApproval: time before the approve signal")
	flag.IntVar(&c.workMS, "work-ms", 50, "simulated work per activity of the mixed shapes")
	flag.IntVar(&c.steps, "pipeline-steps", 20, "MixPipeline length")
	flag.IntVar(&c.fanout, "fanout", 8, "MixFanOut width")
	flag.IntVar(&c.failTimes, "fail-times", 2, "MixRetry: failures before each call succeeds")
	flag.IntVar(&c.prepMS, "prep-ms", 50, "DeliveryOrder preparation timer")
	flag.StringVar(&c.corpora, "agent-corpora", "flight,multidomain,heldout,heldout2", "pairs DomainAgent replays")
	flag.StringVar(&c.prefix, "id-prefix", "", "workflow id prefix (default: mix-<unix time>)")
	flag.StringVar(&c.out, "out", "mixload", "output file prefix")
	flag.Int64Var(&c.seed, "seed", 1, "random seed for the mix")
	flag.DurationVar(&c.timeout, "run-timeout", 5*time.Minute, "give up following one workflow after this long")
	flag.Parse()
	c.nodes = strings.Split(nodes, ",")
	var err error
	if c.mix, err = parseMix(mix); err != nil {
		fail(err)
	}
	if c.prefix == "" {
		c.prefix = fmt.Sprintf("mix-%d", time.Now().Unix())
	}
	pairs, err := loadPairs(c.corpora)
	if err != nil {
		fail(err)
	}
	// Shuffle once, deterministically, so any window of the run samples
	// every corpus instead of the first one in the list.
	rand.New(rand.NewSource(c.seed)).Shuffle(len(pairs), func(i, j int) { pairs[i], pairs[j] = pairs[j], pairs[i] })
	if err := run(context.Background(), c, pairs, os.Stdout); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mixload:", err)
	os.Exit(1)
}

func parseMix(s string) ([]weighted, error) {
	var out []weighted
	for _, part := range strings.Split(s, ",") {
		name, w, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, fmt.Errorf("bad -mix entry %q (want Type=weight)", part)
		}
		n, err := strconv.Atoi(w)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad weight in %q", part)
		}
		if n > 0 {
			out = append(out, weighted{name, n})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("-mix has no positive weight")
	}
	return out, nil
}

type agentPair struct {
	corpus string
	p      evalcorpus.Pair
}

func loadPairs(spec string) ([]agentPair, error) {
	var out []agentPair
	for _, name := range strings.Split(spec, ",") {
		var ps []evalcorpus.Pair
		var err error
		switch strings.TrimSpace(name) {
		case "":
			continue
		case "flight":
			ps, err = evalcorpus.Corpus()
			for i := range ps {
				if ps[i].Activity == "" {
					ps[i].Activity = "BookFlight"
				}
				if ps[i].Domain == "" {
					ps[i].Domain = "flight"
				}
			}
		case "multidomain":
			ps, err = evalcorpus.MultiDomain()
		case "heldout":
			ps, err = evalcorpus.HeldOut()
		case "heldout2":
			ps, err = evalcorpus.HeldOut2()
		default:
			return nil, fmt.Errorf("unknown corpus %q", name)
		}
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			out = append(out, agentPair{name, p})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// HTTP client.
// ---------------------------------------------------------------------------

type client struct {
	nodes           []string
	next            atomic.Uint64
	hc              *http.Client
	notOwnerRetries atomic.Int64
}

func (c *client) base() string { return c.nodes[int(c.next.Add(1))%len(c.nodes)] }

// do sends one request, retrying a 409/503 (a shard briefly without an owner)
// with backoff for up to ~6 s. Any node can be asked: a non-owner forwards.
func (c *client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	delay := 100 * time.Millisecond
	var last error
	for attempt := 0; attempt < 8; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rdr)
		if err != nil {
			return 0, err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			last = err
		} else {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode/100 == 2:
				if out != nil {
					if err := json.Unmarshal(b, out); err != nil {
						return resp.StatusCode, err
					}
				}
				return resp.StatusCode, nil
			case resp.StatusCode == 409 || resp.StatusCode == 503:
				c.notOwnerRetries.Add(1)
				last = fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, clip(string(b)))
			default:
				return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, clip(string(b)))
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 1600*time.Millisecond {
			delay *= 2
		}
	}
	return 0, last
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

type wfResp struct {
	Status core.ExecutionStatus `json:"status"`
}

type histResp struct {
	Events []core.Event `json:"events"`
}

// ---------------------------------------------------------------------------
// One workflow.
// ---------------------------------------------------------------------------

type outcome struct {
	wfType string
	start  time.Time
	end    time.Time
	status core.ExecutionStatus
	err    error
	agent  *agentRow
}

type agentRow struct {
	corpus, domain, pairID, truth, tool string
	hard                                bool
	verdict, detail                     string
	scheduled                           int // real executions of the commit tool
}

func (c cfg) input(wfType string, id string, ap *agentPair) any {
	switch wfType {
	case "DeliveryOrder":
		return map[string]any{
			"order_id": id, "customer_id": "cust-" + id, "restaurant_id": "rest-1",
			"items":        []map[string]any{{"sku": "sku-1", "quantity": 1}},
			"amount_cents": 1999, "currency": "GBP", "prep_time_ns": int64(c.prepMS) * int64(time.Millisecond),
		}
	case "MixPipeline":
		return map[string]int{"steps": c.steps, "work_ms": c.workMS}
	case "MixFanOut":
		return map[string]int{"steps": c.fanout, "work_ms": c.workMS}
	case "MixApproval":
		return map[string]int{"work_ms": c.workMS}
	case "MixRetry":
		return map[string]int{"calls": 3, "fail_times": c.failTimes, "work_ms": c.workMS}
	case "DomainAgent":
		return map[string]any{"tool": ap.p.Activity, "first": ap.p.A, "second": ap.p.B, "work_ms": c.workMS}
	}
	return map[string]any{}
}

func driveOne(ctx context.Context, cl *client, c cfg, wfType, id string, ap *agentPair) outcome {
	o := outcome{wfType: wfType, start: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body := map[string]any{"workflow_id": id, "workflow_type": wfType, "input": c.input(wfType, id, ap)}
	if _, err := cl.do(ctx, http.MethodPost, "/v1/workflows", body, nil); err != nil {
		o.err, o.end = err, time.Now()
		return o
	}
	approved := wfType != "MixApproval"
	approveAt := o.start.Add(c.approveDelay)
	for {
		if !approved && time.Now().After(approveAt) {
			if _, err := cl.do(ctx, http.MethodPost, "/v1/workflows/"+id+"/signals",
				map[string]any{"name": "approve", "payload": map[string]any{"approved": true, "by": "mixload"}}, nil); err == nil {
				approved = true
			}
		}
		select {
		case <-ctx.Done():
			o.err, o.end = ctx.Err(), time.Now()
			return o
		case <-time.After(c.poll):
		}
		var w wfResp
		if _, err := cl.do(ctx, http.MethodGet, "/v1/workflows/"+id, nil, &w); err != nil {
			continue
		}
		if w.Status.IsTerminal() {
			o.status, o.end = w.Status, time.Now()
			break
		}
	}
	if ap != nil {
		row := &agentRow{corpus: ap.corpus, domain: ap.p.Domain, pairID: ap.p.ID, tool: ap.p.Activity,
			hard: ap.p.Hard, truth: "DISTINCT"}
		if ap.p.Truth == evalcorpus.Duplicate {
			row.truth = "DUPLICATE"
		}
		var h histResp
		if _, err := cl.do(ctx, http.MethodGet, "/v1/workflows/"+id+"/history", nil, &h); err == nil {
			tool := "Agent" + ap.p.Activity
			toolK := map[int]bool{}
			conf := map[int]string{}
			for _, e := range h.Events {
				switch e.Type {
				case core.EvActivityScheduled:
					var s core.ActivityScheduled
					if json.Unmarshal(e.Payload, &s) == nil && s.ActivityType == tool {
						toolK[s.K] = true
					}
				case core.EvActivityCompleted:
					var a core.ActivityCompleted
					if json.Unmarshal(e.Payload, &a) == nil {
						var r struct {
							Confirmation string `json:"confirmation"`
						}
						if json.Unmarshal(a.Result, &r) == nil && r.Confirmation != "" {
							conf[a.K] = r.Confirmation
						}
					}
				case core.EvDedupEvaluated:
					var d core.DedupEvaluated
					if json.Unmarshal(e.Payload, &d) == nil && d.K > 0 {
						row.verdict, row.detail = string(d.Verdict), d.Detail
					}
				}
			}
			// Each real execution of the commit tool mints its own
			// confirmation (from its dedup key). A suppressed call carries
			// the earlier call's confirmation, so the number of DISTINCT
			// confirmations is the number of times the action really ran.
			distinct := map[string]bool{}
			for k := range toolK {
				if c, ok := conf[k]; ok {
					distinct[c] = true
				}
			}
			row.scheduled = len(distinct)
		}
		o.agent = row
	}
	return o
}

// ---------------------------------------------------------------------------
// The run.
// ---------------------------------------------------------------------------

func run(ctx context.Context, c cfg, pairs []agentPair, w io.Writer) error {
	cl := &client{nodes: c.nodes, hc: &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 512, MaxConnsPerHost: 0}}}
	rng := rand.New(rand.NewSource(c.seed))
	var rngMu sync.Mutex
	total := 0
	for _, m := range c.mix {
		total += m.weight
	}
	pick := func() string {
		rngMu.Lock()
		defer rngMu.Unlock()
		n := rng.Intn(total)
		for _, m := range c.mix {
			if n < m.weight {
				return m.name
			}
			n -= m.weight
		}
		return c.mix[0].name
	}
	var seq atomic.Int64
	var pairIdx atomic.Int64
	nextPair := func() *agentPair {
		if len(pairs) == 0 {
			return nil
		}
		i := int(pairIdx.Add(1)-1) % len(pairs)
		return &pairs[i]
	}

	start := time.Now()
	measureFrom := start.Add(c.warmup)
	stopAt := measureFrom.Add(c.duration)
	var mu sync.Mutex
	var outs []outcome
	record := func(o outcome) {
		mu.Lock()
		outs = append(outs, o)
		mu.Unlock()
	}
	launch := func() {
		t := pick()
		var ap *agentPair
		if t == "DomainAgent" {
			ap = nextPair()
		}
		id := fmt.Sprintf("%s-%d", c.prefix, seq.Add(1))
		record(driveOne(ctx, cl, c, t, id, ap))
	}

	var wg sync.WaitGroup
	if c.rate <= 0 {
		for i := 0; i < c.concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(stopAt) {
					launch()
				}
			}()
		}
	} else {
		sem := make(chan struct{}, c.maxInflight)
		interval := time.Duration(float64(time.Second) / c.rate)
		next := time.Now()
		dropped := 0
		for time.Now().Before(stopAt) {
			if d := time.Until(next); d > 0 {
				time.Sleep(d)
			}
			next = next.Add(interval)
			select {
			case sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					launch()
				}()
			default:
				dropped++
			}
		}
		if dropped > 0 {
			fmt.Fprintf(w, "open loop: %d starts dropped at the in-flight cap\n", dropped)
		}
	}
	wg.Wait()
	return report(c, outs, measureFrom, stopAt, cl.notOwnerRetries.Load(), w)
}

type typeStats struct {
	started, completed, failed, errs int
	lat                              []time.Duration
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	i := int(p*float64(len(ds)-1) + 0.5)
	return ds[i]
}

func report(c cfg, outs []outcome, from, to time.Time, retries int64, w io.Writer) error {
	byType := map[string]*typeStats{}
	all := &typeStats{}
	var agents []*agentRow
	window := to.Sub(from).Seconds()
	for _, o := range outs {
		// A workflow counts towards the measured window if it ended inside it.
		if o.end.Before(from) || o.end.After(to.Add(c.timeout)) {
			continue
		}
		s := byType[o.wfType]
		if s == nil {
			s = &typeStats{}
			byType[o.wfType] = s
		}
		for _, t := range []*typeStats{s, all} {
			t.started++
			switch {
			case o.err != nil:
				t.errs++
			case o.status == core.StatusCompleted:
				if !o.end.After(to) {
					t.completed++
				}
				t.lat = append(t.lat, o.end.Sub(o.start))
			default:
				t.failed++
			}
		}
		if o.agent != nil && o.err == nil {
			agents = append(agents, o.agent)
		}
	}
	var names []string
	for n := range byType {
		names = append(names, n)
	}
	sort.Strings(names)
	names = append(names, "ALL")
	byType["ALL"] = all

	f, err := os.Create(c.out + "-types.csv")
	if err != nil {
		return err
	}
	cw := csv.NewWriter(f)
	_ = cw.Write([]string{"type", "started", "completed_in_window", "wf_per_s", "failed", "errors", "p50_s", "p95_s", "p99_s"})
	fmt.Fprintf(w, "window %.0fs, %d not-owner/unavailable retries\n", window, retries)
	fmt.Fprintf(w, "%-14s %8s %9s %8s %6s %6s %8s %8s %8s\n", "type", "started", "completed", "wf/s", "failed", "errors", "p50", "p95", "p99")
	for _, n := range names {
		s := byType[n]
		sort.Slice(s.lat, func(i, j int) bool { return s.lat[i] < s.lat[j] })
		rate := float64(s.completed) / window
		p50, p95, p99 := pct(s.lat, .5), pct(s.lat, .95), pct(s.lat, .99)
		fmt.Fprintf(w, "%-14s %8d %9d %8.2f %6d %6d %7.2fs %7.2fs %7.2fs\n", n, s.started, s.completed, rate, s.failed, s.errs,
			p50.Seconds(), p95.Seconds(), p99.Seconds())
		_ = cw.Write([]string{n, strconv.Itoa(s.started), strconv.Itoa(s.completed), fmt.Sprintf("%.3f", rate),
			strconv.Itoa(s.failed), strconv.Itoa(s.errs), fmt.Sprintf("%.3f", p50.Seconds()), fmt.Sprintf("%.3f", p95.Seconds()), fmt.Sprintf("%.3f", p99.Seconds())})
	}
	cw.Flush()
	f.Close()

	if len(agents) == 0 {
		return nil
	}
	af, err := os.Create(c.out + "-agent.csv")
	if err != nil {
		return err
	}
	aw := csv.NewWriter(af)
	_ = aw.Write([]string{"corpus", "domain", "tool", "pair_id", "truth", "hard", "verdict", "executions", "detail"})
	type tally struct{ dup, caught, dist, wrong, esc, mismatch int }
	byCorpus := map[string]*tally{}
	for _, a := range agents {
		_ = aw.Write([]string{a.corpus, a.domain, a.tool, a.pairID, a.truth, strconv.FormatBool(a.hard), a.verdict, strconv.Itoa(a.scheduled), a.detail})
		t := byCorpus[a.corpus]
		if t == nil {
			t = &tally{}
			byCorpus[a.corpus] = t
		}
		suppressed := a.scheduled == 1
		// The verdict and the side effect must agree: DUPLICATE means the
		// tool ran once, anything else means it ran twice.
		if suppressed != (a.verdict == string(core.VerdictDuplicate)) {
			t.mismatch++
		}
		if a.verdict == string(core.VerdictEscalate) {
			t.esc++
		}
		if a.truth == "DUPLICATE" {
			t.dup++
			if suppressed {
				t.caught++
			}
		} else {
			t.dist++
			if suppressed {
				t.wrong++
			}
		}
	}
	aw.Flush()
	af.Close()
	fmt.Fprintf(w, "\nDomainAgent, the engine's own verdicts (suppressed = the commit tool really ran once):\n")
	var cs []string
	for k := range byCorpus {
		cs = append(cs, k)
	}
	sort.Strings(cs)
	for _, k := range cs {
		t := byCorpus[k]
		fmt.Fprintf(w, "  %-12s repeats suppressed %d/%d, different actions suppressed %d/%d, escalated %d, verdict/side-effect mismatches %d\n",
			k, t.caught, t.dup, t.wrong, t.dist, t.esc, t.mismatch)
	}
	return nil
}
