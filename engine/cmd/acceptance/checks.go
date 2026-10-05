package main

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dharaneswar339/durexec/internal/obs"
)

// Checks are the individual measurements behind the matrix, reported as rows
// of their own. The FR and NFR rows say what was concluded; these rows say
// what was measured, so an examiner can trace any PASS back to the query or
// request that produced it.
var Checks = []Requirement{
	{
		ID: "C-1", Kind: "CHECK",
		Short:      "Unique (shard,wf,run,seq) [PK: weak]",
		Text:       "No duplicated (shard_id, workflow_id, run_id, seq) row in the events table.",
		Acceptance: "The GROUP BY over the whole table returns nothing. PRIMARY-KEY-ENFORCED, THEREFORE WEAK EVIDENCE: PostgreSQL refuses such a row, so this shows the schema is in place, not that the engine avoided duplicates.",
	},
	{
		ID: "C-2", Kind: "CHECK",
		Short:      "No step completed twice at one k [REAL]",
		Text:       "No workflow run has two ActivityTaskCompleted (or CompensationCompleted) events at the same program-order index k.",
		Acceptance: "Zero (run, k) pairs with more than one completion. THIS IS THE REAL DUPLICATE-SIDE-EFFECT CHECK: two completions at one k sit at different seqs, which the schema allows.",
	},
	{
		ID: "C-3", Kind: "CHECK",
		Short:      "API-started workflow reaches terminal",
		Text:       "A workflow started through POST /v1/workflows reaches a terminal state.",
		Acceptance: "The DeliveryOrder this tool starts reaches COMPLETED, FAILED or COMPENSATING_FAILED within -workflow-budget.",
	},
	{
		ID: "C-4", Kind: "CHECK",
		Short:      "Shards spread over more than one node",
		Text:       "When more than one node is configured, shard ownership is spread across more than one node.",
		Acceptance: "At least two distinct live owners in /v1/shards (or shard_leases).",
	},
	{
		ID: "C-5", Kind: "CHECK",
		Short:      "Fencing tokens never decrease",
		Text:       "Every shard's fencing token is monotonic across repeated observations.",
		Acceptance: "No shard's token is lower than in an earlier sample (samples from the database and every node, across this run only).",
	},
	{
		ID: "C-6", Kind: "CHECK",
		Short:      "Durable timer fired in completed run",
		Text:       "A durable timer fires, as recorded in a completed workflow's history.",
		Acceptance: "A COMPLETED run holds TimerStarted and a matching TimerFired, and no timer fired more than 1 s before its requested duration.",
	},
	{
		ID: "C-7", Kind: "CHECK",
		Short:      "Compensation LIFO, read from journal",
		Text:       "Saga compensations run in reverse order of the steps they undo, as journaled.",
		Acceptance: "In every compensated run, the forward-step index k undone by each successive compensation strictly decreases.",
	},
	{
		ID: "C-8", Kind: "CHECK",
		Short:      "Verdicts carry score, tau, k, model",
		Text:       "Every SemanticDedupEvaluated event carries similarity, tau_high, tau_low, matched_k and model.",
		Acceptance: "Zero journaled verdicts missing any of those fields, over at least one verdict.",
	},
	{
		ID: "C-9", Kind: "CHECK",
		Short:      "Every internal/obs metric on /metrics",
		Text:       "Every metric defined in internal/obs is served on some node's /metrics.",
		Acceptance: "The names are read from internal/obs itself; none is missing from the union of the nodes' /metrics.",
	},
	{
		ID: "C-10", Kind: "CHECK",
		Short:      "No coordination service configured",
		Text:       "The configuration names no external coordination service.",
		Acceptance: "No broker, coordinator or consensus product in the compose file, in go.mod, or in any node's /metrics names.",
	},
}

// AllRows is the full report, in order: FR, NFR, then the supporting checks.
func AllRows() []Requirement {
	return append(append([]Requirement{}, Spec...), Checks...)
}

// vectorMetricNames reports which internal/obs metrics are labelled vectors.
//
// The distinction matters for C-9. A vector with no label values yet is
// registered but has no series, and the Prometheus text format then omits it
// entirely — so "absent from /metrics" means something different for a vector
// (nothing has happened yet) than for a plain histogram (not registered).
func vectorMetricNames() map[string]bool {
	m := obs.New(prometheus.NewRegistry())
	out := map[string]bool{}
	v := reflect.ValueOf(m).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanInterface() {
			continue
		}
		switch c := f.Interface().(type) {
		case *prometheus.CounterVec, *prometheus.GaugeVec, *prometheus.HistogramVec, *prometheus.SummaryVec:
			for _, n := range describeNames(c.(prometheus.Collector)) {
				out[n] = true
			}
		}
	}
	return out
}

// analysis is everything derived from the evidence once, shared by the rows.
type analysis struct {
	eventsOK  bool
	eventsWhy string
	runs      int
	integrity LogIntegrity
	dups      []DupCompletion
	retries   RetryReport
	lifo      LIFOReport
	verdicts  VerdictReport
	keys      DedupKeyReport
	runStatus map[RunKey]string
	handovers HandoverEvidence
	tokenBad  []TokenViolation
	merged    MetricSet
}

const preDB = "the engine database at -dsn"

func preCluster(ev *Evidence) string {
	return "a reachable engine node at -cluster"
}

