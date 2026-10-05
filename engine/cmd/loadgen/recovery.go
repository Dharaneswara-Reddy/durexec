package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// nfr2Target is the design document's claim about recovery, quoted once.
const nfr2Target = "p50 < 6s, p99 < 15s"

const (
	nfr2P50Budget = 6 * time.Second
	nfr2P99Budget = 15 * time.Second
)

// Episode is one outage of one shard: the moment it stopped being usable and
// the moment work on it was seen to run again.
type Episode struct {
	Shard       core.ShardID
	DisruptedAt time.Time
	RecoveredAt time.Time
	Reason      string
	OwnerBefore string
	OwnerAfter  string
	TokenBefore core.FencingToken
	TokenAfter  core.FencingToken
}

// Duration is the measured unavailability.
func (e Episode) Duration() time.Duration { return e.RecoveredAt.Sub(e.DisruptedAt) }

// shardState is the last thing the watcher saw about a shard.
type shardState struct {
	owner   string
	token   core.FencingToken
	expired bool
	seen    bool
}

// classify decides whether the transition from old to new is a disruption.
//
// Three different observations mean the same thing — this shard is no longer
// being served by the node that was serving it:
//
//	the lease expired          nobody renewed it, so its owner is gone
//	the owner name changed     somebody else took it
//	the fencing token moved    the lease was re-acquired, possibly by the same
//	                           node after a restart
//
// The token is the one that cannot be faked. It is bumped by the database on
// every acquisition and never goes backwards, so a token that has increased is
// proof that ownership genuinely changed hands, even in the case the other two
// signals miss: a node that dies and comes back under the same name between
// two polls.
//
// It returns the empty string when nothing interesting happened.
func classify(old, cur shardState) string {
	if !old.seen {
		return ""
	}
	switch {
	case !old.expired && cur.expired:
		return "lease expired"
	case cur.token > old.token && old.owner != "" && cur.owner != old.owner:
		return "owner changed " + old.owner + " -> " + cur.owner
	case cur.token > old.token:
		return "fencing token advanced"
	case old.owner != "" && cur.owner != "" && cur.owner != old.owner:
		return "owner changed " + old.owner + " -> " + cur.owner
	default:
		return ""
	}
}

// recoveryWatcher ties the ownership watcher to the probe workers.
type recoveryWatcher struct {
	mu       sync.Mutex
	open     map[core.ShardID]*Episode
	closed   []Episode
	baseline []time.Duration
	out      io.Writer
}

func newRecoveryWatcher(out io.Writer) *recoveryWatcher {
	return &recoveryWatcher{open: map[core.ShardID]*Episode{}, out: out}
}

// disrupt opens an episode for a shard, unless one is already open.
func (w *recoveryWatcher) disrupt(ep Episode) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, exists := w.open[ep.Shard]; exists {
		return
	}
	e := ep
	w.open[ep.Shard] = &e
	fmt.Fprintf(w.out, "[%s] shard %d DISRUPTED (%s), owner was %q token %d\n",
		ep.DisruptedAt.Format("15:04:05.000"), ep.Shard, ep.Reason, ep.OwnerBefore, ep.TokenBefore)
}

// observeOwner records the current owner for an episode that is still open, so
// the report can say who picked the shard up.
func (w *recoveryWatcher) observeOwner(s core.ShardID, owner string, token core.FencingToken) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.open[s]; ok && owner != "" {
		e.OwnerAfter = owner
		e.TokenAfter = token
	}
}

// probeDone reports one completed probe workflow.
//
// A probe closes an episode only if it was STARTED at or after the disruption.
// A probe already halfway through when the node died would otherwise close the
// episode the instant it finished and report a recovery time shorter than the
// outage.
func (w *recoveryWatcher) probeDone(s core.ShardID, startedAt, finishedAt time.Time, latency time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.open[s]
	if !ok {
		w.baseline = append(w.baseline, latency)
		return
	}
	if startedAt.Before(e.DisruptedAt) {
		return
	}
	e.RecoveredAt = finishedAt
	w.closed = append(w.closed, *e)
	delete(w.open, s)
	fmt.Fprintf(w.out, "[%s] shard %d RECOVERED after %s (now owned by %q token %d)\n",
		finishedAt.Format("15:04:05.000"), s, fmtDur(e.Duration()), e.OwnerAfter, e.TokenAfter)
}

func (w *recoveryWatcher) snapshot() ([]Episode, []time.Duration, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	eps := make([]Episode, len(w.closed))
	copy(eps, w.closed)
	base := make([]time.Duration, len(w.baseline))
	copy(base, w.baseline)
	return eps, base, len(w.open)
}

// ---------------------------------------------------------------------------
// The run.
// ---------------------------------------------------------------------------

