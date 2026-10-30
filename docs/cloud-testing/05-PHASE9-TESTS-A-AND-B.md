# Phase 9 Follow-up — Test A (PostgreSQL outage) and Test B (retry storm)

Closes the two gaps declared in
[`04-PHASE9-FAULT-INJECTION.md`](04-PHASE9-FAULT-INJECTION.md) §5.

Commit under test: **`fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac`** (unchanged —
**no production code was modified in this phase**)
Cluster: minikube v1.35.0 (kvm2), Kubernetes v1.32.0, **2 vCPU / 3.8 GiB**
PostgreSQL `pgvector/pgvector:pg16`, 8 GiB PVC, `max_connections=250`
2 engine replicas · 16 shards · `LEASE_TTL=10s`

### Raw evidence

| File | Contents |
|---|---|
| `experiments/results/phase9-db-outage.csv` | Test A, 4 trials |
| `experiments/results/phase9-db-outage-logs/` | engine logs during and after each outage |
| `experiments/results/phase9-retry-storm.csv` | Test B, healthy vs storm, 14 samples |
| `experiments/results/phase9-retry-storm-plans.txt` | `EXPLAIN (ANALYZE, BUFFERS)` at live queue shapes |
| `experiments/results/phase9-poll-depth.csv` | Test B2, poll cost vs depth |
| `experiments/results/phase9-poll-depth-plans.txt` | plans at 1k / 10k / 100k rows |
| `experiments/results/phase9-workload-*.csv` | generator output per run |

### Scripts added (no production code touched)

`experiments/scripts/db-outage.sh`, `experiments/scripts/retry-storm.sh`,
`experiments/scripts/poll-depth.sh`

---

## Headline

| Test | Result |
|---|---|
| **A — PostgreSQL outage** | **PASS.** 4/4 trials. Engine recovered **10–13 ms** after the database returned; progress resumed **135–188 ms** later. **0 orphans, 0 duplicate journal records, 0 duplicate invocations, 0 stuck** in all 4. Committed state survived a real restart. |
| **B — retry storm** | **PASS with a documented limitation.** The storm reached **0 claimable of 20 tasks** and poll latency did **not** degrade. But at 20 rows the planner uses a Seq Scan, so the poll index was not exercised; a separate depth probe was required. |
| **B2 — poll cost vs depth** | **PASS, and it corrects the audit.** Poll cost is **constant at 2 buffers / ~0.012 ms from 1,000 to 100,000 rows**, index `tasks_poll_ready` used throughout. |

---

## Test A — PostgreSQL outage recovery

### Method

Two genuinely different failures, because they fail differently:

| Method | Mechanism | Shape |
|---|---|---|
| `pause` | `docker pause` on the postgres container inside the node | TCP stays open, nothing answers — the **partition** shape, no RST to react to |
| `restart` | `kubectl scale statefulset/postgres --replicas=0` then `1` | Process gone, connections **refused**, then restart + WAL replay. PVC retained, so this is also the **durability** test |

Every trial ran against a live open-loop workload driven through the NodePort
(`31-engine-nodeport.yaml`), so load continued to be offered throughout.

**The outage was verified, not assumed.** Each trial polls `pg_isready` until it
fails, and records `INVALID_db_never_unavailable` if the database never actually
went down. No trial was reported as an outage test without that proof.

### Results — 4 trials

| Method | Trial | DB unavailable after | DB back at | **Engine recovered** | **Progress resumed** | Orphans | Dup journal | Dup invoke | Stuck |
|---|---|---|---|---|---|---|---|---|---|
| pause | 1 | 358 ms | 30903 ms | **30914 ms** (+11) | **31046 ms** (+143) | 0 | 0 | 0 | 0 |
| pause | 2 | 493 ms | 31149 ms | **31162 ms** (+13) | **31337 ms** (+188) | 0 | 0 | 0 | 0 |
| restart | 1 | 213 ms | 32920 ms | **32932 ms** (+12) | **33070 ms** (+150) | 0 | 0 | 0 | 0 |
| restart | 2 | — | 32801 ms | **32811 ms** (+10) | **32946 ms** (+135) | 0 | 0 | 0 | 0 |

