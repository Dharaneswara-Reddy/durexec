# Cloud test campaign

What to run, in what order, on which topology, and what each result is
allowed to claim.

**The Phase 8 contract is unchanged and no test here may overstate it:**
at-least-once external invocation, with effectively-once external effects
**only when the recipient honours the idempotency key**.

---

## Harnesses that already exist

Verified present in the repository; none needs writing.

| Harness | Location | Produces |
|---|---|---|
| Load generator | `cmd/loadgen` | completed/s, p50/p95/p99, started, failed, non-COMPLETED, 409 follows |
| Acceptance matrix | `cmd/acceptance` | 36 FR/NFR rows against a live cluster |
| Tier 1 suite | `scripts/aws/acceptance_tier1.sh` | T1.1–T1.8, resumable, per-row flushed CSV |
| Replay benchmark | `internal/bench` | `TestNFR6ReplayBudget`, `BenchmarkReplayLongHistories` |
| Evidence collector | `scripts/aws/collect_evidence.sh` | logs, compose state, `docker stats`, journal invariants, nine durability settings, extsim ledger, raw `/metrics` |
| Effect ledger | `internal/extsim` | `Requests`, `Performed`, `Replayed`, `Unkeyed`, `Injected` |
| Metrics | `/metrics` on each node | `replay_duration_seconds`, `seq_race`, `task_lock_lost`, pool statistics |

**Needs instrumentation:** disk IOPS and network latency are not exported.
Collect them with `iostat -x 5` and `iperf3` alongside, and add both to the
evidence bundle.

---

## Campaign A — smoke, 1 VM, 2 hours, $0.10

| Test | Proves | Does not prove |
|---|---|---|
| T1.1 truthful health | `/healthz` reports owned shards honestly — the defect recorded in `green-but-wrong-findings.txt` | Anything under load |
| T1.2 durable start and complete | A workflow is journalled before it runs, and reaches COMPLETED | Recovery |
| T1.3 duplicate starts | The `(shard_id, workflow_id)` primary key deduplicates at the front door | Semantic duplicates |
| T1.4 cross-node concurrency | Work spreads across three engine processes | Host independence |
| T1.8 no leftover work | 0 pending tasks, 0 non-terminal workflows | — |

**Success:** all five PASS; `cloud-tier1.csv` written.
**Evidence:** run `collect_evidence.sh tier1-baseline` **before any restart.**

---

## Campaign B — fault injection, 3 VMs, 6 hours, $0.88

Topology C from `TEST-ARCHITECTURE-OPTIONS.md`: `VM-DB` (leader + standby),
`VM-1` (node-a, Prometheus), `VM-2` (node-b, node-c).

| # | Test | Method | Success criteria | Proves | Does **not** prove |
|---|---|---|---|---|---|
| B1 | Engine process death | `docker kill -s KILL node-b` on VM-2 | 6/6 COMPLETED; 0 duplicate `(run_id, seq)` | Lease expiry and replay recovery | Host loss |
| B2 | Ownership moves, tokens advance | Kill node-c; diff `GET /v1/shards` | shards moved > 0; **no token regression**; every moved shard bumped; none unowned | Fencing tokens are monotonic | **That a fenced writer is rejected** |
| B3 | **Host loss with survivors** | `aws ec2 stop-instances` on VM-2 | VM-1 and VM-DB keep completing work; VM-2's shards reassigned within 2 × TTL | **The distributed claim.** Only testable with two or more VMs | Automatic recovery — reassignment is lease-driven, not orchestrated |
| B4 | **Network partition** | Security-group rule blocking VM-2 from VM-DB | VM-2's nodes lose their leases; no split-brain writes; token advances once | Partition tolerance of the ownership mechanism | Byzantine behaviour |
| B5 | Task redelivery and retries | `FAULT_CRASH_AT` at an armed crash point | Attempt counter increases; exactly one success event | At-least-once with deduplication | Exactly-once |
| B6 | External-effect idempotency | extsim ledger across B1–B5 | `Performed` equals the number of distinct dedup keys; `Unkeyed` = 0 | Effectively-once **given a cooperating recipient** | Exactly-once effects |
| B7 | Database process restart | `docker restart` the leader on VM-DB | A workflow completes afterwards | Reconnection | Host loss |

**Mandatory joint assertion.** `duplicate_effect_count == 0` is meaningful
**only when `Unkeyed == 0`**. Both must be asserted together; two correct
unkeyed effects would otherwise be reported as a duplicate.

**Evidence:** run `collect_evidence.sh` after **each** of B3 and B4, before
restarting anything. Phase 7 and Phase 8 each destroyed their own evidence by
restarting first — the extsim ledger lives in memory.

---

## Campaign C — failover drill, 4 VMs, 2 hours, $0.39

Topology D: leader and standby on separate hosts.

| Test | Method | Success criteria | Proves | Does not prove |
|---|---|---|---|---|
| C1 | **Leader-host loss and promotion** | Stop `VM-DB-leader` under load; `pg_promote()` on the standby; repoint the engine nodes | Promotion time, and events lost measured against the dead leader's own data directory | **Closes the NOT TESTED row** in `RESULTS_SUMMARY.md` §10 | **Automatic** failover — this is manual and must be reported as such |

Expect **1–2 events lost**, as the local drill measured. Async replication has
a loss window. **Do not report zero data loss.**

---

## Campaign D — saturation, 1 m7i.large, 8 hours, $0.88

Only if a burst-credit cliff is actually observed on `t3`.

Sweep offered load until `completed/s` stops rising while in-flight work
climbs. **Record the generator's own limit at every point** — the
concurrency-16 trap in `docs/results/README.md` is exactly this mistake, where
8.5 wf/s turned out to be the generator rather than the engine.

| Measure | Source |
|---|---|
| completed/s, p50/p95/p99 | `cmd/loadgen` |
| Queue wait, in-flight slope | loadgen plus the `tasks` table |
| Postgres transactions/s, WAL, buffers, connections | `pg_stat_*` |
| CPU, memory, GC | `docker stats`, `/metrics` |
| Disk IOPS and latency | `iostat -x 5` — **must be added to the bundle** |
| Network throughput and latency | `iperf3` between VMs — **must be added** |

---

## Experiment matrix — one variable at a time

| Run | Nodes | Shards | Concurrency | Fault | Purpose |
|---|---|---|---|---|---|
| 1 | 3 | 4 | 128 | none | Baseline |
| 2 | 3 | 1 | 128 | none | Shard scaling |
| 3 | 3 | 2 | 128 | none | Shard scaling |
| 4 | 3 | 4 | 32 | none | Concurrency sensitivity |
| 5 | 3 | 4 | 256 | none | Saturation |
| 6 | 3 | 4 | 128 | kill 1 container | Process recovery |
| 7 | 3 | 4 | 128 | **stop 1 VM** | **Host recovery** |
| 8 | 3 | 4 | 128 | **partition** | **Partition** |

Runs 1–5 change one variable each. Runs 6–8 change only the fault, holding
load fixed, so any change in behaviour is attributable to the fault.

---

## Language that must not appear in the results

| Forbidden | Say instead |
|---|---|
| "exactly-once" | "at-least-once invocation, effectively-once effect given a cooperating recipient" |
| "zero data loss" | "no accepted work lost; 1–2 committed events lost to async replication" |
| "high availability" | "failover in 9–15 s, manual" |
| "production ready" | "validated under induced failure on a three-node test cluster" |
| "N times faster" | the measured number, with the hardware stated |
