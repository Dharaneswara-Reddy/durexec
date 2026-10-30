package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Open-loop load.
//
// driveLoad in throughput.go is CLOSED-loop: a fixed number of goroutines, each
// starting a new workflow the moment its last one finishes. Little's Law then
// holds exactly, so its throughput is
//
//	concurrency / mean latency
//
// and nothing else. That is a property of the generator, not of the engine, and
// it means a closed loop CANNOT find a saturation point: when the engine is
// faster than the generator, the number reported is the generator's own limit.
//
// This is not hypothetical here. Two of the project's published points are
// generator-bound, and both were recorded as engine capacity:
//
//	shards  concurrency  recorded wf/s  recorded p50  concurrency/p50
//	     4           64          32.10     2.0081 s          31.87
//	     4          128          46.30     2.0152 s          63.52
//
// At 4 shards and concurrency 64 the recorded figure is within 0.7% of
// 64/p50 — the generator's limit — and p50 sits exactly on the workload's own
// service floor of about 2.01 s (560 ms of activity sleeps, a 50 ms prep timer,
// a 1 s timer-scanner tick and up to one 100 ms status poll). A queue that has
// drained to the service floor is an engine waiting for work, not a saturated
// one. The 2-shards-to-4 doubling therefore does not survive: its numerator is
// a lower bound, and a ratio whose numerator is a lower bound is not a ratio.
// The 1-to-2 doubling does survive, because both of those points have a p50
// well above the floor.
//
// An open loop fixes the ARRIVAL RATE instead of the number of workflows in
// flight. Arrivals keep coming whether or not earlier ones have finished, so
// the queue is allowed to grow, and the engine's capacity shows up as the rate
// at which completions stop keeping pace with arrivals. That is the only shape
// that can answer "how much can this cluster take".
//
// It also has a failure mode of its own, which is why this file counts so many
// things separately: a generator that cannot keep to its own schedule reports
// ITS limit again, just less obviously. Hence lag, drops, and a gate that
// refuses to call a point saturated unless the evidence supports it.

// arrivalResult is one open-loop point. Every quantity is counted separately on
// purpose: the honest reading of a point depends on all of them, and collapsing
// them into "throughput" is how a generator-bound run gets published.
type arrivalResult struct {
	Targets []core.ShardID
	Owners  []string
	Window  time.Duration
	Warmup  time.Duration

	// Rate is the arrival rate the schedule asked for, in workflows/second.
	Rate float64
	// ServiceFloor is the declared lower bound on this workload's latency.
	ServiceFloor time.Duration

	// Scheduled is how many arrivals the schedule called for in the window.
	Scheduled int64
	// Issued is how many start requests were actually sent.
	Issued int64
	// Dropped is how many arrivals were skipped because no generator worker
	// was free. An arrival is dropped rather than queued: a queue inside the
	// generator would turn the open loop back into a closed one.
	Dropped int64
	// Accepted is how many starts the engine acknowledged.
	Accepted int64
	// Rejected is how many starts the engine refused (409, 503, 4xx).
	Rejected int64
	// Completed is how many of the window's arrivals reached COMPLETED at
	// all, including during the grace period after the window closed.
	Completed int64
	// CompletedInWindow is how many actually FINISHED inside the window, and
	// it is the only one of the two that is a throughput. Dividing Completed
	// by the window counts work that drained during grace as though it had
	// been done during the window, which measured 50.3/s against a workload
	// whose true capacity was 40/s.
	CompletedInWindow int64
	// TerminalOther is how many reached a terminal status that is not
	// COMPLETED — a failed or compensated saga. Not throughput.
	TerminalOther int64
	// TimedOut is how many were still non-terminal when the window closed.
	// On an overloaded engine this is the number that matters.
	TimedOut int64
	// Errored is how many failed at the transport or with an unexpected error.
	Errored int64

	// InFlight is sampled, not derived, so queue growth can be seen directly.
	InFlightP50 int64
	InFlightMax int64
	// InFlightSlope is the least-squares slope of in-flight against time, in
	// workflows per second. A positive slope at the end of the window means
	// the queue was still growing, i.e. the engine is past capacity.
	InFlightSlope float64

	// Lag is actualStart - dueTime per arrival: the generator's own error.
	LagP50 time.Duration
	LagP99 time.Duration
	LagMax time.Duration

	Latency  Summary
	Stats    ClientStats
	FirstErr error
}

