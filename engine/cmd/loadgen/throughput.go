package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// nfr5Target is the design document's claim, quoted once so no two places can
// print a different version of it.
const nfr5Target = ">= 1.7x from 1 to 2 shards"

// orderInput builds a DeliveryOrder workflow input.
//
// PrepTime is set explicitly and small. The workflow's own default is a
// twelve-minute durable sleep between the restaurant accepting and a driver
// being dispatched, which is right for a food delivery and impossible for a
// throughput benchmark. Overriding it is a change to the workload, not to the
// engine, and it is reported in the output so nobody reads a number here as
// the throughput of the twelve-minute workflow.
func orderInput(orderID string, prep time.Duration, amountCents int64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"order_id":      orderID,
		"customer_id":   "cust-" + orderID,
		"restaurant_id": "rest-1",
		"items": []map[string]any{
			{"sku": "sku-1", "quantity": 1},
		},
		"amount_cents": amountCents,
		"currency":     "GBP",
		"prep_time_ns": int64(prep),
	})
	return b
}

// runOutcome is what one workflow did.
type runOutcome struct {
	latency    time.Duration
	finishedAt time.Time
	status     core.ExecutionStatus
	err        error
}

// completed reports whether this run finished the work rather than merely
// stopping.
//
// The distinction is not pedantic. FAILED and COMPENSATING_FAILED are terminal
// statuses, and a workflow that fails at its first step reaches one of them in
// a few milliseconds. Counting those as throughput would mean the fastest way
// to a high number is to break the engine, which is the opposite of what
// NFR-5 is asking about. Only COMPLETED counts; everything else is reported
// separately.
func (o runOutcome) completed() bool {
	return o.err == nil && o.status == core.StatusCompleted
}

// driveOne starts a workflow and polls it to a terminal status.
//
// The returned latency is start-request-sent to terminal-status-observed. It
// is therefore an upper bound: the workflow finished at some point in the
// poll interval before it was noticed. That bias is why the poll interval is
// printed alongside every latency figure.
func driveOne(ctx context.Context, c *Client, id string, cfg config, wantShard core.ShardID) runOutcome {
	t0 := time.Now()
	started, err := c.StartWorkflow(ctx, id, cfg.workflowType, orderInput(id, cfg.prep, cfg.amountCents))
	if err != nil {
		return runOutcome{err: err, finishedAt: time.Now()}
	}
	if started.ShardID != wantShard {
		// The engine hashed this id somewhere other than where the tool
		// predicted. The only way that happens is a shard count mismatch, and
		// carrying on would produce a confident number for a shard set that
		// was never actually targeted.
		return runOutcome{
			err: fmt.Errorf("id %q was routed to shard %d but -shard-count %d predicts shard %d: "+
				"the cluster's SHARD_COUNT does not match -shard-count",
				id, started.ShardID, cfg.shardCount, wantShard),
			finishedAt: time.Now(),
		}
	}

	tick := time.NewTicker(cfg.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return runOutcome{err: ctx.Err(), finishedAt: time.Now()}
		case <-tick.C:
		}
		got, err := c.GetWorkflow(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return runOutcome{err: ctx.Err(), finishedAt: time.Now()}
			}
			// A transient read failure during a failover is expected; keep
			// polling until the context says to stop.
			continue
		}
		if got.Status.IsTerminal() {
			now := time.Now()
			return runOutcome{latency: now.Sub(t0), finishedAt: now, status: got.Status}
		}
	}
}

// ---------------------------------------------------------------------------
// Throughput.
// ---------------------------------------------------------------------------

type throughputResult struct {
	Targets  []core.ShardID
	Owners   []string
	Window   time.Duration
	Warmup   time.Duration
	Started  int64
	Counted  int64
	Failed   int64
	Aborted  int64
	Latency  Summary
	Stats    ClientStats
	FirstErr error
}

// PerSecond is the headline number: workflows that reached a terminal status
// inside the measurement window, divided by the window.
func (r throughputResult) PerSecond() float64 {
	if r.Window <= 0 {
		return 0
	}
	return float64(r.Counted) / r.Window.Seconds()
}

