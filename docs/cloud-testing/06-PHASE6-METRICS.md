# Phase 6 — Production-Relevant Metrics and Performance

**Headline.** Per-node capacity is **12.98 wf/s**, and it scales to **25.78 wf/s on two
nodes (1.99×)**. The limit is **per-shard worker concurrency**, not the database: at every
saturation point the connection pool waited **0.0%** of acquisitions and PostgreSQL used
≤37% of one core. Raising `WORKFLOW_WORKERS` 4→16 nearly doubled single-shard throughput
to **25.26 wf/s** with no code change. **Commit batching is therefore not justified by this
evidence.** Two real defects were found and fixed: the fault injector was never wired into
`cmd/engined`, so both crash seams were dead in the deployed binary (**this was the true
cause of F9-2**), and `workflow_starts_total` mis-counted duplicate starts.

**Not a production-readiness statement.** All figures come from one 2-vCPU / 3.7 GiB
minikube VM on a laptop and are not production capacity estimates.

## 1. Provenance

| Field | Value |
|---|---|
| Commit | `fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac` (unchanged; Phase 6 edits uncommitted) |
| Image under test | `durexec-engine:phase6e` — built from the host toolchain (go1.25.10, `CGO_ENABLED=0`, `-trimpath`, same flags as `engine/Dockerfile`) layered onto the Phase 9 distroless image, because Docker Hub could not resolve `golang:1.25` (TLS timeout) |
| Cluster | minikube kvm2, 2 vCPU / 3.72 GiB allocatable; 2 engine pods, PostgreSQL 16 + pgvector, Prometheus, postgres_exporter |
| Config | `SHARD_COUNT=16`, `WORKFLOW_WORKERS=4`, `ACTIVITY_WORKERS=8` (per shard), `LEASE_TTL=10s`, `SYNC_COMMIT=on` |
| Checks | `gofmt` clean, `go vet` clean, **26/26 test packages pass**, `-race` clean on all 5 changed packages |
| Raw data | `docs/results/phase6-sweep.csv` (32 rows), `phase6-storage.txt`, `phase6-crash-boundaries.txt` |

## 2. Observability: what was added, and how each was validated

| Added | Mechanism | How validated |
|---|---|---|
| PostgreSQL CPU/mem/conns/txn/buffers/WAL/checkpoints | postgres_exporter + 2 custom queries (`pg_stat_wal`, aggregated `pg_stat_statements`) | 323 `pg_*` families live; WAL bytes rise monotonically with offered load (40→452 kB/s) |
| Container CPU + memory | kubelet's own cAdvisor via API-server proxy — no agent deployed | 7 `container_*` families; all 5 pods visible |
| Engine process CPU, RSS, goroutines, GC, FDs | `collectors.NewGoCollector` + `NewProcessCollector` on the engine's private registry | **0 → 83** `go_*`/`process_*` series. They were absent because the engine builds its own registry instead of the default one |
| Connection-pool occupancy and pressure (8 metrics) | new `obs.RegisterPoolStats`, sampled at scrape time from `pgxpool.Stat()` | `db_pool_empty_acquires_total` rose 0.0% → 74.7% exactly as throughput collapsed |
| `workflow_starts_total{workflow_type,outcome}` | new counter, 6 fixed outcome labels | fired 4 request shapes, checked each label (see §7 — found a defect) |

Totals: **641 metric families, 1,772 series.** Labels are bounded: no workflow id, run id or
error string reaches a label. `startOutcome()` is the boundary that enforces this and has a
test asserting every output is in the fixed vocabulary.

**Validation caveat.** `sum(workflow_starts_total)` **double-counts forwarded requests**: 4
requests produced 5 observations because a non-owning node counts `not_owned` and then
forwards. Exclude `not_owned` for client-visible outcomes.

## 3. Throughput versus offered load (open loop, generator-verified)

Single shard, `WORKFLOW_WORKERS=4`. Generator lag p99 ≤1.2 ms in every row.

| Offered | Completed/s | p50 | p99 | In-flight slope | Verdict | Pool wait |
|---|---|---|---|---|---|---|
| 12 | 11.70 | 1.51 s | 2.11 s | −0.05 | not saturated | 0.0% |
| **14** | **12.98** | 3.01 s | 3.91 s | +0.42 | **SATURATED** | 0.0% |
| 16 | 11.28 | 11.51 s | 16.01 s | +3.55 | saturated | 0.5% |
| 20 | 4.00 | 68.9 s | 76.4 s | +11.5 | generator bound | 74.7% |