func runRecoveryMode(ctx context.Context, c *Client, cfg config, out io.Writer) ([]Row, error) {
	watched, err := watchedShards(cfg)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(out, "\n--- recovery: watching shards %s for %s ---\n", shardsString(watched), cfg.duration)
	fmt.Fprintf(out, "    This tool kills nothing. Run your node kill (for example `docker kill node-b`)\n")
	fmt.Fprintf(out, "    from another terminal, as many times as you want samples for.\n")
	fmt.Fprintf(out, "    /v1/shards sampled every %s; each shard probed with a real workflow every %s.\n\n",
		cfg.shardPoll, cfg.probeInterval)

	runCtx, cancel := context.WithTimeout(ctx, cfg.duration)
	defer cancel()

	w := newRecoveryWatcher(out)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		watchOwnership(runCtx, c, w, watched, cfg.shardPoll)
	}()

	for _, s := range watched {
		gen, err := NewIDGen(fmt.Sprintf("%s-probe-s%d", cfg.idPrefix, s), cfg.shardCount, []core.ShardID{s})
		if err != nil {
			cancel()
			wg.Wait()
			return nil, err
		}
		wg.Add(1)
		go func(s core.ShardID, gen *IDGen) {
			defer wg.Done()
			probeShard(runCtx, c, w, cfg, s, gen)
		}(s, gen)
	}
	wg.Wait()

	episodes, baseline, stillOpen := w.snapshot()
	return recoveryRows(cfg, episodes, baseline, stillOpen, c.Stats()), nil
}

func watchedShards(cfg config) ([]core.ShardID, error) {
	if cfg.shardIDs != "" {
		return selectTargets(0, cfg.shardIDs, cfg.shardCount, nil, false)
	}
	out := make([]core.ShardID, 0, cfg.shardCount)
	for i := int32(0); i < cfg.shardCount; i++ {
		out = append(out, core.ShardID(i))
	}
	return out, nil
}