// driveLoad runs a closed-loop load test: a fixed number of concurrent
// in-flight workflows, each replaced as soon as it finishes, for a fixed time.
//
// Closed-loop rather than a fixed arrival rate, because the question NFR-5
// asks is "how much work can this cluster get through", not "how does it
// behave at 40 requests per second". A closed loop measures capacity without
// needing a rate to be guessed first, and it cannot build an unbounded queue
// that turns the result into a measure of the queue rather than the engine.
func driveLoad(ctx context.Context, c *Client, gen *IDGen, cfg config, window time.Duration) throughputResult {
	start := time.Now()
	windowStart := start.Add(cfg.warmup)
	windowEnd := windowStart.Add(window)

	var (
		started  atomic.Int64
		counted  atomic.Int64
		failed   atomic.Int64
		aborted  atomic.Int64
		mu       sync.Mutex
		samples  []time.Duration
		firstErr error
	)

	runCtx, cancel := context.WithDeadline(ctx, windowEnd)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if runCtx.Err() != nil || !time.Now().Before(windowEnd) {
					return
				}
				id, wantShard, err := gen.Next()
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				started.Add(1)
				out := driveOne(runCtx, c, id, cfg, wantShard)

				switch {
				case out.err != nil:
					// A run cut short by the window closing is not a failure;
					// it simply did not finish in time to be counted.
					if errors.Is(out.err, context.DeadlineExceeded) || errors.Is(out.err, context.Canceled) {
						return
					}
					failed.Add(1)
					mu.Lock()
					if firstErr == nil {
						firstErr = out.err
					}
					mu.Unlock()
				case !out.completed():
					// Terminal, but the work did not get done — a failed or
					// compensated saga. Counted apart from throughput.
					aborted.Add(1)
				case out.finishedAt.Before(windowStart):
					// Completed during warm-up; deliberately not counted.
				default:
					counted.Add(1)
					mu.Lock()
					samples = append(samples, out.latency)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	// The real window is whatever actually elapsed, which matters when the run
	// was interrupted. Dividing by the requested window would inflate an
	// interrupted run's throughput.
	actual := time.Since(windowStart)
	if actual > window {
		actual = window
	}
	if actual <= 0 {
		actual = window
	}

	return throughputResult{
		Targets:  gen.Targets(),
		Window:   actual,
		Warmup:   cfg.warmup,
		Started:  started.Load(),
		Counted:  counted.Load(),
		Failed:   failed.Load(),
		Aborted:  aborted.Load(),
		Latency:  Summarise(samples),
		Stats:    c.Stats(),
		FirstErr: firstErr,
	}
}

// runThroughputMode measures one point on the scaling curve.
func runThroughputMode(ctx context.Context, c *Client, cfg config, out io.Writer) ([]Row, error) {
	res, err := measurePoint(ctx, c, cfg, cfg.shards, out)
	if err != nil {
		return nil, err
	}
	rows := throughputRows(cfg, res)

	// A single shard count cannot answer NFR-5, which is a ratio between two
	// runs. Saying so, with no number, is the whole point of this row.
	rows = append(rows, Row{
		Measurement: "throughput",
		NFR:         "NFR-5",
		Metric:      "scaling_1_to_2",
		Target:      nfr5Target,
		Verdict:     VerdictNoEvidence,
		Kind:        KindNotMeasured,
		Note: "a ratio needs two runs; use -mode scaling, or run -shards 1 and -shards 2 " +
			"into the same -csv and divide the two completed_per_sec values",
	})
	return rows, nil
}

// measurePoint resolves the shard set, warns if it does not span machines, and
// drives the load.
func measurePoint(ctx context.Context, c *Client, cfg config, shardsWanted int, out io.Writer) (throughputResult, error) {
	owners, err := ownersOf(ctx, c)
	if err != nil {
		return throughputResult{}, fmt.Errorf("reading /v1/shards: %w", err)
	}
	targets, err := selectTargets(shardsWanted, cfg.shardIDs, cfg.shardCount, owners, cfg.spread)
	if err != nil {
		return throughputResult{}, err
	}
	gen, err := NewIDGen(fmt.Sprintf("%s-s%d", cfg.idPrefix, shardsWanted), cfg.shardCount, targets)
	if err != nil {
		return throughputResult{}, err
	}

	distinct := distinctOwners(targets, owners)
	window := cfg.duration - cfg.warmup

	fmt.Fprintf(out, "\n--- throughput: %d shard(s) [%s] owned by %v ---\n",
		len(targets), shardsString(targets), distinct)
	fmt.Fprintf(out, "    window %s (after %s warm-up), concurrency %d, poll %s, prep %s\n",
		window, cfg.warmup, cfg.concurrency, cfg.poll, cfg.prep)

	res := driveLoad(ctx, c, gen, cfg, window)
	res.Owners = distinct
	res.Targets = targets

	fmt.Fprintf(out, "    completed %d in %s = %.2f wf/s (started %d, failed %d, ended non-COMPLETED %d, 409 follows %d)\n",
		res.Counted, fmtDur(res.Window), res.PerSecond(), res.Started, res.Failed, res.Aborted, res.Stats.NotOwnerFollows)
	if res.FirstErr != nil {
		fmt.Fprintf(out, "    first error: %v\n", res.FirstErr)
	}
	return res, nil
}

func throughputRows(cfg config, res throughputResult) []Row {
	shardNote := fmt.Sprintf("shards %s owned by %d node(s): %v",
		shardsString(res.Targets), len(res.Owners), res.Owners)
	if len(res.Owners) < len(res.Targets) {
		shardNote += "; fewer owners than shards, so this point measures one machine more than once " +
			"and is NOT evidence of horizontal scaling"
	}
	suffix := fmt.Sprintf("@%dshard", len(res.Targets))

	rows := []Row{
		{
			Measurement: "throughput", NFR: "NFR-5",
			Metric: "completed_per_sec " + suffix,
			Value:  fmt.Sprintf("%.3f", res.PerSecond()), Unit: "wf/s",
			Target: "no absolute target; NFR-5 is a ratio", Verdict: VerdictNoTarget,
			Kind: KindMeasured, Note: shardNote,
		},
		{
			Measurement: "throughput", NFR: "NFR-5",
			Metric: "completed " + suffix,
			Value:  fmt.Sprintf("%d", res.Counted), Unit: "workflows",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("status COMPLETED observed inside the %s window; %d started, %d errored, "+
				"%d reached a terminal status other than COMPLETED and are excluded",
				fmtDur(res.Window), res.Started, res.Failed, res.Aborted),
		},
		{
			Measurement: "throughput", NFR: "NFR-5",
			Metric: "latency_p50 " + suffix,
			Value:  durSeconds(res.Latency.P50), Unit: "s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("end to end, start request to terminal status; %s; upper bound by up to one %s poll interval",
				percentileConfidence(res.Latency.N), cfg.poll),
		},
		{
			Measurement: "throughput", NFR: "NFR-5",
			Metric: "latency_p95 " + suffix,
			Value:  durSeconds(res.Latency.P95), Unit: "s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
		},
		{
			Measurement: "throughput", NFR: "NFR-5",
			Metric: "latency_p99 " + suffix,
			Value:  durSeconds(res.Latency.P99), Unit: "s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: percentileConfidence(res.Latency.N),
		},
		{
			Measurement: "throughput", NFR: "NFR-5",
			Metric: "workload " + suffix,
			Value:  cfg.workflowType, Unit: "",
			Verdict: VerdictNoTarget, Kind: KindAssumed,
			Note: fmt.Sprintf("prep timer forced to %s (the workflow's own default is 12m); amount %d cents; "+
				"concurrency %d; these are inputs this tool chose, not properties of the engine",
				cfg.prep, cfg.amountCents, cfg.concurrency),
		},
		{
			Measurement: "throughput", NFR: "-",
			Metric: "routing_409_follows " + suffix,
			Value:  fmt.Sprintf("%d", res.Stats.NotOwnerFollows), Unit: "requests",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("requests re-sent to the named owner after a 409; %d more hit a shard nobody owned, "+
				"%d got 503 LostOwnership, %d failed at the transport",
				res.Stats.NotOwnerUnowned, res.Stats.Unavailable, res.Stats.TransportErrors),
		},
	}
	return rows
}

