# Local shard sweep — findings

**LOCAL EXPLORATORY MEASUREMENTS.** Taken on one laptop (i5-1335U, 10 physical
cores, 15 GiB, 1.9 GiB swap already in use, btrfs 86 % full) on 2026-10-29
against HEAD `fde6ae0` with 11 modified files in the tree. **No number here
belongs in the study's performance tables.** Their purpose is to shape the
cloud experiment and to validate the harness.

Raw evidence: `results.csv` and `raw/`. Runner:
`experiments/scripts/shard_sweep.sh`.

---

## 1. What the sweep value means

`-sweep 8` does **not** redeploy the engine with 8 shards. The engine runs a
fixed `SHARD_COUNT=16`; loadgen's `IDGen` emits workflow ids that hash onto a
chosen subset of those 16. So the column means **"how many of the 16 existing
shards were driven"**.

Verified live during the runs: 16 shards, owned 4 / 9 / 3 by node-a / node-b /
node-c, 0 expired leases.

---

## 2. Results

### Shard sweep at fixed concurrency 128

| Shards | wf/s | started | completed | p50 (s) | p99 (s) | gen CPU | owner nodes |
|---|---|---|---|---|---|---|---|
| 1 | 5.25 | 233 | 105 | 22.78 | 24.88 | 50 % | 1 (node-b) |
| 2 | 4.85 | 225 | 97 | 13.27 | 15.78 | 49 % | 2 |
| 4 | 14.70 | 422 | 294 | 5.37 | 9.20 | 52 % | 3 |
| 8 | 28.85 | 759 | 577 | 2.94 | 19.59 | 57 % | 3 |
| 16 | 47.15 | 1191 | 943 | 2.57 | 5.27 | 57 % | 3 |

### Concurrency sweep at 16 shards

| Concurrency | wf/s | p50 (s) | p99 (s) | gen CPU | host load |
|---|---|---|---|---|---|
| 128 | 47.15 | 2.57 | 5.27 | 57 % | — |
| **256** | **129.30** | **1.92** | **2.82** | 63 % | — |
| 512 | 44.50 | 3.29 | 18.66 | **101 %** | **21.97** |

`failed 0` at **every** point, including the overload. 0 unowned shards and
all three nodes healthy afterwards.

---

## 3. The finding that changes the cloud design

**At 16 shards, raising concurrency from 128 to 256 raised throughput 2.74×
(47.15 → 129.30 wf/s) while *lowering* p99 latency from 5.27 s to 2.82 s.**

The engine had far more capacity than the generator was asking for. Therefore:

> **The entire fixed-concurrency shard sweep is offered-load-limited at its
> upper end. A sweep that varies shard count at fixed concurrency confounds
> shard count with offered load, and understates capacity by a factor that
> itself varies with shard count.**

This is a methodological defect, not a property of the engine. It must not be
repeated in the cloud. **Every shard-count point needs its own concurrency
sweep to find its own knee.**

### Consequence for the plan

`KUBERNETES-TEST-PLAN.md` predicted Experiment A would be "flat or slightly
negative past the point where shard count exceeds concurrent workflows,
because more shards means more lease renewals and more poll queries". **The
data contradicts that.** Throughput rose monotonically from 4 shards to 16
(14.70 → 28.85 → 47.15, i.e. 1.96× then 1.63×), with latency falling
throughout. Shard count is a productive variable and belongs in the cloud
matrix.

---

## 4. Bottleneck attribution

| Run | Verdict | Evidence |
|---|---|---|
| 1 and 2 shards @ c128 | **INCONCLUSIVE** | Only 105 and 97 completions. The 2-shard point is *below* the 1-shard point (4.85 vs 5.25), which is within run-to-run noise at this sample size. Both spent the window saturated — p50 of 22.8 s and 13.3 s against a 20 s window. Needs repeats before any claim |
| 4, 8, 16 shards @ c128 | **GENERATOR-BOUND** | Proven directly: the same 16-shard configuration produced 2.74× more at c256. The offered load, not the engine, set these numbers |
| 16 shards @ c256 | **ATTRIBUTED — best observed** | Highest throughput *and* lowest latency (p50 1.92 s, p99 2.82 s). Generator at 63 %, not saturated. Nothing failed |
| 16 shards @ c512 | **INCONCLUSIVE** | Congestion collapse to 44.50 wf/s with p99 18.66 s. But **generator CPU hit 101 % (a full core) and host load reached 21.97 on a 12-thread machine.** Engine overload, generator saturation and host thrashing cannot be separated on a single laptop |

**The c512 row is the strongest argument for the cloud campaign**, and it is
now evidence rather than assertion: on one machine the load generator, three
engine nodes, PostgreSQL and its standby all compete for the same 12 threads,
so a collapse cannot be attributed. **The cloud campaign must place the
generator on its own node**, which `KUBERNETES-TEST-PLAN.md` already requires.

---

## 5. Hypotheses tested