// Verdict classifies a point. The whole purpose of this file is that a number
// cannot be published without one.
type Verdict string

const (
	// VerdictSaturated means the point is a real measurement of capacity.
	VerdictSaturated Verdict = "SATURATED"
	// VerdictGeneratorBound means the generator, not the engine, was the
	// limit. No capacity number may be taken from the point.
	VerdictGeneratorBound Verdict = "GENERATOR_BOUND"
	// VerdictNotSaturated means the engine kept up: latency never rose above
	// the workload's own service floor. The throughput is a LOWER BOUND on
	// capacity, not a measurement of it.
	VerdictNotSaturated Verdict = "NOT_SATURATED"
	// VerdictOverloaded means the queue was still growing at the end of the
	// window. Useful — it brackets capacity from above — but not a capacity
	// figure either.
	VerdictOverloaded Verdict = "OVERLOADED"
)

// Gate thresholds. Named, so the report can print the number that fired.
const (
	// maxDropFraction is how many arrivals may be dropped before the
	// generator is judged to have failed to keep its schedule.
	maxDropFraction = 0.01
	// maxLagFraction is how far p99 lag may drift, as a fraction of one
	// inter-arrival gap, before the schedule is judged unmet.
	maxLagFraction = 0.25
	// minLagAllowance floors that tolerance. At a high rate the gap is only a
	// few milliseconds, and a quarter of it is below the jitter of a Go timer
	// on a loaded host — which made the gate report GENERATOR_BOUND for a
	// generator that was 0.6ms late and had in fact delivered its rate.
	// Below this, lag is operating-system scheduling noise and says nothing
	// about whether the rate was offered.
	minLagAllowance = 5 * time.Millisecond
	// floorMargin is how far above the declared service floor p50 must sit
	// before the engine counts as queueing rather than idling.
	floorMargin = 1.05
)

// classify applies the gate. It is a pure function of the counters so it can
// be tested against the recorded runs, which is the point: the next run cannot
// quietly disagree with the judgement already made by hand about the published
// 4-shard points.
func (r arrivalResult) classify() (Verdict, string) {
	// 1. Did the generator keep its own schedule?
	if r.Scheduled > 0 {
		dropped := float64(r.Dropped) / float64(r.Scheduled)
		if dropped > maxDropFraction {
			return VerdictGeneratorBound, fmt.Sprintf(
				"the generator dropped %.1f%% of its scheduled arrivals (%d of %d), "+
					"above the %.0f%% limit: it could not offer the rate it was asked for",
				dropped*100, r.Dropped, r.Scheduled, maxDropFraction*100)
		}
	}
	if r.Rate > 0 {
		gap := time.Duration(float64(time.Second) / r.Rate)
		limit := time.Duration(float64(gap) * maxLagFraction)
		if limit < minLagAllowance {
			limit = minLagAllowance
		}
		if r.LagP99 > limit {
			return VerdictGeneratorBound, fmt.Sprintf(
				"p99 generator lag was %s against a %s inter-arrival gap (limit %s): "+
					"arrivals were late, so the offered rate was not %.2f/s",
				fmtDur(r.LagP99), fmtDur(gap), fmtDur(limit), r.Rate)
		}
	}

	// 2. Was the engine actually queueing? A p50 on the workload's own service
	// floor means the queue had drained and the engine was waiting for work,
	// whatever the throughput says. This is the test that the published
	// 4-shard points fail.
	if r.ServiceFloor > 0 {
		need := time.Duration(float64(r.ServiceFloor) * floorMargin)
		if r.Latency.P50 > 0 && r.Latency.P50 < need {
			return VerdictNotSaturated, fmt.Sprintf(
				"p50 was %s, not more than %.0f%% above the declared %s service floor: the engine "+
					"drained its queue and was waiting for work, so %.2f wf/s is a LOWER "+
					"BOUND on capacity, not a measurement of it%s",
				fmtDur(r.Latency.P50), (floorMargin-1)*100, fmtDur(r.ServiceFloor),
				r.CompletedPerSecond(), belowFloorNote(r))
		}
	}

	// 3. Was it in steady state? A queue still growing at the end of the
	// window means the arrival rate is above capacity.
	if r.InFlightSlope > 0 && r.TimedOut > 0 {
		return VerdictOverloaded, fmt.Sprintf(
			"in-flight work grew at %.2f workflows/s through the window and %d were "+
				"still unfinished when it closed: the arrival rate is above capacity, "+
				"so this point brackets capacity from above",
			r.InFlightSlope, r.TimedOut)
	}

	return VerdictSaturated, fmt.Sprintf(
		"the generator kept its schedule (%d of %d arrivals, p99 lag %s), p50 %s is "+
			"above the %s floor, and in-flight work was stable (%+.2f/s)",
		r.Issued, r.Scheduled, fmtDur(r.LagP99), fmtDur(r.Latency.P50),
		fmtDur(r.ServiceFloor), r.InFlightSlope)
}