func analyse(ev *Evidence) analysis {
	a := analysis{runStatus: map[RunKey]string{}, merged: MetricSet{}}
	switch {
	case !ev.StoreRead:
		a.eventsWhy = ev.dbWhy("")
	case ev.EventsErr != "":
		a.eventsWhy = "reading events failed: " + ev.EventsErr
	default:
		a.eventsOK = true
	}
	if a.eventsOK {
		a.runs = len(GroupByRun(ev.Events))
		a.integrity = CheckLogIntegrity(ev.Events)
		a.dups = FindDuplicateCompletions(ev.Events)
		a.retries = AnalyseRetries(ev.Events)
		compMap := ev.Opts.CompMap
		if compMap == nil {
			compMap = defaultCompensationMap
		}
		a.lifo = CheckCompensationLIFO(ev.Events, compMap)
		a.verdicts = AnalyseVerdicts(ev.Events)
		a.keys = AnalyseDedupKeys(ev.Events)
	}
	for _, e := range ev.Execs {
		a.runStatus[RunKey{ShardID: e.ShardID, WorkflowID: e.WorkflowID, RunID: e.RunID}] = e.Status
	}
	if ev.StoreRead && ev.LeasesErr == "" {
		a.handovers = SummariseLeases(ev.Leases)
	}
	a.tokenBad = CheckTokenMonotonic(tokenSeries(ev.TokenSamples))
	for _, ms := range ev.Metrics {
		for name, fam := range ms {
			if _, ok := a.merged[name]; !ok {
				a.merged[name] = fam
			}
		}
	}
	return a
}

// Evaluate decides every row from the evidence. It is a pure function: the
// same evidence always yields the same matrix.
func Evaluate(rec *Recorder, ev *Evidence) {
	a := analyse(ev)
	evalChecks(rec, ev, a)
	evalFR(rec, ev, a)
	evalNFR(rec, ev, a)
}

func truncNote(ev *Evidence) string {
	if ev.EventsTruncated {
		return fmt.Sprintf(" (NOTE: only the first %d events were read; raise -event-limit)", ev.Opts.EventLimit)
	}
	return ""
}

func firstN(list []string, n int) string {
	if len(list) > n {
		return strings.Join(list[:n], "; ") + fmt.Sprintf("; ... %d more", len(list)-n)
	}
	return strings.Join(list, "; ")
}

func dupDetails(d []DupCompletion) []string {
	out := make([]string, 0, len(d))
	for _, x := range d {
		out = append(out, x.Detail)
	}
	return out
}

// ---- supporting checks -----------------------------------------------------

