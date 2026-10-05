package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// nfr3Target is the design document's claim about duplicate side effects.
const nfr3Target = "0 per 1000 runs"

// pkCheckCaveat is printed with the primary-key result every time.
//
// The events table declares PRIMARY KEY (shard_id, workflow_id, run_id, seq).
// PostgreSQL will not store a second row with those four values, so the query
// that looks for one cannot return anything but zero on any database that
// applied the migration. It is worth running as a check that the audit is
// pointed at a real schema, and it is worth reporting, but presenting it as
// evidence of idempotency would be presenting a constraint definition as an
// experimental result.
const pkCheckCaveat = "ENFORCED BY THE SCHEMA, NOT EVIDENCE: events has PRIMARY KEY " +
	"(shard_id, workflow_id, run_id, seq), so a second row with the same four values cannot be " +
	"inserted and this count cannot be anything but 0. It confirms the audit reached the right " +
	"schema; it says nothing about duplicate side effects."

// kCheckMeaning is the counterpart caveat for the check that does matter.
const kCheckMeaning = "THE CHECK THAT MATTERS: k is the step's position in the workflow's own program " +
	"order. Nothing in the schema stops two ActivityTaskCompleted events sharing a k, so a duplicate " +
	"here means the engine journaled the same step finishing twice, which is what a repeated side " +
	"effect looks like in the log."

func runDuplicatesMode(ctx context.Context, c *Client, cfg config, out io.Writer) ([]Row, error) {
	prefix := cfg.auditPrefix
	var batch batchResult

	if prefix == "" {
		targets, err := duplicateTargets(ctx, c, cfg)
		if err != nil {
			return nil, err
		}
		prefix = cfg.idPrefix + "-dup"
		gen, err := NewIDGen(prefix, cfg.shardCount, targets)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "\n--- duplicates: running %d workflows over shards %s ---\n",
			cfg.runs, shardsString(targets))
		fmt.Fprintf(out, "    id prefix %q, concurrency %d, grace %s\n", prefix, cfg.concurrency, cfg.grace)
		fmt.Fprintf(out, "    Kill and restart nodes while this runs if you want the audit to mean\n")
		fmt.Fprintf(out, "    anything: on a cluster nobody disturbs, a duplicate has no way to occur.\n\n")

		batch = runBatch(ctx, c, gen, cfg, cfg.runs)
		fmt.Fprintf(out, "    started %d, completed %d, failed %d, unfinished %d in %s\n",
			batch.Started, batch.Completed, batch.Failed, batch.Unfinished, fmtDur(batch.Elapsed))
		if batch.FirstErr != nil {
			fmt.Fprintf(out, "    first error: %v\n", batch.FirstErr)
		}
	} else {
		fmt.Fprintf(out, "\n--- duplicates: auditing existing workflows with prefix %q ---\n", prefix)
		fmt.Fprintf(out, "    no new load was generated in this run\n")
	}

	reader, err := NewPGReader(ctx, cfg.dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}
	defer reader.Close()

	audit, err := Audit(ctx, reader, prefix)
	if err != nil {
		return nil, fmt.Errorf("auditing the event log: %w", err)
	}
	printAudit(out, audit)
	return duplicateRows(cfg, prefix, batch, audit), nil
}

// duplicateTargets spreads the batch over every shard unless told otherwise.
//
// Breadth is the point here. A duplicated side effect is most likely where a
// step is retried after an ownership change, so confining the batch to one
// shard would be testing the quietest corner of the cluster.
func duplicateTargets(ctx context.Context, c *Client, cfg config) ([]core.ShardID, error) {
	if cfg.shardIDs != "" {
		return selectTargets(0, cfg.shardIDs, cfg.shardCount, nil, false)
	}
	out := make([]core.ShardID, 0, cfg.shardCount)
	for i := int32(0); i < cfg.shardCount; i++ {
		out = append(out, core.ShardID(i))
	}
	_ = ctx
	return out, nil
}

// ---------------------------------------------------------------------------
// The batch.
// ---------------------------------------------------------------------------

type batchResult struct {
	Started    int64
	Completed  int64
	Failed     int64
	Unfinished int64
	Elapsed    time.Duration
	Latency    Summary
	FirstErr   error
}