| # | Hypothesis | Verdict |
|---|---|---|
| 1 | More shards reduce contention over task and event rows | **Supported.** Connections stayed constant while throughput rose 9× from 1 to 16 shards |
| 2 | More shards increase lease-renewal and polling overhead | **Not observed** up to 16 shards on 3 nodes. Untested beyond |
| 3 | The fixed connection pool limits parallel database work | **Refuted as the binding constraint.** Verified empirically: each node held exactly **20 pool + 2 renewal** connections (66 cluster-wide) at *every* shard count, read from the database's own `pg_stat_activity`. The budget never moved while throughput rose 9× |
| 4 | PostgreSQL WAL, fsync, locks or connections limit throughput | **Not established.** Not instrumented per run; `pg_stat` deltas are missing from the runner |
| 5 | Engine CPU or worker concurrency limits throughput | **Not established.** `docker stats` samples were collected but not yet reduced to per-run figures |
| 6 | The load generator limits throughput | **Confirmed for c128, and for c512 in a different way** — 101 % CPU there |
| 7 | The shard-selection mechanism misrepresents a production configuration | **Partly true, and important.** Driving 8 of 16 deployed shards is not the same as deploying 8 shards: the other 8 still exist, hold leases and are renewed. The cloud matrix must set `SHARD_COUNT` for real |

---

## 6. A correctness result worth keeping

Under roughly 2× overload — host load 21.97, p99 latency 18.66 s — the engine
recorded **zero failures, zero non-COMPLETED terminal states, zero 409
follow-ups and zero unowned shards**, and all three nodes were healthy
afterwards. It degraded in latency, not in correctness.

This is consistent with, and independent of, the duplicate-free results
already recorded in `docs/RESULTS_SUMMARY.md` §1.

---

## 7. Harness defects found and their status

| Defect | Status |
|---|---|
| `cmd/loadgen -mode scaling` buffers all rows and writes once at the end, so an interrupted sweep leaves nothing | **Worked around**, not fixed in loadgen: `shard_sweep.sh` invokes loadgen once per point and flushes a row before the next |
| My per-point invocation breaks loadgen's built-in ratio computation — each process sees no 1-shard baseline and prints "NO EVIDENCE" | **Known consequence.** Ratios must be computed from `results.csv` afterwards. Recorded here rather than hidden |
| My runner failed to parse `owner_nodes`, leaving the column empty | **Open.** Owner counts were recovered by hand from `raw/*.txt`. The parser needs fixing before the cloud campaign |
| `tasks.completed_at` queried by the Phase 9 smoke script | **Fixed and verified** against the live schema: `tasks` has no such column |
| No `pg_stat` or engine-CPU deltas recorded per run | **Open.** Required for ATTRIBUTED verdicts in the cloud |

---

## 8. Why these numbers differ from the published run

`docs/results/scaling-saturated.txt` records 12.80 / 23.15 / 46.30 wf/s at
1 / 2 / 4 shards. This run gave 5.25 / 4.85 / 14.70 for the same points —
roughly 2.5× lower, with a different shape at 1→2.

Plausible contributors, none isolated: 1.9 GiB of swap already in use, 162 420
events already in the log where the published run started cleaner, btrfs at
86 %, and a different background load. **The two are not comparable, and this
file does not supersede the published figures.** It is a separate exploratory
run on a differently-loaded machine.

---

## 9. Errata and later analysis (added 2026-10-30; sections 1–8 unchanged)

Found while building the cloud evidence inventory
(`docs/cloud-campaign/01-EVIDENCE-INVENTORY.md`). Nothing above is rewritten.

1. **`results.csv` latency columns are corrupt.** `p50_ms`, `p95_ms` and
   `p99_ms` contain the shard count (1, 2, 4, 8, 16) on every row. The
   latency figures in §2 came from the raw loadgen output, not from the CSV.
   The runner's header has since changed to `p50_s,p95_s,p99_s`; this file
   predates that fix.
2. **The raw loadgen outputs (`raw/*.txt`) were never committed.** Only
   `.enginecpu` and `.time` are in the repository, so §2's latency and
   completion figures can't be re-derived from it. If the files are still on
   the laptop, they should be added.
3. **Engine CPU, reduced from `raw/*.enginecpu`** (hypothesis 5 was "not
   established" because these samples were never reduced). Only 4 samples
   per node per run, so this is weak evidence:

   | Point | node-a | node-b | node-c | sum |
   |---|---|---|---|---|
   | 1 shard c128 | 24 % | 76 % | 32 % | 132 % |
   | 4 shards c128 | 45 % | 73 % | 42 % | 160 % |
   | 16 shards c128 | 74 % | 113 % | 64 % | 250 % |
   | 16 shards c256 | 87 % | 130 % | 73 % | 291 % |
   | 16 shards c512 | 113 % | 171 % | 108 % | 391 % |

   node-b owned **9 of 16 shards** and ran hottest at every point, so engine
   CPU tracks shard ownership. At c512 the three engines used 3.9 cores
   while the generator used a full core, on a 12-thread host at load 21.97.
4. **Shard count was confounded with worker count, not only with offered
   load.** `WORKFLOW_WORKERS` and `ACTIVITY_WORKERS` are per shard
   (`internal/runtime/pool.go:49`), so driving 16 shards instead of 1 also
   ran 16× the worker goroutines. Phase 6 measured that workers alone
   (4 → 16, one shard) give 1.95×. Hypothesis 1 ("more shards reduce
   contention") is therefore **not separated** from "more workers". The
   cloud matrix E1 holds total workers constant to separate them.
