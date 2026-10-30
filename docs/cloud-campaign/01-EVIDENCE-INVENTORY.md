# Evidence inventory — every metric collected before the cloud campaign

Compiled 2026-10-30 from `docs/results/`, `experiments/results/`,
`docs/cloud-testing/`, `docs/cloud-deployment/` and the source. For each
metric: what it measures, how it was collected, where it came from, what it
showed, whether that was conclusive, and whether the AWS harness can collect
the same thing.

**No cloud measurement exists yet.** Every "result" below is from the laptop
(i5-1335U, 10 cores / 12 threads, 15 GiB) or the 2-vCPU / 3.7 GiB minikube lab.

"AWS" column key: **same** = collected the same way in the cloud harness;
**new** = collected in the cloud by instrumentation added on 2026-10-30
(`03-HARNESS-READINESS.md`); **gap** = not collectable, with the reason.

---

## 1. Throughput and latency

| Metric | How collected | Source | Previous result | Conclusive? | AWS |
|---|---|---|---|---|---|
| Completed wf/s, closed loop | `cmd/loadgen -mode scaling`, completions inside the window ÷ window | `scaling-saturated*.txt` | 12.80 / 23.15 / 46.30 wf/s at 1/2/4 shards, c=128, reproduced on 2 later builds | **Yes for ratios** (1.81×, 2.00×); absolute figures are laptop-specific | same |
| Completed wf/s, closed loop, 16 shards | same, one process per point (`shard_sweep.sh`) | `local-shard-sweep/` | 47.15 (c128) → **129.30 (c256)** → 44.50 (c512) | **No.** c128 offered-load-limited; c512 confounded (generator 101 %, host load 21.97) | same |
| Completed wf/s, open loop + saturation verdict | `loadgen -mode arrival`, generator lag and in-flight slope gates | `phase6-sweep.csv` tag `aw600` | **12.98 wf/s per node** (SATURATED at 14 offered); 25.78 on 2 nodes (1.99×) | **Yes**, on the 2-vCPU lab; rows tagged `baseline` at ≥16 offered are GENERATOR_BOUND and invalid | same (MODE=arrival) |
| Overload behaviour, open loop | same, offered 16 and 20 on 1 node | `phase6-sweep.csv` `aw600` | **Collapse, not plateau:** 20 offered → **4.0 wf/s**, p99 76 s, 292 k empty pool acquires, 15 120 s cumulative pool wait | Yes for that lab: the pool is the failure mechanism under overload | same + new (pool deltas per run) |
| Latency p50 / p95 / p99, client end-to-end | loadgen, start request → terminal status, 100 ms poll | all loadgen runs | c128 16 shards p50 2.57 s p99 5.27 s; c256 p50 1.92 s p99 2.82 s | Partly. **`local-shard-sweep/results.csv` latency columns are corrupt** (they hold the shard count) and the raw loadgen `.txt` files were never committed, so FINDINGS.md's latency figures cannot be re-derived from the repository | same; p95 added |
| Latency p90 / max | — | — | never recorded | — | new (journal decomposition gives p90 and max) |
| **Latency decomposition** (queue wait, decision turnaround, activity sched→done, timer lag) | — | — | **never measured.** One local trace (2026-10-30, harness validation, not a result): a 50 ms durable timer fired **636 ms** late; over 113 runs timer lag p50 709 ms, p99 1 186 ms, against a 1 s timer-scan interval (`internal/timers/scanner.go:37`) | **Hypothesis only** | new (`sql/run_latency_and_invariants.sql`) |
| Service floor of the workload | declared to loadgen (`-service-floor`), not measured | `cmd/loadgen/arrival.go:38` | assumed ≈ 2.01 s | **Not measured.** A local idle run gave p50 1.50 s — the declared floor is 34 % high for that host | new (calibration stage C0) |
| Replay time | `go test ./internal/bench` | `RESULTS_SUMMARY.md` §3 | 200 events p50 3.11 ms p99 5.65 ms; linear to 60.6 ms at 5 000 | Yes (microbenchmark, laptop) | same (`replay_duration_seconds` delta per run) |

## 2. Scaling variables

| Variable | How varied | Previous result | Conclusive? | What it means for the cloud |
|---|---|---|---|---|
| Shards **driven** (of 16 deployed) | `loadgen -sweep N` | 9× from 1→16 driven at c128 | **No**, and confounded twice: (a) offered load fixed; (b) `WORKFLOW_WORKERS` is **per shard**, so driving more shards also drives more workers | Deploy `SHARD_COUNT` for real, and separate shards from workers (matrix E1) |
| Shards **deployed** | never varied | — | — | E1 |
| Engine nodes | Phase 6 1→2 nodes; compose 3 nodes | 1.99× (open loop, lab) | Yes for 1→2 on the lab | E2: 1/2/3 nodes on dedicated hardware |
| `WORKFLOW_WORKERS` | Phase 6: 4 → 16 at 1 shard | **1.95× throughput, p50 3.01 → 1.31 s**; pool wait 0 % → 3.9 % | **Yes** — the binding constraint on the lab was worker concurrency, then the pool | E1 holds total workers constant to test it on cloud |
| Concurrency (closed loop) | c16 / 64 / 128 / 256 / 512 | c16 generator-bound (8.5 wf/s = 16 ÷ 1.9 s); c256 best; c512 collapse | c16: **invalid** (kept as the trap); c512: inconclusive | Closed loop only as a cross-check; knee located open-loop |
| `SYNC_COMMIT` | never varied | — | — | E3: cost of durability |
| DB instance size | never varied | — | — | Excluded unless the DB is attributed as the bottleneck (see matrix §6) |