// runBatch starts exactly n workflows and waits for them, bounded by grace.
func runBatch(ctx context.Context, c *Client, gen *IDGen, cfg config, n int) batchResult {
	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, cfg.grace+cfg.duration)
	defer cancel()

	var (
		issued     atomic.Int64
		started    atomic.Int64
		completed  atomic.Int64
		failed     atomic.Int64
		unfinished atomic.Int64
		mu         sync.Mutex
		samples    []time.Duration
		firstErr   error
	)

	var wg sync.WaitGroup
	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if int(issued.Add(1)) > n {
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
				case out.completed():
					completed.Add(1)
					mu.Lock()
					samples = append(samples, out.latency)
					mu.Unlock()
				case out.err == nil:
					// Terminal but not COMPLETED: a failed or compensated
					// saga. Its history is still audited — a duplicate side
					// effect during a rollback counts just as much.
					failed.Add(1)
				case runCtx.Err() != nil:
					unfinished.Add(1)
				default:
					failed.Add(1)
					mu.Lock()
					if firstErr == nil {
						firstErr = out.err
					}
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	return batchResult{
		Started:    started.Load(),
		Completed:  completed.Load(),
		Failed:     failed.Load(),
		Unfinished: unfinished.Load(),
		Elapsed:    time.Since(start),
		Latency:    Summarise(samples),
		FirstErr:   firstErr,
	}
}

// ---------------------------------------------------------------------------
// The audit.
// ---------------------------------------------------------------------------

// AuditResult is everything the database was asked.
type AuditResult struct {
	Prefix          string
	PKDuplicates    int
	CompletedEvents int
	Unreadable      int
	RunsWithEvents  int
	DuplicateKs     []DuplicateK
	StatusCounts    map[string]int
}

// Audit runs both checks over one batch of workflows.
func Audit(ctx context.Context, r EventReader, prefix string) (AuditResult, error) {
	res := AuditResult{Prefix: prefix}

	pk, err := r.PrimaryKeyDuplicates(ctx, prefix)
	if err != nil {
		return res, fmt.Errorf("primary key check: %w", err)
	}
	res.PKDuplicates = pk

	raw, err := r.ActivityCompletedEvents(ctx, prefix)
	if err != nil {
		return res, fmt.Errorf("reading ActivityTaskCompleted events: %w", err)
	}
	events, unreadable := ParseCompleted(raw)
	res.CompletedEvents = len(events)
	res.Unreadable = len(unreadable)
	res.RunsWithEvents = countRuns(events)
	res.DuplicateKs = FindDuplicateK(events)

	if res.StatusCounts, err = r.StatusCounts(ctx, prefix); err != nil {
		return res, fmt.Errorf("reading execution statuses: %w", err)
	}
	return res, nil
}

func printAudit(out io.Writer, a AuditResult) {
	fmt.Fprintf(out, "\n    audited %d ActivityTaskCompleted events across %d runs (prefix %q)\n",
		a.CompletedEvents, a.RunsWithEvents, a.Prefix)
	if len(a.StatusCounts) > 0 {
		fmt.Fprintf(out, "    execution statuses: %s\n", formatCounts(a.StatusCounts))
	}
	if a.Unreadable > 0 {
		fmt.Fprintf(out, "    WARNING: %d payloads could not be decoded and were not checked\n", a.Unreadable)
	}
	if len(a.DuplicateKs) == 0 {
		fmt.Fprintf(out, "    no duplicate (shard, workflow, run, k) found\n")
		return
	}
	fmt.Fprintf(out, "    %d DUPLICATE STEP(S) FOUND:\n", len(a.DuplicateKs))
	for i, d := range a.DuplicateKs {
		if i >= 20 {
			fmt.Fprintf(out, "      ... and %d more\n", len(a.DuplicateKs)-20)
			break
		}
		fmt.Fprintf(out, "      shard %d workflow %s run %s k=%d completed %d times at seqs %v\n",
			d.ShardID, d.WorkflowID, d.RunID, d.K, d.Count, d.Seqs)
	}
}