// CompletedPerSecond is completions per second of the measurement window.
func (r arrivalResult) CompletedPerSecond() float64 {
	if r.Window <= 0 {
		return 0
	}
	return float64(r.CompletedInWindow) / r.Window.Seconds()
}

// OfferedPerSecond is the rate actually offered, which is not the rate asked
// for if any arrival was dropped or late.
func (r arrivalResult) OfferedPerSecond() float64 {
	if r.Window <= 0 {
		return 0
	}
	return float64(r.Issued) / r.Window.Seconds()
}

// driveArrivals runs one open-loop point at a fixed arrival rate.
func driveArrivals(ctx context.Context, c *Client, gen *IDGen, cfg config,
	rate float64, window time.Duration) arrivalResult {

	start := time.Now()
	windowStart := start.Add(cfg.warmup)
	windowEnd := windowStart.Add(window)
	// Workflows started before the window closes are given until grace to
	// finish; anything still running then is counted as timed out rather than
	// silently dropped, which is what a closed loop does with them.
	hardEnd := windowEnd.Add(cfg.grace)

	var (
		scheduled, issued, dropped atomic.Int64
		accepted, rejected         atomic.Int64
		completed, terminalOther   atomic.Int64
		completedInWindow          atomic.Int64
		timedOut, errored          atomic.Int64
		inFlight                   atomic.Int64
		mu                         sync.Mutex
		samples                    []time.Duration
		lags                       []time.Duration
		inFlightSeries             []int64
		inFlightAt                 []float64
		firstErr                   error
	)

	runCtx, cancel := context.WithDeadline(ctx, hardEnd)
	defer cancel()

	// A bounded worker pool issues the starts. Bounded, because an unbounded
	// one hides generator saturation as memory growth instead of reporting it;
	// and an arrival that finds no free worker is DROPPED and counted, not
	// queued, because queueing here would make this a closed loop again.
	workers := cfg.arrivalWorkers
	if workers <= 0 {
		// Little's Law, used forwards for once: at `rate` arrivals a second
		// with a service time of about the floor, this many are in flight.
		// Four times that leaves room for the latency the run is looking for.
		est := rate * (cfg.serviceFloor.Seconds() + 1)
		workers = int(math.Ceil(est * 4))
		if workers < 8 {
			workers = 8
		}
		if workers > 4096 {
			workers = 4096
		}
	}
	slots := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		slots <- struct{}{}
	}

	// In-flight sampler. Sampled rather than derived so queue growth is
	// measured, not inferred from the counters.
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				now := time.Now()
				if now.Before(windowStart) || now.After(windowEnd) {
					continue
				}
				mu.Lock()
				inFlightSeries = append(inFlightSeries, inFlight.Load())
				inFlightAt = append(inFlightAt, now.Sub(windowStart).Seconds())
				mu.Unlock()
			}
		}
	}()

	var wg sync.WaitGroup
	for k := int64(0); ; k++ {
		// Arrival k is due at a time computed from the start, not by sleeping
		// one gap at a time. That is what stops the schedule drifting: a slow
		// start request delays that one arrival, not every arrival after it.
		due := start.Add(time.Duration(float64(k) * float64(time.Second) / rate))
		if !due.Before(windowEnd) {
			break
		}
		if wait := time.Until(due); wait > 0 {
			select {
			case <-runCtx.Done():
				break
			case <-time.After(wait):
			}
		}
		if runCtx.Err() != nil {
			break
		}

		inWindow := !due.Before(windowStart)
		if inWindow {
			scheduled.Add(1)
		}

		select {
		case <-slots:
		default:
			// No free worker at the due time. Report it rather than wait.
			if inWindow {
				dropped.Add(1)
			}
			continue
		}

		id, wantShard, err := gen.Next()
		if err != nil {
			slots <- struct{}{}
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			break
		}

		wg.Add(1)
		go func(id string, wantShard core.ShardID, due time.Time, inWindow bool) {
			defer wg.Done()
			defer func() { slots <- struct{}{} }()

			actual := time.Now()
			if inWindow {
				issued.Add(1)
				mu.Lock()
				lags = append(lags, actual.Sub(due))
				mu.Unlock()
			}
			inFlight.Add(1)
			defer inFlight.Add(-1)

			out := driveOneOpen(runCtx, c, id, cfg, wantShard, &accepted, &rejected)
			if !inWindow {
				return
			}
			switch {
			case out.err != nil:
				if errors.Is(out.err, context.DeadlineExceeded) ||
					errors.Is(out.err, context.Canceled) {
					// Still running when the clock ran out. This is the
					// counter that matters on an overloaded engine, and a
					// closed-loop generator throws it away.
					timedOut.Add(1)
					return
				}
				errored.Add(1)
				mu.Lock()
				if firstErr == nil {
					firstErr = out.err
				}
				mu.Unlock()
			case !out.completed():
				terminalOther.Add(1)
			default:
				completed.Add(1)
				if !out.finishedAt.After(windowEnd) {
					completedInWindow.Add(1)
				}
				mu.Lock()
				samples = append(samples, out.latency)
				mu.Unlock()
			}
		}(id, wantShard, due, inWindow)
	}

	wg.Wait()
	cancel()
	samplerWG.Wait()

	actualWindow := window
	if elapsed := time.Since(windowStart); elapsed < window {
		actualWindow = elapsed
	}
	if actualWindow <= 0 {
		actualWindow = window
	}

	res := arrivalResult{
		Targets:           gen.Targets(),
		Window:            actualWindow,
		Warmup:            cfg.warmup,
		Rate:              rate,
		ServiceFloor:      cfg.serviceFloor,
		Scheduled:         scheduled.Load(),
		Issued:            issued.Load(),
		Dropped:           dropped.Load(),
		Accepted:          accepted.Load(),
		Rejected:          rejected.Load(),
		Completed:         completed.Load(),
		CompletedInWindow: completedInWindow.Load(),
		TerminalOther:     terminalOther.Load(),
		TimedOut:          timedOut.Load(),
		Errored:           errored.Load(),
		Latency:           Summarise(samples),
		Stats:             c.Stats(),
		FirstErr:          firstErr,
	}
	res.LagP50, res.LagP99, res.LagMax = lagPercentiles(lags)
	res.InFlightP50, res.InFlightMax, res.InFlightSlope = inFlightStats(inFlightSeries, inFlightAt)
	return res
}