`(+n)` is milliseconds **after** the database became reachable again.

**Finding A-1 — recovery is reconnect-speed, not restart-speed.** The engine
regained service **10–13 ms** after PostgreSQL returned, in all four trials, with
**no pod restart** (restart counts unchanged). The pgx pool re-establishes
connections transparently; nothing had to be rescheduled.

**Finding A-2 — a 25 s database restart costs ~8 s beyond the outage window.**
`restart` trials held the database down for 25 s but `pg_isready` only passed at
~32.9 s: pod recreation plus startup and WAL replay. That overhead belongs to
PostgreSQL, not the engine.

### Durability across a real restart

| | Before restart | After restart |
|---|---|---|
| `executions` | 8,643 | 9,005 |
| `events` | 103,685 | 108,008 |

Counts only increased, as the workload continued. **Nothing committed was
lost.** `pg_postmaster_start_time()` confirmed the server genuinely restarted
(uptime 51 s when checked).

### Transient failure versus correctness failure

During the outages the engine logged **183 × `ERROR start workflow failed`** and
**2 × `WARN activity failed, releasing task for another attempt`**.

These are **expected transient request failures**: unable to write durably, the
engine **refused new work** rather than accepting what it could not persist.
The oracle confirms the distinction — **0 orphans** means no workflow was
accepted and then abandoned, which is exactly the F-01 class of defect this
behaviour prevents.

**No workflow failed as a result of a database outage.** All 42 terminal
failures cluster-wide are attributable to deliberate fault injection in Test B
or to one malformed input of mine (see below).

### Lease behaviour across the outage

Neither node could renew during the outage, so both lost every lease; on
recovery each re-acquired its **own** shards via the `owner_node = $2` fast path
documented in `04`'s §3. Fencing tokens advanced on both nodes
(durexec-0 2→5, durexec-1 22→26). **No shard was stolen**, because no node could
reach the database either — the failure was symmetric.

---

## Test B — retry storm

### Method

The storm is **real, not synthetic**. Retryable activity failures were armed
through `POST /v1/admin/fail-step` (400 per activity type, `non_retryable:false`),
so each failure travels the engine's own retry path and the resulting backoff is
production backoff. **No row was hand-inserted into `tasks` for this part**, and
no temporary table was used.

`FAULT_INJECTION` was switched on in the lab ConfigMap for this test only and
**reverted to `off` afterwards**. It is a deployment value, not production code.

Poll latency is the server's own timing of the **real** `pollSQL`, read from
`pg_stat_statements` (extension created in the lab database; the library was
already in `shared_preload_libraries`).

### Results — healthy baseline vs storm

| Phase | t (s) | Tasks | **Claimable** | Backing off | Poll calls | **Poll mean** | Poll max | Blks read |
|---|---|---|---|---|---|---|---|---|
| healthy | 0 | 14 | 11 | 3 | 428 | 0.053 ms | 1.045 ms | 0 |
| healthy | 15 | 14 | 8 | 6 | 8127 | 0.065 ms | 5.618 ms | 0 |
| healthy | 30 | 11 | 1 | 10 | 16029 | 0.071 ms | 5.618 ms | 0 |
| healthy | 45 | 17 | 11 | 6 | 24334 | 0.064 ms | 8.553 ms | 0 |
| healthy | 60 | 5 | 1 | 4 | 32793 | 0.061 ms | 8.553 ms | 0 |
| healthy | 90 | 15 | 11 | 4 | 41351 | 0.059 ms | 8.553 ms | 0 |
| healthy | 120 | 12 | 4 | 8 | 49808 | 0.058 ms | 8.553 ms | 0 |
| **storm** | 15 | 20 | **0** | 20 | 3649 | 0.063 ms | 2.201 ms | 0 |
| **storm** | 30 | 20 | **0** | 20 | 6473 | 0.062 ms | 6.012 ms | 0 |
| **storm** | 45 | 20 | **0** | 20 | 9502 | 0.061 ms | 6.012 ms | 0 |
| **storm** | 60 | 20 | 1 | 19 | 12423 | 0.060 ms | 6.012 ms | 0 |
| **storm** | 90 | 20 | **0** | 20 | 15269 | 0.057 ms | 6.012 ms | 0 |
| **storm** | 120 | 20 | **0** | 20 | 18247 | 0.055 ms | 6.012 ms | 0 |