func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func duplicateRows(cfg config, prefix string, batch batchResult, a AuditResult) []Row {
	rows := []Row{
		{
			Measurement: "duplicates", NFR: "NFR-3",
			Metric: "duplicate_k_events",
			Value:  fmt.Sprintf("%d", len(a.DuplicateKs)), Unit: "steps",
			Target:  nfr3Target,
			Verdict: verdictZero(len(a.DuplicateKs)),
			Kind:    KindMeasured,
			Note:    kCheckMeaning,
		},
		{
			Measurement: "duplicates", NFR: "NFR-3",
			Metric: "runs_audited",
			Value:  fmt.Sprintf("%d", a.RunsWithEvents), Unit: "runs",
			Target:  "1000 for the stated claim",
			Verdict: runsVerdict(a.RunsWithEvents),
			Kind:    KindMeasured,
			Note: fmt.Sprintf("distinct (shard, workflow, run) triples with at least one completed activity, "+
				"prefix %q; %d completed activity events in total", prefix, a.CompletedEvents),
		},
		{
			Measurement: "duplicates", NFR: "NFR-3",
			Metric: "primary_key_duplicates",
			Value:  fmt.Sprintf("%d", a.PKDuplicates), Unit: "tuples",
			Target:  "0",
			Verdict: VerdictNoEvidence,
			Kind:    KindNotMeasured,
			Note:    pkCheckCaveat,
		},
		{
			Measurement: "duplicates", NFR: "NFR-3",
			Metric: "undecodable_payloads",
			Value:  fmt.Sprintf("%d", a.Unreadable), Unit: "events",
			Target: "0", Verdict: verdictZero(a.Unreadable), Kind: KindMeasured,
			Note: "events the audit could not parse, and therefore could not check; a non-zero value " +
				"means the duplicate count above covers fewer events than it claims",
		},
	}

	if a.StatusCounts != nil {
		rows = append(rows, Row{
			Measurement: "duplicates", NFR: "-",
			Metric: "execution_statuses",
			Value:  formatCounts(a.StatusCounts), Unit: "",
			Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: "a batch that mostly failed audits very little, whatever the duplicate count says",
		})
	}

	if cfg.auditPrefix == "" {
		rows = append(rows, Row{
			Measurement: "duplicates", NFR: "-",
			Metric: "batch_completed",
			Value:  fmt.Sprintf("%d", batch.Completed), Unit: "workflows",
			Target: fmt.Sprintf("%d requested", cfg.runs), Verdict: VerdictNoTarget, Kind: KindMeasured,
			Note: fmt.Sprintf("%d started, %d failed, %d unfinished when the grace period ended, in %s",
				batch.Started, batch.Failed, batch.Unfinished, fmtDur(batch.Elapsed)),
		})
	} else {
		rows = append(rows, Row{
			Measurement: "duplicates", NFR: "-",
			Metric: "batch_completed",
			Target: "n/a", Verdict: VerdictNoTarget, Kind: KindNotMeasured,
			Note: "-audit-prefix was given, so this run generated no load and audited workflows " +
				"some earlier run produced",
		})
	}

	if len(a.DuplicateKs) > 0 {
		sample := a.DuplicateKs[0]
		rows = append(rows, Row{
			Measurement: "duplicates", NFR: "NFR-3",
			Metric:  "duplicate_k_example",
			Value:   fmt.Sprintf("shard %d %s k=%d", sample.ShardID, sample.WorkflowID, sample.K),
			Verdict: VerdictNotMet, Kind: KindMeasured,
			Note: fmt.Sprintf("run %s, completed %d times at seqs %v", sample.RunID, sample.Count, sample.Seqs),
		})
	}
	return rows
}

func verdictZero(n int) string {
	if n == 0 {
		return VerdictMet
	}
	return VerdictNotMet
}

// runsVerdict judges the denominator, not the result.
//
// NFR-3 is stated per 1000 runs. An audit over 40 runs that finds nothing has
// not met that claim; it has failed to test it, and the two should not look
// the same in a table.
func runsVerdict(n int) string {
	if n >= 1000 {
		return VerdictMet
	}
	return VerdictNotMet
}