// driveOneOpen is driveOne with the accept/reject distinction kept, which the
// closed loop folds into one "failed" counter. An engine that refuses work is
// behaving very differently from one that is slow, and at saturation the
// difference is the whole story.
func driveOneOpen(ctx context.Context, c *Client, id string, cfg config,
	wantShard core.ShardID, accepted, rejected *atomic.Int64) runOutcome {

	out := driveOne(ctx, c, id, cfg, wantShard)
	switch {
	case out.err == nil:
		accepted.Add(1)
	case errors.Is(out.err, context.DeadlineExceeded), errors.Is(out.err, context.Canceled):
		// The start was accepted; the run simply did not finish in time.
		accepted.Add(1)
	default:
		rejected.Add(1)
	}
	return out
}

func lagPercentiles(lags []time.Duration) (p50, p99, max time.Duration) {
	if len(lags) == 0 {
		return 0, 0, 0
	}
	sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
	at := func(q float64) time.Duration {
		i := int(q * float64(len(lags)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(lags) {
			i = len(lags) - 1
		}
		return lags[i]
	}
	return at(0.50), at(0.99), lags[len(lags)-1]
}

// inFlightStats returns the median, the maximum, and the least-squares slope
// of in-flight work against time.
//
// The slope is what separates "at capacity" from "past it". At capacity
// in-flight work is noisy but level; past capacity it climbs for as long as
// the run lasts, because arrivals do not slow down to match.
func inFlightStats(series []int64, at []float64) (p50, max int64, slope float64) {
	if len(series) == 0 {
		return 0, 0, 0
	}
	sorted := append([]int64(nil), series...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 = sorted[len(sorted)/2]
	max = sorted[len(sorted)-1]

	if len(series) < 3 || len(at) != len(series) {
		return p50, max, 0
	}
	var sx, sy, sxx, sxy float64
	n := float64(len(series))
	for i := range series {
		x, y := at[i], float64(series[i])
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return p50, max, 0
	}
	return p50, max, (n*sxy - sx*sy) / den
}

// ---------------------------------------------------------------------------
// Reporting.
// ---------------------------------------------------------------------------

func arrivalRows(cfg config, res arrivalResult) []Row {
	verdict, why := res.classify()
	shardNote := fmt.Sprintf("shards %s owned by %d node(s): %v",
		shardsString(res.Targets), len(res.Owners), res.Owners)
	suffix := fmt.Sprintf("@%dshard@%.1frps", len(res.Targets), res.Rate)

	// The capacity figure carries its own verdict, in the same row, so the
	// number cannot be read without it. That is the whole point of the mode:
	// the published 4-shard points were read without one.
	capacityVerdict := VerdictNoTarget
	if verdict != VerdictSaturated {
		capacityVerdict = VerdictNoEvidence
	}

	return []Row{
		{
			Measurement: "arrival", NFR: "NFR-5",
			Metric: "saturation_verdict " + suffix,
			Value:  string(verdict), Unit: "",
			Verdict: capacityVerdict, Kind: KindDerived,
			Note: why,
		},
		{
			Measurement: "arrival", NFR: "NFR-5",
			Metric: "completed_per_sec " + suffix,
			Value:  fmt.Sprintf("%.3f", res.CompletedPerSecond()), Unit: "wf/s",
			Target:  "no absolute target; NFR-5 is a ratio",
			Verdict: capacityVerdict, Kind: KindMeasured,
			Note: fmt.Sprintf("%s; verdict %s — %s", shardNote, verdict, why),
		},
		{
			Measurement: "arrival", NFR: "-",
			Metric: "offered_per_sec " + suffix,
			Value:  fmt.Sprintf("%.3f", res.OfferedPerSecond()), Unit: "wf/s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("asked for %.3f/s; %d scheduled, %d issued, %d dropped "+
				"(an arrival with no free generator worker is dropped, never queued)",
				res.Rate, res.Scheduled, res.Issued, res.Dropped),
		},
		{
			Measurement: "arrival", NFR: "-",
			Metric: "accounting " + suffix,
			Value: fmt.Sprintf("%d/%d/%d/%d/%d/%d/%d",
				res.Scheduled, res.Issued, res.Accepted, res.Completed,
				res.Rejected, res.TimedOut, res.Errored),
			Unit:    "scheduled/issued/accepted/completed/rejected/timedout/errored",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("%d reached a terminal status other than COMPLETED; "+
				"%d of the %d completions finished inside the window and only those "+
				"count as throughput, the rest drained during the %s grace period",
				res.TerminalOther, res.CompletedInWindow, res.Completed, fmtDur(cfg.grace)),
		},
		{
			Measurement: "arrival", NFR: "-",
			Metric: "generator_lag_p99 " + suffix,
			Value:  durSeconds(res.LagP99), Unit: "s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("p50 %s, max %s; the gap between an arrival's due time and "+
				"its actual start. A generator that cannot keep its schedule is measuring itself",
				fmtDur(res.LagP50), fmtDur(res.LagMax)),
		},
		{
			Measurement: "arrival", NFR: "-",
			Metric: "in_flight_slope " + suffix,
			Value:  fmt.Sprintf("%+.3f", res.InFlightSlope), Unit: "wf/s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("in-flight p50 %d, max %d, sampled every 100ms. A positive "+
				"slope means the queue was still growing when the window closed",
				res.InFlightP50, res.InFlightMax),
		},
		{
			Measurement: "arrival", NFR: "NFR-5",
			Metric: "latency_p50 " + suffix,
			Value:  durSeconds(res.Latency.P50), Unit: "s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("%s; declared service floor %s; %s",
				percentileConfidence(res.Latency.N), fmtDur(res.ServiceFloor),
				"a p50 on the floor means the engine was idling, not saturated"),
		},
		{
			Measurement: "arrival", NFR: "NFR-5",
			Metric: "latency_p95 " + suffix,
			Value:  durSeconds(res.Latency.P95), Unit: "s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
		},
		{
			Measurement: "arrival", NFR: "NFR-5",
			Metric: "latency_p99 " + suffix,
			Value:  durSeconds(res.Latency.P99), Unit: "s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: percentileConfidence(res.Latency.N),
		},
	}
}