## 3. Generator

| Metric | How collected | Previous result | Conclusive? | AWS |
|---|---|---|---|---|
| Generator CPU % | `/usr/bin/time` around loadgen (`.time` files) | 49–63 % of one core up to c256; **101 % at c512** | Yes as a saturation flag | **new** — kubelet Summary API counters during the window. The previous EKS script used `kubectl top` after the Job finished, which returns nothing without metrics-server, so its saturation verdict could never fire |
| Generator RSS, wall time | `/usr/bin/time` | 27–51 MB; 25.1 s | Yes | new (working set peak) |
| Generator schedule lag p99, dropped arrivals | loadgen arrival mode | lag p99 ~1.4 ms locally | Yes | same |
| Generator placement | `spec.nodeName` + node role label | — (laptop) | — | same; a generator not on the harness node is INVALID |

## 4. Engine process

| Metric | How collected | Previous result | Conclusive? | AWS |
|---|---|---|---|---|
| Engine CPU per node | `docker stats` samples (`.enginecpu`), **4 samples per run** | Never reduced until 2026-10-30. Reduced now: 16 shards c128 = 74 / 113 / 64 %; c256 = 87 / 130 / 73 %; c512 = 113 / 171 / 108 % (node-a / node-b / node-c) | **Weak** (4 samples). It does show CPU tracking shard ownership: node-b held **9 of 16 shards** and ran hottest at every point | new: exact CPU-seconds per pod and per node from cumulative counters |
| Ownership skew | `/v1/shards` | 4 / 9 / 3 shards across 3 nodes | Yes, as an observation | same (shards before/after per run) |
| Goroutines, RSS, GC pauses | `go_*` and `process_*` collectors | Phase 6 only | Yes | new (per-run deltas from every pod) |
| Pool acquires / empty acquires / wait | `db_pool_*` | Phase 6: pool wait 0 → 3.9 % at w16; collapse at overload. Local sweep: **20 pool + 2 renewal per node at every shard count** | The local "pool refuted" verdict holds only for closed-loop, sub-knee runs | new (per-run deltas, empty-acquire fraction) |
| Pool acquires per workflow | — | never computed. Local harness check: ~104 acquires per completed workflow at c=64 — most DB traffic is polling | Observation | new |
| Lease transitions, fencing rejections, task failures | `lease_transitions_total`, `fencing_rejections_total`, `worker_task_failures_total` | Phase 9 fault runs | Yes | new (per-run deltas; "absent" ≠ 0 is recorded) |
| `task_queue_oldest_ready_seconds` | gauge | Phase 6 | Yes | new (max per run) |
| Lease **renewal latency** | — | **not exported** | — | **gap** — only the transition counter exists; renewal latency would need new engine instrumentation (recommendation R4) |
| DB operation latency from the engine's side | — | not exported | — | partial: `pg_stat_statements` mean per statement is the server-side substitute |

## 5. PostgreSQL

| Metric | How collected | Previous result | Conclusive? | AWS |
|---|---|---|---|---|
| Commits/s, rollbacks, WAL bytes/records/FPI, checkpoints, buffers, statement calls/time, peak connections, deadlocks, DB size | `pg_stat_*` deltas (`perf_collect.py`) | `phase6-sweep.csv` (lab) | Yes for the lab | **new** — `sql/pgstat_snapshot.sql` before/during/after every point. **The cloud DB host did not enable pg_stat_statements or track_io_timing** until 2026-10-30 |
| Per-run pg_stat deltas in the laptop sweep | — | **missing** (FINDINGS §7 "Open") | — | new |
| Wait-event profile | — | never | — | new (sampled every 5 s) |
| Lock waits, `pg_locks` not granted | `log_lock_waits` (lab only) | not reduced | — | new (peak per run) |
| DB-host CPU, iowait, steal, disk IOPS and utilisation | `docker stats` / cAdvisor (lab) | lab: shared with engine, unattributable | No | **new** — host `/proc` via `pg_read_file` (no exporter on the EC2 host) |
| Connection budget | `pg_stat_activity` | 66 cluster-wide, constant | Yes | same |
| Storage per event / workflow | `pg_total_relation_size` | 418 B / event, 5 548 B / workflow, 11.99 events / workflow; `tasks` 24 kB heap vs 4 472 kB index | Yes | same (`g_db_size_bytes`) |
| Poll query cost vs depth | `EXPLAIN ANALYZE` on the real schema | 0.011–0.014 ms from 1 k to 100 k rows, index used | Yes | not repeated (no reason to expect hardware to change the plan) |
| Retry storm poll rate | `pg_stat_statements` | ~149 polls/s in storm vs ~411 healthy; latency unchanged | Yes | not repeated |