**Finding B-1 — the required "nothing claimable" case was reached.** Five of six
storm samples show **0 claimable of 20 tasks**, i.e. 100% backing off.

**Finding B-2 — poll latency did not degrade.** Storm mean 0.055–0.063 ms against
healthy 0.053–0.071 ms; storm max 6.0 ms against healthy 8.6 ms. `shared_blks_read`
stayed at **0** throughout: the working set never left shared buffers.

**Finding B-3 — poll rate fell ~2.7× during the storm.** ~149 polls/s versus
~411 polls/s healthy. Workers finding no claimable work poll less often, so an
empty queue costs *less* database traffic, not more.

### Limitation B-L1 — the storm did not exercise the poll index

The captured plans at live queue shapes show **`Seq Scan`**, `Bitmap Heap Scan`
and `tasks_pkey` — **never `tasks_poll_ready`**.

That is **correct planner behaviour**, not a defect: at ~20 rows a sequential
scan is genuinely cheaper than an index descent. But it means the flat latency in
B-2 proves the table is *small*, **not** that the index does its job.

The audit's original concern was measured at **100,000 rows per shard**. This
host cannot generate that depth through the generator: queue depth is bounded by
in-flight workflows, and 2 vCPU limits concurrency. Hence Test B2.

---

## Test B2 — poll cost versus queue depth, on the real schema

### Why this is the correct form of the measurement

The audit's 52× figure was taken on a **TEMP table that omitted the primary
key**, so the planner saw neither production statistics nor production access
paths. Here the rows go into the **real `tasks` table**, with the real
`tasks_pkey` and the real `tasks_poll_ready` index, and `ANALYZE` is run so the
planner sees true statistics.

**Safety.** Every probe row has `visible_at = now() + 1 day`, so
`GREATEST(visible_at, …)` is always in the future and **no worker can ever claim
one**. They are inert backlog. All carry the `__depthprobe__` prefix and are
deleted by an `EXIT` trap — verified: **0 remaining**. There are no foreign keys
on `tasks`, so no integrity constraint was bypassed.

### Results

| Depth (shard 0) | Claimable | Scan | Actual time | Buffers hit | Buffers read | Index used |
|---|---|---|---|---|---|---|
| 1 | mixed | Seq Scan | 0.022 ms | 18 | 0 | none |
| 1,001 | 0 | **Index Scan** | **0.011 ms** | **2** | 0 | `tasks_poll_ready` |
| 10,000 | 0 | **Index Scan** | **0.012 ms** | **2** | 0 | `tasks_poll_ready` |
| 100,001 | 0 | **Index Scan** | **0.014 ms** | **2** | 0 | `tasks_poll_ready` |
| 100,001 | **1 needle** | **Index Scan** | **0.011 ms** | **2** | 0 | `tasks_poll_ready` |

Sizes at 100k rows: heap 11 MB, `tasks_poll_ready` 1192 kB, `tasks_pkey` 3128 kB.

**Finding B2-1 — poll cost is constant in queue depth.** Two buffers and
~0.012 ms whether the backlog is 1,000 or 100,000 rows. A hundredfold increase
in depth changed nothing measurable.

**Finding B2-2 — finding one claimable task among 100,000 backing-off tasks costs
the same two buffers.** This is the case that matters operationally, and the
index serves it exactly.

**Finding B2-3 — the planner switches correctly.** Seq Scan at 1 row, Index Scan
from 1,000 up. B-L1's Seq Scan was the right choice at that size.