func evalChecks(rec *Recorder, ev *Evidence, a analysis) {
	// C-1: primary-key duplicates. Weak by construction, and labelled so.
	switch {
	case !ev.StoreRead || ev.SeqDupsErr != "":
		rec.Blocked("C-1", preDB, "%s", ev.dbWhy(ev.SeqDupsErr))
	case len(ev.SeqDups) > 0:
		rec.Fail("C-1", "%d duplicated (shard_id, workflow_id, run_id, seq) tuples: %s",
			len(ev.SeqDups), firstN(ev.SeqDups, 3))
	default:
		rec.Pass("C-1", "0 duplicated (shard_id, workflow_id, run_id, seq) tuples in the whole events table. "+
			"PRIMARY-KEY-ENFORCED, THEREFORE WEAK EVIDENCE: this cannot fail while the key exists.")
	}

	// C-2: the real duplicate-side-effect check.
	switch {
	case !a.eventsOK:
		rec.Blocked("C-2", preDB, "%s", a.eventsWhy)
	case len(a.dups) > 0:
		rec.Fail("C-2", "REAL duplicate-side-effect check: %d step(s) completed more than once: %s",
			len(a.dups), firstN(dupDetails(a.dups), 3))
	case a.runs == 0:
		rec.Blocked("C-2", "at least one workflow run in the events table", "the events table is empty")
	case ev.EventsTruncated:
		rec.Blocked("C-2", "the whole events table (raise -event-limit)",
			"no duplicate among the %d events read, but the table has more", len(ev.Events))
	default:
		rec.Pass("C-2", "REAL duplicate-side-effect check: 0 steps completed twice at the same k, across %d runs (%d events).",
			a.runs, len(ev.Events))
	}

	// C-3: a workflow started through the API finishes.
	d := ev.Delivery
	switch {
	case d == nil:
		rec.Blocked("C-3", preCluster(ev), "%s", ev.DriveNote)
	case !d.Started:
		rec.Fail("C-3", "POST /v1/workflows for %s %s did not start it: %s (via %v)", d.Type, d.ID, d.Err, d.Hops)
	case !d.Terminal:
		rec.Fail("C-3", "%s %s was still %s after %s; last error: %s",
			d.Type, d.ID, d.Status, ev.Opts.WorkflowBudget, orNone(d.Err))
	default:
		rec.Pass("C-3", "%s %s (shard %d) started via %v and reached %s after %.1fs of polling.",
			d.Type, d.ID, d.ShardID, d.Hops, d.Status, d.Elapsed.Seconds())
	}

	// C-4: ownership spread.
	owners, shards, src, why := ownership(ev)
	switch {
	case len(ev.Nodes) <= 1:
		rec.Blocked("C-4", "more than one -cluster URL", "only %d node configured, so there is nothing to spread over", len(ev.Nodes))
	case why != "":
		rec.Blocked("C-4", preCluster(ev)+" or "+preDB, "%s", why)
	case len(ev.healthy()) < 2:
		rec.Blocked("C-4", "at least two healthy nodes", "%d of %d configured nodes answered /healthz: %s",
			len(ev.healthy()), len(ev.Nodes), ev.clusterReason())
	case len(owners) >= 2:
		rec.Pass("C-4", "%d shards held by %d live owners (%s), read from %s.", shards, len(owners), censusString(owners), src)
	default:
		rec.Fail("C-4", "%d nodes configured and %d healthy, but live shard owners are only: %s (from %s)",
			len(ev.Nodes), len(ev.healthy()), censusString(owners), src)
	}

	// C-5: fencing tokens never go down.
	sources := map[string]bool{}
	for _, s := range ev.TokenSamples {
		sources[s.Source] = true
	}
	switch {
	case len(a.tokenBad) > 0:
		msgs := make([]string, 0, len(a.tokenBad))
		for _, v := range a.tokenBad {
			msgs = append(msgs, v.String())
		}
		rec.Fail("C-5", "fencing token decreased: %s", firstN(msgs, 3))
	case len(ev.TokenSamples) < 2:
		rec.Blocked("C-5", preCluster(ev)+" or "+preDB, "fewer than two token samples could be taken (%d)", len(ev.TokenSamples))
	default:
		first, last := ev.TokenSamples[0].At, ev.TokenSamples[len(ev.TokenSamples)-1].At
		rec.Pass("C-5", "%d samples of every shard's fencing token over %.1fs (database and each node's /v1/shards); none decreased. Covers this run's window only.",
			len(ev.TokenSamples), last.Sub(first).Seconds())
	}

	// C-6: a durable timer fired in a completed run.
	obsv, from, twhy := completedTimers(ev, a)
	var fired, early []string
	for _, o := range obsv {
		if !o.Fired {
			continue
		}
		line := fmt.Sprintf("%s k=%d asked %.1fs, fired after %.1fs", o.Run, o.K,
			float64(o.Requested)/1e9, float64(o.Elapsed)/1e9)
		if o.Elapsed < o.Requested-1e9 {
			early = append(early, line)
		} else {
			fired = append(fired, line)
		}
	}
	switch {
	case len(early) > 0:
		rec.Fail("C-6", "timer fired before its requested duration: %s", firstN(early, 3))
	case twhy != "" && len(fired) == 0:
		rec.Blocked("C-6", "a completed workflow whose history holds a durable timer", "%s", twhy)
	case len(fired) == 0:
		rec.Blocked("C-6", "a completed workflow whose history holds a durable timer",
			"no completed run with a fired timer was found in %s", from)
	default:
		rec.Pass("C-6", "%d timer(s) fired in completed runs (%s), none early; e.g. %s.", len(fired), from, fired[0])
	}

	// C-7: compensation order.
	l := a.lifo
	switch {
	case !a.eventsOK:
		rec.Blocked("C-7", preDB, "%s", a.eventsWhy)
	case len(l.Violations) > 0:
		rec.Fail("C-7", "%d ordering violation(s): %s", len(l.Violations), firstN(l.Violations, 3))
	case l.RunsChecked > 0:
		rec.Pass("C-7", "%d of %d compensated runs checked, all strictly LIFO; e.g. %s.",
			l.RunsLIFO, l.RunsWithCompensation, l.Example)
	case l.RunsWithCompensation > 0:
		rec.Blocked("C-7", "a -compensation-map entry for "+strings.Join(l.Unmapped, ", "),
			"%d compensated runs found, none resolvable to the steps they undo", l.RunsWithCompensation)
	default:
		rec.Blocked("C-7", "a run that compensated (re-run with -inject on a FAULT_INJECTION cluster)",
			"no compensation appears in %d runs%s%s", a.runs, truncNote(ev), injectNote(ev))
	}

	// C-8: verdict journal completeness.
	vr := a.verdicts
	switch {
	case !a.eventsOK:
		rec.Blocked("C-8", preDB, "%s", a.eventsWhy)
	case len(vr.Incomplete) > 0:
		rec.Fail("C-8", "%d of %d verdicts are missing fields: %s", len(vr.Incomplete), vr.Total, firstN(vr.Incomplete, 3))
	case vr.Total == 0:
		rec.Blocked("C-8", "at least one SemanticDedupEvaluated event (a TravelAgent run books through the semantic check)",
			"no verdict in %d runs%s", a.runs, truncNote(ev))
	default:
		rec.Pass("C-8", "all %d journaled verdicts carry similarity, tau_high, tau_low, matched_k and model; verdicts: %s; models: %s.",
			vr.Total, censusString(vr.ByVerdict), censusString(vr.Models))
	}

	// C-9: metrics named in internal/obs.
	switch {
	case len(ev.Metrics) == 0:
		why := ev.clusterReason()
		if len(ev.MetricsErr) > 0 {
			why = fmt.Sprintf("%v", ev.MetricsErr)
		}
		rec.Blocked("C-9", "a node serving /metrics", "%s", why)
	default:
		missing := MissingMetrics(ev.ExpectedMetrics, a.merged)
		var hard, vec []string
		for _, m := range missing {
			if ev.VectorMetrics[m] {
				vec = append(vec, m)
			} else {
				hard = append(hard, m)
			}
		}
		switch {
		case len(hard) > 0:
			rec.Fail("C-9", "%d of %d internal/obs metrics absent from every node's /metrics: %s",
				len(hard), len(ev.ExpectedMetrics), strings.Join(hard, ", "))
		case len(vec) > 0:
			rec.Manual("C-9", "trigger the event behind each (e.g. a fencing rejection for fencing_rejections_total) and re-run",
				"%d of %d metrics served by %d node(s); missing only labelled vectors that have no series yet, which Prometheus omits until first use: %s",
				len(ev.ExpectedMetrics)-len(vec), len(ev.ExpectedMetrics), len(ev.Metrics), strings.Join(vec, ", "))
		default:
			rec.Pass("C-9", "all %d metrics defined in internal/obs are served (union of %d node(s)): %s.",
				len(ev.ExpectedMetrics), len(ev.Metrics), strings.Join(ev.ExpectedMetrics, ", "))
		}
	}

	// C-10: no coordination service.
	var metricHits []string
	for _, name := range a.merged.Names() {
		for _, h := range coordinationTokens(name) {
			metricHits = append(metricHits, name+" ("+h+")")
		}
	}
	found := append(append(append([]string{}, ev.Compose.Coordination...), ev.GoMod.Coordination...), metricHits...)
	switch {
	case len(found) > 0:
		rec.Fail("C-10", "coordination service named: %s", firstN(found, 5))
	case !ev.Compose.Read:
		rec.Blocked("C-10", "the compose file (-compose)", "could not read %s: %s", ev.Compose.Path, ev.Compose.Err)
	case len(ev.Metrics) == 0:
		// The files alone describe a deployment, not the running one. A
		// PASS needs at least one live node's /metrics read as well.
		rec.Blocked("C-10", "a reachable node's /metrics (the live half of this check)",
			"%s (%d services) and %s (%s) name no coordination product, but no running node was read",
			ev.Compose.Path, len(ev.Compose.Services), ev.GoMod.Path, modSummary(ev.GoMod))
	default:
		rec.Pass("C-10", "none of %d denylisted products (%s) in %s (%d services), %s (%s), or /metrics of %d node(s).",
			len(coordinationServices), strings.Join(coordinationServices, ", "), ev.Compose.Path, len(ev.Compose.Services),
			ev.GoMod.Path, modSummary(ev.GoMod), len(ev.Metrics))
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func injectNote(ev *Evidence) string {
	if ev.InjectErr != "" {
		return "; -inject failed: " + ev.InjectErr
	}
	return ""
}

func modSummary(m ModScan) string {
	if !m.Read {
		return "not read: " + m.Err
	}
	return fmt.Sprintf("%d modules", len(m.Modules))
}

// ownership returns live owners -> shard count, from the API or the database.
func ownership(ev *Evidence) (owners map[string]int, shards int, source, why string) {
	owners = map[string]int{}
	switch {
	case len(ev.ShardViews) > 0:
		for _, v := range ev.ShardViews {
			shards++
			if v.Owner != "" && !v.Expired {
				owners[v.Owner]++
			}
		}
		return owners, shards, "GET /v1/shards on " + ev.ShardViewsFrom, ""
	case ev.StoreRead && ev.LeasesErr == "" && len(ev.Leases) > 0:
		for _, l := range ev.Leases {
			shards++
			if l.Owner != "" && l.ExpiresAt.After(ev.LeasesAt) {
				owners[l.Owner]++
			}
		}
		return owners, shards, "shard_leases", ""
	}
	why = "no shard view: " + orNone(ev.ShardViewsErr)
	if ev.LeasesErr != "" {
		why += "; no lease rows: " + ev.LeasesErr
	}
	return owners, 0, "", why
}

// completedTimers returns timer observations from completed runs, preferring
// the run this tool drove.
func completedTimers(ev *Evidence, a analysis) ([]TimerObservation, string, string) {
	if d := ev.Delivery; d != nil && d.Terminal && d.Status == statusCompleted {
		hist := ev.DeliveryDBHistory
		if len(hist) == 0 {
			hist = ev.DeliveryAPIHistory
		}
		if obs := AnalyseTimers(hist); len(obs) > 0 {
			return obs, "the driven run " + d.ID, ""
		}
	}
	if !a.eventsOK {
		return nil, "", "no driven run with a timer, and " + a.eventsWhy
	}
	var out []TimerObservation
	for _, o := range AnalyseTimers(ev.Events) {
		if a.runStatus[o.Run] == statusCompleted {
			out = append(out, o)
		}
	}
	return out, "the events table", ""
}

// ---- functional requirements -----------------------------------------------

func evalFR(rec *Recorder, ev *Evidence, a analysis) {
	// FR-1
	switch {
	case !a.eventsOK:
		rec.Blocked("FR-1", preDB, "%s", a.eventsWhy)
	case !a.integrity.OK():
		rec.Fail("FR-1", "event log is not append-only and ordered: %s", strings.Join(a.integrity.Violations(3), "; "))
	default:
		rec.Manual("FR-1",
			"kill the owning node mid-workflow (make kill-owner), let the run finish, and check GET /v1/workflows/{id}/history before and after the kill agree event for event; replay determinism is unit-tested in internal/bench/replay_test.go (TestReplayIsDeterministic)",
			"seq runs 1..n with no gap, repeat or backward step in all %d runs (%d events)%s. Necessary, not the byte-identical-replay-after-kill condition; this tool kills nothing.",
			a.integrity.Runs, a.integrity.Events, truncNote(ev))
	}

	// FR-2
	switch {
	case !a.eventsOK:
		rec.Blocked("FR-2", preDB, "%s", a.eventsWhy)
	case len(a.dups) > 0:
		rec.Fail("FR-2", "%d step(s) completed twice: %s", len(a.dups), firstN(dupDetails(a.dups), 3))
	default:
		rec.Manual("FR-2",
			"kill a node mid-workflow (make kill-owner) and re-run this tool; not re-running a completed step on replay is unit-tested in internal/workflow/dispatcher_test.go (TestSideEffectNotReinvokedOnReplay)",
			"C-2: 0 steps completed twice across %d runs; fencing tokens record at least %d ownership changes, but this tool did not kill anything itself.",
			a.runs, a.handovers.Handovers)
	}

	// FR-3
	r := a.retries
	switch {
	case !a.eventsOK:
		rec.Blocked("FR-3", preDB, "%s", a.eventsWhy)
	case len(r.MultipleSuccesses) > 0:
		rec.Fail("FR-3", "a retried step succeeded more than once: %s", firstN(r.MultipleSuccesses, 3))
	case len(r.NonRetryableRetried) > 0:
		rec.Fail("FR-3", "a non-retryable failure was retried: %s", firstN(r.NonRetryableRetried, 3))
	case r.MostFailuresThenOneSuccess >= 3:
		rec.Pass("FR-3", "the log shows a step failing %d times and then succeeding exactly once: %s. %d step(s) failed at least once, %d marked non-retryable, none retried.",
			r.MostFailuresThenOneSuccess, r.Example, r.StepsWithFailures, r.NonRetryable)
	default:
		rec.Blocked("FR-3",
			"a step in history that failed at least 3 times and then succeeded (POST /v1/admin/fail-step with attempts=3 on a FAULT_INJECTION node); backoff itself is unit-tested (TestBackoffCeilingGrowsAndIsBounded)",
			"most failures before one success on any step: %d, across %d runs%s", r.MostFailuresThenOneSuccess, a.runs, truncNote(ev))
	}

	// FR-4
	switch {
	case rec.Get("C-6").Outcome == Fail:
		rec.Fail("FR-4", "%s", rec.Get("C-6").Observed)
	default:
		c6 := rec.Get("C-6")
		rec.Manual("FR-4",
			"start a workflow with a 2-minute sleep, restart the whole cluster during the sleep (make down && make up), and confirm TimerFired is journaled and the run completes",
			"C-6 %s: %s. This tool does not restart the cluster.", c6.Outcome, strings.TrimSuffix(orNone(c6.Observed), "."))
	}

	// FR-5
	evalSignal(rec, ev)

	// FR-6
	evalRouting(rec, ev)

	// FR-7
	moves := "lease table not read (" + ev.dbWhy(ev.LeasesErr) + ")"
	if ev.StoreRead && ev.LeasesErr == "" {
		moves = fmt.Sprintf("fencing tokens show at least %d past ownership changes over %d shards", a.handovers.Handovers, a.handovers.Shards)
	}
	rec.Manual("FR-7",
		"docker kill the owner of a shard (make kill-owner), wait out the 10 s lease, and confirm another node owns the shard and in-flight workflows on it finish without intervention",
		"%s; %d of %d nodes healthy now. This tool cannot kill a node.", moves, len(ev.healthy()), len(ev.Nodes))

	// FR-8
	fencingRejections, hasRej := a.merged.Sum("fencing_rejections_total")
	switch {
	case !a.eventsOK && len(a.tokenBad) == 0:
		rec.Blocked("FR-8", preDB, "%s", a.eventsWhy)
	case len(a.dups) > 0 || len(ev.SeqDups) > 0 || len(a.tokenBad) > 0:
		rec.Fail("FR-8", "C-2 duplicate completions: %d; C-1 duplicate keys: %d; C-5 token decreases: %d",
			len(a.dups), len(ev.SeqDups), len(a.tokenBad))
	default:
		rej := "not exported yet"
		if hasRej {
			rej = fmt.Sprintf("%.0f", fencingRejections)
		}
		rec.Manual("FR-8",
			"induce 20 failovers under load (go run ./cmd/loadgen -mode duplicates, or make kill-owner 20 times) and re-run this tool; stale-token refusal is unit-tested in internal/eventlog/integration_test.go (TestCommitDecisionRejectsStaleToken)",
			"C-2 (REAL check): 0 steps completed twice in %d runs; C-1 (PK, weak): 0 duplicate keys; C-5: no token decrease; fencing tokens show at least %d ownership changes; fencing_rejections_total=%s. Whether those changes were induced failovers under load cannot be told from here.",
			a.runs, a.handovers.Handovers, rej)
	}

	// FR-9
	standby := "standby not probed: " + ev.StandbyErr
	if ev.StandbyErr == "" {
		standby = fmt.Sprintf("standby in recovery=%v, replay lag %.1fs", ev.Standby.InRecovery, ev.Standby.LagSeconds)
	}
	streams := "unknown (" + orNone(ev.ReplErr) + ")"
	if !ev.StoreRead {
		streams = "unknown (" + ev.dbWhy("") + ")"
	} else if ev.ReplErr == "" {
		streams = fmt.Sprintf("%d", ev.ReplStreams)
	}
	rec.Manual("FR-9",
		"stop postgres-leader, promote postgres-standby (see deploy/check_replication.sh), point DATABASE_URL at it, and record recovery time over repeated trials for p50/p99",
		"streaming replicas on the leader: %s; %s. This tool cannot stop the leader.", streams, standby)

	// FR-10
	c7 := rec.Get("C-7")
	switch c7.Outcome {
	case Pass:
		rec.Pass("FR-10", "%s", c7.Observed)
	case Fail:
		rec.Fail("FR-10", "%s", c7.Observed)
	default:
		rec.Blocked("FR-10", c7.Action, "%s", c7.Observed)
	}

	// FR-11
	cf := 0
	for _, e := range ev.Execs {
		if e.Status == statusCompensatingFailed {
			cf++
		}
	}
	cfText := fmt.Sprintf("%d executions are COMPENSATING_FAILED in the database", cf)
	if !ev.StoreRead || ev.ExecsErr != "" {
		cfText = "executions not read: " + ev.dbWhy(ev.ExecsErr)
	}
	rec.Manual("FR-11",
		"open the admin panel and confirm a COMPENSATING_FAILED run is shown (make one by arming fail-step, non_retryable, on RefundPayment); the status itself is unit-tested in internal/workflow/dispatcher_test.go (TestCompensationFailureIsItsOwnStatus)",
		"%s. Visibility in the panel is a human judgement.", cfText)

	// FR-12
	k := a.keys
	switch {
	case !a.eventsOK:
		rec.Blocked("FR-12", preDB, "%s", a.eventsWhy)
	case len(k.Missing) > 0:
		rec.Fail("FR-12", "%d of %d scheduled activities carried no dedup_key: %s", len(k.Missing), k.Scheduled, firstN(k.Missing, 3))
	case k.Scheduled == 0:
		rec.Blocked("FR-12", "at least one scheduled activity in history", "no ActivityTaskScheduled events%s", truncNote(ev))
	default:
		rec.Manual("FR-12",
			"record the keys a stub external API receives across a crash and replay and confirm they are identical; key stability is unit-tested in internal/activity/dedup_test.go (TestDedupKeyIsStable)",
			"all %d scheduled activities carry a dedup_key (e.g. %s). The log holds one copy per step, so identity across replays cannot be seen here.",
			k.Scheduled, k.Example)
	}

	// FR-13
	vr := a.verdicts
	switch {
	case !a.eventsOK:
		rec.Blocked("FR-13", preDB, "%s", a.eventsWhy)
	case len(vr.Executed) > 0:
		rec.Fail("FR-13", "an action judged DUPLICATE ran anyway: %s", firstN(vr.Executed, 3))
	case vr.Blocked > 0:
		rec.Pass("FR-13", "%d action(s) judged DUPLICATE and never scheduled, read from the journal: %s (%d further DUPLICATE verdicts ran because the matched call had no result yet, which is by design).",
			vr.Blocked, vr.Example, vr.NotReusable)
	default:
		rec.Blocked("FR-13", "a DUPLICATE verdict in history (the travel agent's double-booking scenario, app/agent/fixture.go, with semantic dedup on)",
			"%d verdicts journaled, none DUPLICATE and suppressed%s", vr.Total, truncNote(ev))
	}

	// FR-14
	c8 := rec.Get("C-8")
	switch c8.Outcome {
	case Pass:
		rec.Pass("FR-14", "%s", c8.Observed)
	case Fail:
		rec.Fail("FR-14", "%s", c8.Observed)
	default:
		rec.Blocked("FR-14", c8.Action, "%s", c8.Observed)
	}

	// FR-15
	d := ev.Delivery
	switch {
	case d == nil || !d.Started:
		rec.Blocked("FR-15", "a workflow driven by this tool", "%s", orNone(ev.DriveNote))
	case !d.Terminal:
		rec.Blocked("FR-15", "the driven workflow to finish, so its history is stable", "%s is %s", d.ID, d.Status)
	case ev.DeliveryAPIErr != "":
		rec.Fail("FR-15", "GET /v1/workflows/%s/history failed: %s", d.ID, ev.DeliveryAPIErr)
	case ev.DeliveryDBErr != "":
		rec.Blocked("FR-15", preDB, "%s", ev.DeliveryDBErr)
	default:
		same, diff := CompareHistories(ev.DeliveryAPIHistory, ev.DeliveryDBHistory)
		switch {
		case !same:
			rec.Fail("FR-15", "query API history differs from the events table for %s: %s", d.ID, diff)
		case len(ev.DeliveryDBHistory) == 0:
			rec.Fail("FR-15", "query API and events table both return an empty history for %s", d.ID)
		default:
			rec.Pass("FR-15", "GET /v1/workflows/%s/history returned %d events identical to the events table (seq, type, run, created_at, payload).",
				d.ID, len(ev.DeliveryAPIHistory))
		}
	}

	// FR-16
	ag := ev.Agent
	switch {
	case d == nil || ag == nil:
		rec.Blocked("FR-16", preCluster(ev), "%s", orNone(ev.DriveNote))
	case !d.Started || !ag.Started:
		rec.Fail("FR-16", "the running nodes did not accept both applications: %s=%s, %s=%s",
			wfDelivery, orNone(d.Err), wfAgent, orNone(ag.Err))
	case !d.Terminal || !ag.Terminal:
		rec.Blocked("FR-16", "both driven workflows to finish within -workflow-budget",
			"%s is %s, %s is %s", d.ID, d.Status, ag.ID, ag.Status)
	case d.Status == statusCompleted && ag.Status == statusCompleted:
		rec.Pass("FR-16", "%s %s and %s %s both COMPLETED on the same running nodes (%s), with no rebuild in between.",
			wfDelivery, d.ID, wfAgent, ag.ID, nodeIDs(ev))
	default:
		rec.Manual("FR-16", "inspect why the run that did not complete ended as it did (GET /v1/workflows/{id}/history)",
			"both applications ran on the same nodes, but %s ended %s and %s ended %s", d.ID, d.Status, ag.ID, ag.Status)
	}
}

func nodeIDs(ev *Evidence) string {
	var ids []string
	for _, n := range ev.healthy() {
		ids = append(ids, n.NodeID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

// evalSignal decides FR-5 from the driven travel-agent run.
func evalSignal(rec *Recorder, ev *Evidence) {
	ag := ev.Agent
	switch {
	case ag == nil:
		rec.Blocked("FR-5", preCluster(ev), "%s", orNone(ev.DriveNote))
		return
	case !ag.Started:
		rec.Blocked("FR-5", "the node to accept a TravelAgent start", "%s", ag.Err)
		return
	case ev.AgentSignalErr != "":
		rec.Fail("FR-5", "the %s signal to a running workflow was refused: %s", signalApproval, ev.AgentSignalErr)
		return
	case !ag.Terminal:
		rec.Blocked("FR-5", "the TravelAgent run to finish within -workflow-budget", "%s is %s", ag.ID, ag.Status)
		return
	case ag.Status != statusCompleted:
		rec.Manual("FR-5", "inspect GET /v1/workflows/"+ag.ID+"/history to see why the run ended "+ag.Status,
			"signal accepted %.2fs after start; run ended %s", ag.SignalAt.Seconds(), ag.Status)
		return
	case len(ev.AgentHistory) == 0:
		rec.Blocked("FR-5", "the TravelAgent run's history", "%s", orNone(ev.AgentHistoryErr))
		return
	}
	t := SignalBeforeWait(ev.AgentHistory, map[string]bool{"Finalize": true})
	approved, found := CompletedResultField(ev.AgentHistory, "approved")
	switch {
	case !t.Received:
		rec.Fail("FR-5", "POST .../signals was accepted but no SignalReceived is in %s's history: the signal was lost", ag.ID)
	case !found || !approved:
		rec.Fail("FR-5", "SignalReceived at seq %d, but the run completed without approved=true: the buffered signal was not read", t.SignalSeq)
	case !t.BeforeWait:
		rec.Blocked("FR-5", "a signal that lands before the workflow reaches its wait (re-run; this one arrived after)",
			"SignalReceived at seq %d, after the last pre-wait step (seq %d)", t.SignalSeq, t.LastPreWaitSeq)
	default:
		rec.Pass("FR-5", "%s journaled at seq %d, before the last pre-wait step completed at seq %d, so before the wait began; the run later read it and completed approved=true. Buffered, not lost.",
			signalApproval, t.SignalSeq, t.LastPreWaitSeq)
	}
}

// evalRouting decides FR-6: 16 shards over 3 nodes, and routing is correct.
func evalRouting(rec *Recorder, ev *Evidence) {
	owners, shards, src, why := ownership(ev)
	var bad []string
	for _, p := range ev.Routing {
		if !p.OK {
			bad = append(bad, p.Node+": "+p.Detail)
		}
	}
	switch {
	case why != "":
		rec.Blocked("FR-6", preCluster(ev), "%s", why)
	case shards != 16:
		rec.Fail("FR-6", "%d shards in %s; the design fixes 16", shards, src)
	case len(bad) > 0:
		rec.Fail("FR-6", "routing fault for %s: %s", ev.Delivery.ID, firstN(bad, 3))
	case len(ev.healthy()) < 3:
		rec.Blocked("FR-6", "three healthy nodes", "%d healthy: %s", len(ev.healthy()), ev.clusterReason())
	case len(ev.Routing) == 0:
		rec.Blocked("FR-6", "a driven workflow to route", "%s", orNone(ev.DriveNote))
	case len(owners) < 3:
		rec.Manual("FR-6", "re-run once every node has claimed its share of the ring",
			"16 shards, but only %d live owners (%s); routing from all %d nodes correct", len(owners), censusString(owners), len(ev.Routing))
	default:
		rec.Pass("FR-6", "16 shards over %d live owners (%s); asked directly with no retry, each of the %d nodes answered for %s (shard %d) correctly.",
			len(owners), censusString(owners), len(ev.Routing), ev.Delivery.ID, ev.Delivery.ShardID)
	}
}

// ---- non-functional requirements -------------------------------------------

func evalNFR(rec *Recorder, ev *Evidence, a analysis) {
	// NFR-1
	orphans := "orphan query not run: " + ev.dbWhy(ev.OrphansErr)
	if ev.StoreRead && ev.OrphansErr == "" {
		orphans = fmt.Sprintf("%d RUNNING executions have no queued task and no timer (a run waiting on a signal looks the same, so a human must tell them apart)", len(ev.Orphans))
	}
	census := "not read"
	if ev.StoreRead && ev.ExecsErr == "" {
		byStatus, _ := TerminalCensus(ev.Execs)
		census = censusString(byStatus)
	}
	rec.Manual("NFR-1",
		"count completed steps (ActivityTaskCompleted) before and after 20 induced kills (go run ./cmd/loadgen -mode recovery) and confirm none is lost",
		"%s; executions by status: %s.", orphans, census)

	// NFR-2
	h, ok := sumHistograms(ev, "shard_recovery_seconds")
	rec.Manual("NFR-2",
		"measure from the kill, over 20 kills: go run ./cmd/loadgen -mode recovery (make kill-owner between probes)",
		"%s. This histogram starts when a lease has already expired, so it leaves out the up-to-10 s from the kill to expiry; it cannot be compared with the target directly.",
		histText(h, ok, "shard_recovery_seconds"))

	// NFR-3
	switch {
	case !a.eventsOK:
		rec.Blocked("NFR-3", preDB, "%s", a.eventsWhy)
	case len(a.dups) > 0:
		rec.Fail("NFR-3", "C-2: %d step(s) completed twice: %s", len(a.dups), firstN(dupDetails(a.dups), 3))
	default:
		rec.Manual("NFR-3",
			"count real invocations at a stub external API over 1 000 runs (go run ./cmd/loadgen -mode duplicates)",
			"C-2 (REAL check): 0 steps completed twice across %d runs. Necessary but not sufficient: an activity that ran twice and was journaled once is invisible in the log.", a.runs)
	}

	// NFR-4
	ttl := ev.Compose.LeaseTTL
	if ttl == "" {
		ttl = "unknown"
	}
	rec.Manual("NFR-4",
		"kill a shard's owner and time the window in which that shard's requests fail (go run ./cmd/loadgen -mode recovery samples /v1/shards)",
		"configured LEASE_TTL=%s; one shard is 1/16 of workflows. The gap needs a kill, which this tool does not do.", ttl)

	// NFR-5
	completed, hasC := a.merged.Sum("workflow_completed_total")
	cText := "workflow_completed_total not served"
	if hasC {
		cText = fmt.Sprintf("workflow_completed_total=%.0f across %d node(s)", completed, len(ev.Metrics))
	}
	rec.Manual("NFR-5",
		"run go run ./cmd/loadgen -mode scaling at 1, 2 and 4 shards and compare completed workflows per second",
		"%s; throughput needs a controlled load, which this tool does not generate.", cText)

	// NFR-6
	longest, over200 := 0, 0
	if a.eventsOK {
		longest, over200 = RunLengths(ev.Events)
	}
	rh, rok := sumHistograms(ev, "replay_duration_seconds")
	rec.Manual("NFR-6",
		"run go test ./internal/bench -run TestNFR6ReplayBudget -bench BenchmarkReplayLongHistories, which times replay at 200 events",
		"%s; longest run in the table has %d events (%d runs at 200 or more). The histogram mixes all history lengths, so it cannot state the 200-event figure.",
		histText(rh, rok, "replay_duration_seconds"), longest, over200)

	// NFR-7
	evalDedupLatency(rec, ev, a)

	// NFR-8
	rec.Manual("NFR-8",
		"run make experiment (go run ./cmd/experiment -embedder=http) over the hand-labelled retry pairs and read the false-positive rate at tau=0.90",
		"journaled verdicts: %s. A false positive needs a human label, which the log does not hold.", censusString(a.verdicts.ByVerdict))

	// NFR-9
	total, capped := 0, 0
	for _, s := range ev.Compose.Services {
		if s.MemLimitMB > 0 {
			capped++
			total += s.MemLimitMB
		}
	}
	memText := "compose file not read: " + ev.Compose.Err
	if ev.Compose.Read {
		memText = fmt.Sprintf("%s caps %d of %d services at mem_limit totalling %d MB (target 3072 MB)",
			ev.Compose.Path, capped, len(ev.Compose.Services), total)
	}
	rec.Manual("NFR-9",
		"on the host, run docker stats --no-stream and sum MEM USAGE over the stack's containers",
		"%s. Limits are not usage, and this tool may not ask docker what is running.", memText)

	// NFR-10
	evalDependencies(rec, ev)
}

func evalDedupLatency(rec *Recorder, ev *Evidence, a analysis) {
	const target = 0.040
	h, ok := sumHistograms(ev, "dedup_latency_seconds")
	var fake []string
	for m := range a.verdicts.Models {
		if strings.HasPrefix(m, "fake-") {
			fake = append(fake, m)
		}
	}
	switch {
	case len(ev.Metrics) == 0:
		rec.Blocked("NFR-7", "a node serving /metrics", "%s", ev.clusterReason())
	case !ok:
		rec.Blocked("NFR-7", "dedup_latency_seconds on /metrics", "no node serves it (see C-9)")
	case h.Count < 100:
		rec.Blocked("NFR-7", "at least 100 semantic checks recorded, for a meaningful p99", "%s", histText(h, ok, "dedup_latency_seconds"))
	case !a.eventsOK:
		rec.Manual("NFR-7", "confirm the checks used the real all-MiniLM-L6-v2 model (make up-semantic); the database was unavailable to read model names",
			"%s", histText(h, ok, "dedup_latency_seconds"))
	case len(fake) > 0:
		rec.Manual("NFR-7", "re-run on make up-semantic, so the per-check timer measures real all-MiniLM-L6-v2 inference",
			"%s; but the journal shows the stand-in encoder (%s), whose latency is not local model inference", histText(h, ok, "dedup_latency_seconds"), strings.Join(fake, ", "))
	default:
		v, lo, hi, _ := h.Quantile(0.99)
		switch {
		case hi <= target:
			rec.Pass("NFR-7", "p99 of %d checks is in the bucket %.0f-%.0f ms (estimate %.1f ms), wholly under 40 ms.", int(h.Count), lo*1e3, hi*1e3, v*1e3)
		case lo >= target:
			rec.Fail("NFR-7", "p99 of %d checks is in the bucket %.0f-%s ms (estimate %.1f ms), at or over 40 ms.", int(h.Count), lo*1e3, msText(hi), v*1e3)
		default:
			rec.Manual("NFR-7", "time the check at finer resolution (the histogram bucket straddles 40 ms)",
				"p99 of %d checks lies in the bucket %.0f-%s ms (estimate %.1f ms), which contains the 40 ms target.", int(h.Count), lo*1e3, msText(hi), v*1e3)
		}
	}
}

func evalDependencies(rec *Recorder, ev *Evidence) {
	c10 := rec.Get("C-10")
	if c10.Outcome == Fail {
		rec.Fail("NFR-10", "%s", c10.Observed)
		return
	}
	if c10.Outcome != Pass {
		rec.Blocked("NFR-10", c10.Action, "%s", c10.Observed)
		return
	}
	groups := map[string][]string{}
	var unknown []string
	for _, s := range ev.Compose.Services {
		cls := classifyService(s)
		if cls == "unclassified" || !permitted(s) {
			unknown = append(unknown, s.Name)
			continue
		}
		groups[cls] = append(groups[cls], s.Name)
	}
	var urls []string
	for k, v := range ev.Compose.URLEnv {
		if k != "DATABASE_URL" {
			urls = append(urls, k+"="+v)
		}
	}
	sort.Strings(urls)
	desc := fmt.Sprintf("PostgreSQL: %s; engine: %s; observability (scrapes the engine, not called by it): %s",
		strings.Join(groups["postgresql"], ", "), strings.Join(groups["engine"], ", "), orNone(strings.Join(groups["observability"], ", ")))
	switch {
	case len(groups["postgresql"]) == 0:
		rec.Fail("NFR-10", "no PostgreSQL service in %s; %s", ev.Compose.Path, desc)
	case len(unknown) > 0 || len(urls) > 0:
		rec.Manual("NFR-10", "decide whether these are engine dependencies: "+strings.Join(append(unknown, urls...), ", "),
			"%s; C-10 found no coordination service.", desc)
	default:
		rec.Pass("NFR-10", "one external dependency, PostgreSQL. %s; the only *_URL setting in the compose file is DATABASE_URL; C-10 found no broker, coordinator or consensus service in the compose file, go.mod or live /metrics.", desc)
	}
}

func permitted(s ComposeService) bool {
	name := strings.ToLower(s.Name + " " + s.Image)
	for _, p := range permittedServices {
		if strings.Contains(name, p) {
			return true
		}
	}
	return false
}

// sumHistograms adds one histogram across every node that served it.
func sumHistograms(ev *Evidence, name string) (Histogram, bool) {
	out := Histogram{Name: name}
	found := false
	byLE := map[float64]float64{}
	for _, ms := range ev.Metrics {
		h, ok := ms.ReadHistogram(name)
		if !ok {
			continue
		}
		found = true
		out.Count += h.Count
		out.Sum += h.Sum
		for _, b := range h.Buckets {
			byLE[b.LE] += b.Count
		}
	}
	for le, c := range byLE {
		out.Buckets = append(out.Buckets, Bucket{LE: le, Count: c})
	}
	sort.Slice(out.Buckets, func(i, j int) bool { return out.Buckets[i].LE < out.Buckets[j].LE })
	return out, found
}

func histText(h Histogram, ok bool, name string) string {
	if !ok {
		return name + " not served by any node"
	}
	if h.Count == 0 {
		return name + ": served, no observations yet"
	}
	p50, _, hi50, _ := h.Quantile(0.5)
	p99, _, hi99, _ := h.Quantile(0.99)
	return fmt.Sprintf("%s: %d observations, p50 ~%.3fs (bucket to %s s), p99 ~%.3fs (bucket to %s s)",
		name, int(h.Count), p50, secText(hi50), p99, secText(hi99))
}

func secText(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return fmt.Sprintf("%.3f", v)
}

func msText(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return fmt.Sprintf("%.0f", v*1e3)
}