Two shards (two nodes): 25.04 wf/s at 26 offered (not saturated) → **25.78 wf/s at 28
(SATURATED)** → 15.42 at 30 → 5.64 at 34. **Four shards ≈ two shards (25.10 vs 25.04)**:
with `-spread` the extra shards land on the same two nodes, so shard count beyond node
count buys nothing. Capacity scales with **nodes**, consistent with finding R-2.

Past saturation the system suffers **congestion collapse**, not a plateau: offering 30
instead of 28 (+7%) cut throughput 40%. **No work was lost** — after the worst run the
database held 0 pending tasks and 0 non-terminal executions.

### The bottleneck, identified and then confirmed

| Offered 14, 1 shard | `WORKFLOW_WORKERS=4` | `WORKFLOW_WORKERS=16` |
|---|---|---|
| Verdict | **SATURATED** | not saturated |
| p50 | 3.01 s | **1.31 s** |
| Peak throughput reached | 12.98 wf/s | **25.26 wf/s** (at 26 offered, still not saturated) |

A configuration change gave 1.95× throughput and 2.3× lower p50 at the same load. It also
moved pool wait from 0.0% to 3.9%, so the pool becomes the *next* constraint.

## 4. Database and infrastructure, at the saturated point (28 offered, 2 shards)

| Quantity | Value |
|---|---|
| PostgreSQL CPU | 36.5% of one core (of 2) |
| Engine CPU (both pods) | 41.2% of one core |
| PostgreSQL RSS | 239 MB; engines 34 MB combined |
| Transactions/s | 3,749 (`xact_commit` counts **read-only autocommit statements too**) |
| WAL | 452 kB/s |
| Peak connections | 43 of `max_connections=250` |
| Buffer reads from disk | **0** — the whole working set is in `shared_buffers` |
| Pool wait | **0.0%** |

**Measured storage** (46.8k executions, 561k events): **11.99 events per workflow**,
**418 bytes per event** (308 heap + 110 index), **5,548 bytes of database per workflow**.
Mean replay 0.16–0.26 ms.

**Finding:** the `tasks` table holds **24 kB of heap against 4,472 kB of indexes** with zero
pending rows — index bloat on a high-churn queue table, which would degrade the very poll
path Test B2 measured at 2 buffers.

## 5. Monitoring overhead — measured by A/B, same binary and config

| Arm | Trial 1 | Trial 2 | Mean |
|---|---|---|---|
| Monitoring on | 12.90 wf/s | 13.04 | 12.97 |
| Monitoring off (stack deleted) | 13.04 | 13.06 | 13.05 |

Difference **0.6%**, inside run-to-run spread; p50 identical (~3.0 s). Resource cost:
Prometheus 0.25–0.45% of a core and 42–43 MiB; exporter 0.15–0.20% and 8 MiB; together
1.7–5.0% of (observer + engine) CPU.

## 6. Failed, invalid and corrected runs — all preserved

| # | Problem | Consequence | Resolution |
|---|---|---|---|
| 1 | Prometheus `nodes/metrics` RBAC | cAdvisor 403 on every scrape | needs `nodes/proxy` (API-server proxy path) |
| 2 | cAdvisor has **no `container` label** here | per-container queries returned nothing | report pod-level and say so |
| 3 | Replaced pods keep their cgroup series | gauges double-counted ~2× | `max by (pod)` + a preflight that refuses to measure until the count is 2. **Counters were immune** — `increase()` over a dead series is 0 |
| 4 | cAdvisor refreshes only ~every 15 s | `increase()` empty on a 22 s window | 60 s minimum window; sample count recorded per row |
| 5 | Sweep A rates 16–32 | generator worker-starved → `GENERATOR_BOUND`, not an engine measurement | re-run with 600–900 arrival workers; both kept |
| 6 | Container is named `engined`, not `engine` | arm-confirmation silently read 0 via `2>/dev/null` | fixed; **this masked defect 8 for one run** |
| 7 | `events.type` does not exist (it is `event_type`); duplicate-`k` query grouped all event types | false alarm of "3 duplicate k groups" on a healthy run | query corrected to `ActivityTaskScheduled` only |
| 8 | `kubectl apply` 3-way merge left `FAULT_CRASH_AT` set while resetting `FAULT_INJECTION` | engine **refused to boot** (correct); rollout stopped at `durexec-1` and never touched `durexec-0` | key declared explicitly in `10-config.yaml` |