### Comparison with the pre-fix figure

| | Pre-fix (recorded in `internal/taskq/queue.go:193`) | **Measured now** |
|---|---|---|
| One poll at 100k rows/shard | **95 ms**, **208 MiB** read | **0.011 ms**, **2 buffers (~16 KiB)** |

Both rows read **and** buffers touched are reported, as required: the
improvement is not an artifact of counting rows alone. This substantiates
commit `bf6ffb1` on the production schema, which the audit's own measurement
did not.

---

## Unplanned result — saga compensation is correct under injected failure

The injected failures exhausted their 5-attempt retry budget and produced 42
terminal failures. Their histories show the saga running:

```
1 WorkflowExecutionStarted
2 ActivityTaskScheduled   ChargePayment
3 ActivityTaskCompleted
4 ActivityTaskScheduled   ReserveInventory
5 ActivityTaskFailed      after 5 attempts: injected failure
6 ActivityTaskScheduled   RefundPayment      <-- compensation
7 ActivityTaskCompleted
8 WorkflowExecutionFailed
```

| Failure point | Count | Compensated | Verdict |
|---|---|---|---|
| `ReserveInventory` (k=1), charge had succeeded | 17 | **17 of 17** | debt owed, debt paid |
| `ChargePayment` (k=0), charge itself failed | 25 | **0** | correct — no side effect to undo |

**0 uncompensated.** This matches the rule stated in
`app/delivery/workflow.go:203`: a compensation is owed only once the side effect
is known to have happened.

Compensations are recorded as **ordinary activities**, not a distinct event type.
That is why a first search for `%Compensat%` event types returned nothing; the
absence was a bad probe, not a missing behaviour. Recorded here because it would
mislead the next reader too.

---

## Failure accounting — all 42 explained

| Reason | Count | Classification |
|---|---|---|
| `ChargePayment failed after 5 attempts: injected failure` | 24 | **deliberate** (Test B) |
| `ReserveInventory failed after 5 attempts: injected failure` | 17 | **deliberate** (Test B) |
| `ChargePayment: amount must be positive, got 0` | 1 | **deliberate** — malformed smoke input from Phase 9 |

**Zero unexplained failures. Zero durability failures. Zero failures caused by a
database outage.** Cluster-wide terminal state at close: 16,585 COMPLETED,
42 FAILED, 6 RUNNING (in flight, workload still active).

Note: the `dbout-` and `dbres-` generators were still submitting when fault
injection was later armed, so some of their workflows carry injected failures.
That is why those prefixes show failures despite their outages causing none.

---

## Oracle reconciliation

| Workload | Started | Completed | Failed | Orphans | Dup journal | Dup invoke | Stuck |
|---|---|---|---|---|---|---|---|
| `dbout-` | 2970 | 2952 | 13 | **0** | **0** | **0** | 0 |
| `dbres-` | 2600 | 2587 | 13 | **0** | **0** | **0** | 0 |
| `storm-` | 2714 | 2699 | 15 | **0** | **0** | **0** | 0 |

Correctness is read from **durable state**, never from engine metrics.

---

## Invalid and corrected runs (preserved, per spec §12)

| Item | Status | Reason |
|---|---|---|
| Test B first attempt | **Produced no samples** | `set -euo pipefail` plus a pipe in the query helper aborted the sweep on one transient `kubectl exec` hiccup, losing every later sample. Harness changed to degrade to `NA`. |
| Test B poll-latency columns, first run | **Empty** | `round(double precision, int)` does not exist in PostgreSQL; `total_exec_time` needed a `::numeric` cast. Silently returned nothing. |
| Test B queue-shape columns, first run | **Internally inconsistent** | Each count was its own statement, so `now()` advanced between them and `claimable + backing_off` could exceed the total. Rewritten as one snapshot query. |
| `storm2-` prefix | **0 workflows** | The launcher reused the already-running generator, so this prefix was never used. Not an engine result. |

No run was deleted.

---

## Limitations