// watchOwnership polls /v1/shards and opens an episode on every handover.
func watchOwnership(ctx context.Context, c *Client, w *recoveryWatcher, watched []core.ShardID, every time.Duration) {
	want := map[core.ShardID]bool{}
	for _, s := range watched {
		want[s] = true
	}
	prev := map[core.ShardID]shardState{}

	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		resp, err := c.Shards(ctx)
		if err == nil {
			for _, sv := range resp.Shards {
				if !want[sv.ShardID] {
					continue
				}
				cur := shardState{owner: sv.Owner, token: sv.Token, expired: sv.Expired, seen: true}
				if reason := classify(prev[sv.ShardID], cur); reason != "" {
					w.disrupt(Episode{
						Shard:       sv.ShardID,
						DisruptedAt: resp.ObservedAt,
						Reason:      reason,
						OwnerBefore: prev[sv.ShardID].owner,
						TokenBefore: prev[sv.ShardID].token,
					})
				}
				if !sv.Expired && sv.Owner != "" {
					w.observeOwner(sv.ShardID, sv.Owner, sv.Token)
				}
				prev[sv.ShardID] = cur
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// probeShard keeps one workflow at a time running on a shard.
//
// "Work resumes" is deliberately defined as a workflow completing end to end,
// not as the API accepting a request. A node can answer 201 the moment it
// takes the lease while its workers are still starting; counting that as
// recovery would measure how fast the lease table changed, which is not what
// the requirement is about. A completed DeliveryOrder means the workflow
// worker, the activity workers, the task queue and the timer scanner are all
// running on the new owner.
func probeShard(ctx context.Context, c *Client, w *recoveryWatcher, cfg config, s core.ShardID, gen *IDGen) {
	for ctx.Err() == nil {
		id, err := gen.NextFor(s)
		if err != nil {
			return
		}
		probeCtx, cancel := context.WithTimeout(ctx, cfg.probeTimeout)
		startedAt := time.Now()
		out := driveOne(probeCtx, c, id, cfg, s)
		cancel()

		if out.completed() {
			w.probeDone(s, startedAt, out.finishedAt, out.latency)
		}
		if !sleepCtx(ctx, cfg.probeInterval) {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Reporting.
// ---------------------------------------------------------------------------

func recoveryRows(cfg config, episodes []Episode, baseline []time.Duration, stillOpen int, stats ClientStats) []Row {
	sort.Slice(episodes, func(i, j int) bool { return episodes[i].DisruptedAt.Before(episodes[j].DisruptedAt) })

	durations := make([]time.Duration, 0, len(episodes))
	for _, e := range episodes {
		durations = append(durations, e.Duration())
	}
	sum := Summarise(durations)
	base := Summarise(baseline)

	// The observer's uncertainty, stated as a number rather than implied. The
	// disruption is only known to within one ownership poll, and the recovery
	// to within one probe cycle plus one workflow poll.
	uncertainty := fmt.Sprintf("timing granularity: +/-%s on the disruption (one /v1/shards poll), "+
		"up to %s+%s on the recovery (one probe gap plus one status poll)",
		cfg.shardPoll, cfg.probeInterval, cfg.poll)

	if len(episodes) == 0 {
		note := "no ownership change was observed during the watch window"
		if stillOpen > 0 {
			note = fmt.Sprintf("%d shard(s) were still down when the window closed and never recovered; "+
				"no completed episode to measure", stillOpen)
		}
		return []Row{
			{
				Measurement: "recovery", NFR: "NFR-2", Metric: "recovery_p50",
				Target: nfr2Target, Verdict: VerdictNoEvidence, Kind: KindNotMeasured, Note: note,
			},
			{
				Measurement: "recovery", NFR: "NFR-2", Metric: "recovery_p99",
				Target: nfr2Target, Verdict: VerdictNoEvidence, Kind: KindNotMeasured,
				Note: "kill a node while this mode is running, or it has nothing to time",
			},
			{
				Measurement: "recovery", NFR: "NFR-2", Metric: "kills_observed",
				Value: "0", Unit: "episodes", Verdict: VerdictNoTarget, Kind: KindMeasured, Note: uncertainty,
			},
		}
	}

	rows := []Row{
		{
			Measurement: "recovery", NFR: "NFR-2", Metric: "kills_observed",
			Value: fmt.Sprintf("%d", len(episodes)), Unit: "episodes",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: percentileConfidence(len(episodes)) + "; " + uncertainty,
		},
		{
			Measurement: "recovery", NFR: "NFR-2", Metric: "recovery_p50",
			Value: durSeconds(sum.P50), Unit: "s",
			Target: "< " + nfr2P50Budget.String(), Verdict: verdictUnder(sum.P50, nfr2P50Budget),
			Kind: KindMeasured,
			Note: "shard stopped being served until a workflow started after that moment completed on it",
		},
		{
			Measurement: "recovery", NFR: "NFR-2", Metric: "recovery_p99",
			Value: durSeconds(sum.P99), Unit: "s",
			Target: "< " + nfr2P99Budget.String(), Verdict: verdictUnder(sum.P99, nfr2P99Budget),
			Kind: KindMeasured, Note: percentileConfidence(len(episodes)),
		},
		{
			Measurement: "recovery", NFR: "NFR-2", Metric: "recovery_min",
			Value: durSeconds(sum.Min), Unit: "s", Verdict: VerdictNoTarget, Kind: KindMeasured,
		},
		{
			Measurement: "recovery", NFR: "NFR-2", Metric: "recovery_max",
			Value: durSeconds(sum.Max), Unit: "s", Verdict: VerdictNoTarget, Kind: KindMeasured,
		},
	}

	if base.N > 0 {
		adjusted := sum.P50 - base.P50
		if adjusted < 0 {
			adjusted = 0
		}
		rows = append(rows,
			Row{
				Measurement: "recovery", NFR: "-", Metric: "probe_baseline_p50",
				Value: durSeconds(base.P50), Unit: "s", Verdict: VerdictNoTarget, Kind: KindMeasured,
				Note: fmt.Sprintf("how long the same probe workflow takes on a healthy shard (%s); "+
					"every recovery figure above includes one of these", percentileConfidence(base.N)),
			},
			Row{
				Measurement: "recovery", NFR: "NFR-2", Metric: "recovery_p50_less_baseline",
				Value: durSeconds(adjusted), Unit: "s",
				Target: "< " + nfr2P50Budget.String(), Verdict: VerdictNoTarget, Kind: KindDerived,
				Note: "recovery_p50 minus probe_baseline_p50, an estimate of the outage with the probe's own " +
					"run time removed; the verdict against NFR-2 is taken from the unadjusted figure, " +
					"which is the conservative one",
			},
		)
	}

	for i, e := range episodes {
		rows = append(rows, Row{
			Measurement: "recovery", NFR: "NFR-2",
			Metric: fmt.Sprintf("episode_%d_shard_%d", i+1, e.Shard),
			Value:  durSeconds(e.Duration()), Unit: "s",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("%s; owner %q(token %d) -> %q(token %d)",
				e.Reason, e.OwnerBefore, e.TokenBefore, e.OwnerAfter, e.TokenAfter),
		})
	}

	if stillOpen > 0 {
		rows = append(rows, Row{
			Measurement: "recovery", NFR: "NFR-2", Metric: "episodes_unfinished",
			Value: fmt.Sprintf("%d", stillOpen), Unit: "shards",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: "still down when the window closed; excluded from the percentiles above, which therefore " +
				"understate recovery time",
		})
	}
	return rows
}

func verdictUnder(got, budget time.Duration) string {
	if got < budget {
		return VerdictMet
	}
	return VerdictNotMet
}