// ---------------------------------------------------------------------------
// Scaling.
// ---------------------------------------------------------------------------

// runScalingMode runs several shard counts back to back and divides.
//
// The ratio is computed here rather than left to the reader because it is the
// claim: NFR-5 is about the relationship between two runs, and a report that
// prints two throughputs and lets somebody else do the arithmetic has not
// actually evaluated the requirement.
func runScalingMode(ctx context.Context, c *Client, cfg config, out io.Writer) ([]Row, error) {
	results := make(map[int]throughputResult, len(cfg.sweep))
	var rows []Row

	for _, n := range cfg.sweep {
		if ctx.Err() != nil {
			break
		}
		res, err := measurePoint(ctx, c, cfg, n, out)
		if err != nil {
			return rows, err
		}
		results[n] = res
		rows = append(rows, throughputRows(cfg, res)...)
	}

	base, haveBase := results[1]
	if !haveBase || base.PerSecond() <= 0 {
		rows = append(rows, Row{
			Measurement: "scaling", NFR: "NFR-5",
			Metric: "scaling_1_to_2", Target: nfr5Target,
			Verdict: VerdictNoEvidence, Kind: KindNotMeasured,
			Note: "the 1-shard run produced no completions, so there is no baseline to divide by",
		})
		return rows, nil
	}

	for _, n := range cfg.sweep {
		if n == 1 {
			continue
		}
		res, ok := results[n]
		if !ok {
			continue
		}
		ratio := res.PerSecond() / base.PerSecond()
		metric := fmt.Sprintf("scaling_1_to_%d", n)

		row := Row{
			Measurement: "scaling", NFR: "NFR-5",
			Metric: metric,
			Value:  fmt.Sprintf("%.3f", ratio), Unit: "x",
			Kind: KindDerived,
			Note: fmt.Sprintf("%.3f wf/s at %d shards over %.3f wf/s at 1 shard; %d vs %d distinct owner node(s)",
				res.PerSecond(), n, base.PerSecond(), len(res.Owners), len(base.Owners)),
		}
		if n == 2 {
			row.Target = nfr5Target
			if ratio >= 1.7 {
				row.Verdict = VerdictMet
			} else {
				row.Verdict = VerdictNotMet
			}
			if len(res.Owners) < 2 {
				// A 2-shard run confined to one machine cannot demonstrate
				// horizontal scaling whatever ratio it produces.
				row.Verdict = VerdictNoEvidence
				row.Note += "; both shards were on one node, so this ratio is not evidence about scaling out"
			}
		} else {
			row.Target = "no target stated in the design document"
			row.Verdict = VerdictNoTarget
		}
		rows = append(rows, row)
	}
	return rows, nil
}