**L-1 (carried forward, unchanged).** `internal/extsim` is **not wired into
`engined`**. Nothing here is evidence of exactly-once **external** effects. Only
journal records and activity invocations were counted.

**L-2.** Host is **2 vCPU / 3.8 GiB**; minikube cannot resize an existing
profile. No throughput figure from this host is publishable.

**L-3.** Queue depth through the generator is bounded by in-flight workflows
(~20 tasks here). Depth beyond that was reached only with inert probe rows, which
measure **poll cost**, not end-to-end behaviour under a genuinely deep live
backlog.

**L-4.** Test A injected fault during normal execution with scheduling, claiming
and retry all active concurrently across 16 shards. It did **not** target a
specific lifecycle instant — the crash points that would allow that remain
unarmable (defect **F9-2** in `04`).

**L-5.** Database CPU and I/O counters were not collected: no PostgreSQL
exporter is deployed. Buffer behaviour was captured instead, via
`pg_stat_statements` and `EXPLAIN (ANALYZE, BUFFERS)`. Closing this is Phase 6 work.

**L-6.** Single node, so no node failure, partition or zone loss.

---

## Reproduce

```bash
./experiments/scripts/lab.sh up
IP=$(minikube ip)
go build -o /tmp/loadgen ./engine/cmd/loadgen
/tmp/loadgen -mode throughput -base http://$IP:30080 -shards 16 -concurrency 8 \
  -duration 1200s -prep 200ms -id-prefix dbout- \
  -csv experiments/results/phase9-workload-dbout.csv &
sleep 35

# Test A
./experiments/scripts/db-outage.sh pause   30 2
./experiments/scripts/db-outage.sh restart 25 2

# Test B  (requires FAULT_INJECTION: "true" in 10-config.yaml + rollout restart)
./experiments/scripts/retry-storm.sh healthy 0
./experiments/scripts/retry-storm.sh storm 400

# Test B2 (self-cleaning)
./experiments/scripts/poll-depth.sh

./experiments/scripts/oracle.sh dbout-
```

---

## Go / No-Go for Phase 6 metrics

### GO.

**Tested, with evidence**

- PostgreSQL outage and restoration, 2 failure shapes, 4 trials: engine recovers
  in **10–13 ms** after the database returns, needs no restart, loses nothing.
- Committed state survives a genuine PostgreSQL restart.
- Transient request rejection is distinguished from durability failure:
  183 refusals, **0 orphans**.
- Retry storm reaching **0 claimable tasks**, with no poll-latency degradation.
- Poll cost **constant from 1k to 100k rows** on the production schema with
  production indexes — correcting the audit's temp-table measurement.
- Saga compensation: **17 of 17** owed compensations ran; 25 with nothing to undo
  correctly ran none.
- **0 orphans, 0 duplicate journal records, 0 duplicate activity invocations**
  across 8,284 workflows in these tests.

**Not tested, and not claimed**

- Exactly-once **external** effects (L-1 — structurally unmeasurable here).
- Crash **at** the decision-commit and activity-journal boundaries (F9-2).
- Database CPU/IOPS/WAL rate (L-5 — exactly what Phase 6 is for).
- Deep **live** backlog, as opposed to inert probe rows (L-3).
- Node failure, partition, multi-node (L-6).

**Why this justifies proceeding.** Both declared gaps are closed with real
measurements, no new correctness defect was found, and the two most load-bearing
performance claims in the repository — the poll index and failover timing — now
rest on measurements taken against the production schema rather than a temp
table. The remaining unknowns are **observability** gaps (L-5), which is
precisely what Phase 6 exists to fix.

**Carry forward into Phase 6:** deploy a PostgreSQL exporter and a container
metrics path (L-5), and resolve **F9-2** so scenarios 3–5 become testable.

**This is not a production-readiness statement.** The launch blockers in
`docs/audit/` and `docs/cloud-testing/01-RECON.md` (no importable SDK, no
workflow versioning, no backup/PITR, asynchronous replication) all still stand.