// runArrivalMode sweeps the arrival rate and reports each point with its own
// verdict.
func runArrivalMode(ctx context.Context, c *Client, cfg config, out io.Writer) ([]Row, error) {
	if cfg.serviceFloor <= 0 {
		// Refused rather than guessed. The floor is a property of the
		// workload — for DeliveryOrder with -prep 50ms it is about 2.01s:
		// 560ms of activity sleeps, the prep timer, the timer scanner's 1s
		// tick and up to one status poll. Inferring it from the run would
		// make the gate circular, and declaring it afterwards is how a
		// generator-bound point gets published as capacity.
		return nil, errors.New("-service-floor is required in -mode arrival: " +
			"declare the workload's own latency floor BEFORE the run, or the " +
			"saturation gate cannot tell a saturated engine from an idle one " +
			"(for DeliveryOrder with -prep 50ms it is about 2.01s)")
	}
	if len(cfg.rates) == 0 {
		return nil, errors.New("-rates is required in -mode arrival (e.g. -rates 5,10,20,40)")
	}

	owners, err := ownersOf(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading /v1/shards: %w", err)
	}
	targets, err := selectTargets(cfg.shards, cfg.shardIDs, cfg.shardCount, owners, cfg.spread)
	if err != nil {
		return nil, err
	}
	distinct := distinctOwners(targets, owners)
	window := cfg.duration - cfg.warmup

	var rows []Row
	for _, rate := range cfg.rates {
		if ctx.Err() != nil {
			break
		}
		gen, gerr := NewIDGen(
			fmt.Sprintf("%s-r%s", cfg.idPrefix, fmtRate(rate)), cfg.shardCount, targets)
		if gerr != nil {
			return rows, gerr
		}

		fmt.Fprintf(out, "\n--- arrival: %.2f wf/s offered to %d shard(s) [%s] owned by %v ---\n",
			rate, len(targets), shardsString(targets), distinct)
		fmt.Fprintf(out, "    window %s (after %s warm-up), grace %s, service floor %s\n",
			window, cfg.warmup, cfg.grace, cfg.serviceFloor)

		res := driveArrivals(ctx, c, gen, cfg, rate, window)
		res.Owners = distinct
		res.Targets = targets

		verdict, why := res.classify()
		fmt.Fprintf(out, "    scheduled %d, issued %d, dropped %d, accepted %d, "+
			"completed %d, rejected %d, timed out %d, errored %d\n",
			res.Scheduled, res.Issued, res.Dropped, res.Accepted,
			res.Completed, res.Rejected, res.TimedOut, res.Errored)
		fmt.Fprintf(out, "    completed %.2f wf/s (offered %.2f), p50 %s, "+
			"in-flight p50 %d max %d slope %+.2f/s, lag p99 %s\n",
			res.CompletedPerSecond(), res.OfferedPerSecond(), fmtDur(res.Latency.P50),
			res.InFlightP50, res.InFlightMax, res.InFlightSlope, fmtDur(res.LagP99))
		fmt.Fprintf(out, "    VERDICT %s: %s\n", verdict, why)
		if res.FirstErr != nil {
			fmt.Fprintf(out, "    first error: %v\n", res.FirstErr)
		}

		rows = append(rows, arrivalRows(cfg, res)...)
	}
	return rows, nil
}

func fmtRate(r float64) string {
	if r == math.Trunc(r) {
		return fmt.Sprintf("%d", int64(r))
	}
	return fmt.Sprintf("%.2f", r)
}

// belowFloorNote flags a p50 clearly BELOW the declared floor. The floor is
// declared, not measured, and on different hardware the real one moves (it is
// dominated by durable-timer lateness, not by the activity sleeps). A p50 well
// under it means the declaration is too high for this deployment -- and a
// too-high floor would let an idle engine on slower hardware be classified
// SATURATED. Recalibrate the floor at a trivially low rate before trusting
// any verdict on this deployment.
func belowFloorNote(r arrivalResult) string {
	if r.ServiceFloor <= 0 || r.Latency.P50 <= 0 || r.Latency.P50 >= r.ServiceFloor*9/10 {
		return ""
	}
	return fmt.Sprintf("; NOTE p50 is %.0f%% BELOW the declared floor, so the declared floor "+
		"does not describe this deployment: recalibrate it at a low rate before trusting verdicts",
		100*(1-r.Latency.P50.Seconds()/r.ServiceFloor.Seconds()))
}