## 7. Defects found and fixed

1. **F9-2 root cause — crash seams dead in production.** `cmd/engined` never called
   `eventlog.Store.UseFaultInjector` and never set `runtime.Engine.Faults`. Both injectors
   were `nil`, and because `ShouldCrash` safely handles a nil receiver it returned `false`
   **silently**. Two independent wiring omissions; no unit test could catch either, because
   the tests supply their own wiring. Fixed with one line each.
2. **`workflow_starts_total` mis-counted duplicates.** A duplicate start arrives as
   *success* with `res.AlreadyOpen=true`, not only as `ErrWorkflowAlreadyOpen`, so one
   create plus one duplicate recorded `accepted=2`. Caught by firing the event and reading
   the counter — not by reading the code.

## 8. F9-2 status: closed, all three boundaries proven

Armed from **configuration** (`FAULT_CRASH_AT`), not over the network: no endpoint was
added, so no new control surface exists. `config.Validate` refuses the variable unless
`FAULT_INJECTION=true` **and** the point is one some code path actually checks; tests cover
disarmed-never-fires, fires-exactly-once, point isolation, and rejection of unwired/typo'd
names.

| Boundary | Fired | Durable-state result |
|---|---|---|
| Workflow creation (`BeforeStartCommit`, new) | yes, owner only | HTTP 500, **executions=0 events=0 tasks=0** — no trace. Retry returned **201**, not `already_open`, independently proving nothing committed |
| Before decision commit, **mid-run** | yes | status **COMPLETED**, 4 scheduled / 4 completed, **0 duplicate `(run_id,seq)`** |
| After activity journaled, before ack | yes — log names `activity=ChargePayment seq=3` | 3 scheduled / 3 completed: replay found the journaled result and did **not** re-run the activity |

**Limitation:** these seams return an error at the boundary; they do not kill the process.
They test the **durable-state** boundary, not loss of in-memory state or lease expiry — real
process death is covered by the Phase 9 SIGKILL work. `BeforeNextPlanCompletes` (fixture
only) and `AfterLeaseRenew` (**no caller at all**) remain unwired and are now refused by
configuration rather than silently ignored.

## 9. Optimisation decision: batching and snapshots are NOT justified

| Candidate | Evidence | Decision |
|---|---|---|
| Commit-path batching (`stash@{0}`) | At every saturated point: pool wait **0.0%**, DB CPU ≤37% of one core, 0 disk reads. Database round-trips were never the constraint | **Do not merge.** Keep the stash; revisit once worker counts are raised and the pool becomes the limit (already visible at 3.9%) |
| History snapshots | Mean replay **0.16–0.26 ms** over 11.99 events/workflow; replay never appears in the latency budget (p50 1.5 s) | **Not justified.** Revisit only with a workflow that accumulates thousands of events |
| Worker/queue tuning | `WORKFLOW_WORKERS` 4→16 gave **1.95× throughput, 2.3× lower p50**, zero code | **Do this first** |
| `tasks` index bloat | 4,472 kB of indexes over 24 kB of heap at 0 pending rows | Worth a maintenance plan; not yet measured as a slowdown |

## 10. Remaining unmeasured

Per-container (as opposed to per-pod) CPU/memory; container disk I/O beyond
`fs_writes_bytes`; network bytes per container (cAdvisor emitted none);
**replication lag** (the Kubernetes lab has no standby — only compose does); replay cost at
large history sizes; `>2` node scaling; multi-trial variance at most points (1 trial each,
except the four-trial rate-14 set whose spread was 12.90–13.06 wf/s).

## 11. Production launch blockers (unchanged by this phase)

No importable SDK; no workflow versioning story; no backup or PITR procedure; asynchronous
replication; database failover still 9–15 s and manual; `extsim` not wired into `engined`,
so **exactly-once external effects remain unproven**; horizontal autoscaling impossible by
design (R-2); 9 commits unpushed and CI has never run.