## 6. Infrastructure

| Metric | Previous | AWS |
|---|---|---|
| Node CPU / memory | laptop host only | new (kubelet Summary, per node) |
| Pod CPU / memory | `docker stats` | new |
| CPU throttling | — | **gap**: the Summary API does not expose throttled periods. `engined` has `limits.cpu: "2"` (`30-engine.yaml:185`), equal to an m7i.large's 2 vCPUs, so the quota cannot be exceeded on the pilot's engine nodes — an argument, not a measurement. It WOULD bind on a larger instance type; any run on one must read `cpu.stat` (`nr_throttled`) through a node debug pod first. Memory limit 512 Mi against a measured RSS of ~30–40 MB |
| Node pressure, evictions, restarts, readiness | — | same (`kubectl get events`, pod JSON, every run) |
| Network throughput | — | new (per-pod and per-node rx/tx bytes from the Summary API) |
| Engine/harness → DB latency | — | new: `psql \timing select 1` from the harness node, 20 samples per point (an application-level RTT, not ICMP) |
| Engine node → DB RTT | — | **gap**: the distroless engine cannot run a probe; the harness and engine nodes share one subnet, so the harness RTT is the proxy and is labelled as such |
| Disk IOPS on the DB volume | — | new (`/proc/diskstats`) |
| Kubernetes scheduling delay | — | partial: pod events timestamps (Scheduled → Started) are kept in every bundle; not reduced |
| EC2/EKS instance characteristics | — | same (node labels: instance type, zone; evidence bundle) |

## 7. Correctness

| Property | How established before | Result | AWS |
|---|---|---|---|
| No step executed twice | SQL on `(shard, workflow, run, k)` after a 2-node kill | **0 of 3 512**; 0 of 3 052 on re-run | new: per run AND per fault trial (`dup_activity_completion`, `dup_run_seq`, `seq_gaps`, `multi_terminal`, orphan completions) |
| No accepted work lost | count of executions vs accepted | 878 / 878 COMPLETED | new: drain check before judging (a trial that does not drain in 300 s is `LOST_OR_STUCK`) |
| Effectively-once external effect | extsim ledger | 7 scenarios, 1 effect per key; scenario E leaves the effect uncompensated | **not in this campaign's core**: extsim is deployable but the matrix runner is the Compose/minikube script (`experiments/scripts/extsim-matrix.sh`). Listed as optional stage F9 |
| Fencing rejects a stale owner | Phase 8 (I), Phase 9 stale-owner | token advanced; **but the stale worker was stopped by the task-claim check, not by fencing** | new: freeze trial records `fencing_rejections_total` delta of the thawed victim |
| Recovery timing | Phase 9 oracle on the PG clock | graceful 5.05 s mean; frozen-node takeover **29.86 s** (sd 1.15) against a 10 s TTL; abrupt kills contaminated by restart back-off | same oracle (`eks_fault_trial.sh`), victims rotated, back-off detected |
| DB outage recovery | Phase 9 Test A | engine back 10–13 ms after PG returns; no pod restart; 25 s restart costs ~8 s extra | new: EC2 reboot of the DB host (real host restart) |
| DB failover | compose standby drill | 9–15 s manual; 1–2 events lost (async) | **excluded** — the pilot has one PostgreSQL host and no standby |

## 8. Inconclusive and invalid results carried forward (not discarded)

| Result | Why inconclusive | What the cloud does about it |
|---|---|---|
| Laptop 1 and 2 driven shards (5.25 vs 4.85 wf/s) | ~100 completions, p50 above the window | Not repeated as subset sweeps; E1 deploys real shard counts with ≥ 1 000 completions per point |
| Laptop c512 collapse | generator, engine and host all saturated together | E5 overload on dedicated nodes, open loop, with pool and DB attribution |
| Laptop c128 shard sweep | offered-load-limited (proved by c256) | every configuration gets its own knee search |
| Hypothesis 4 (DB WAL/fsync/locks limit throughput) | not instrumented | wait-event profile, lock waits, WAL rate, `synchronous_commit` arm (E3) |
| Hypothesis 5 (engine CPU limits throughput) | samples not reduced | reduced above (weak); exact per-pod CPU in the cloud |
| Hypothesis 7 (subset sweep ≠ deployment) | — | E1 deploys real shard counts |
| Phase 9 abrupt-kill trials | restart back-off contamination | rotation, cool-down, crash-loop detection |
| `results.csv` latency columns | parser defect (values = shard count) | the cloud runner's parser was tested against real loadgen output |
| Missing raw loadgen output for the local sweep | never committed | the cloud runner keeps every raw file per point under `raw/<run>/` |
