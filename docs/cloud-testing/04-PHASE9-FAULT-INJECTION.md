# Phase 9 — Fault Injection and Recovery Validation

Commit `fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac` · 2026-10-26/27
Cluster: minikube v1.35.0 (kvm2), Kubernetes v1.32.0, **2 vCPU / 3.8 GiB**
Image `durexec-engine:local` · 2 engine replicas · 16 shards · `LEASE_TTL=10s`

Raw evidence:
`experiments/results/phase9-recovery.csv`,
`experiments/results/phase9-stale-owner.csv`,
`experiments/results/phase9-workload-*.csv`

---

> **Follow-up:** the two gaps this report declared (scenario 8 and a dedicated
> retry storm) are now closed in
> [05-PHASE9-TESTS-A-AND-B.md](05-PHASE9-TESTS-A-AND-B.md).

## Headline

**16 fault injections across 4,067 workflows produced 0 orphans, 0 duplicate
journal records, 0 duplicate activity invocations and 0 stuck workflows.**

**R-4 is explained and is not a defect.** True failover to a different node takes
**29.9 s mean (sd 1.15 s)** against a 10 s lease TTL, because adopting another
node's shard deliberately waits `2 × LEASE_TTL` to avoid churn.

---

## 1. Recovery oracle

`experiments/scripts/oracle.sh` reads **durable state**, never engine metrics,
and keeps three counts separate as spec §2 requires:

| Count | Source |
|---|---|
| Journal records | rows in `events` |
| Activity invocations | `ActivityTaskScheduled` per `(workflow, run, k)` |
| **External side effects** | **NOT MEASURABLE — see Limitation L-1** |

### Recovery time is defined by four instants, all on the PostgreSQL clock

| Instant | Meaning |
|---|---|
| `t_inject` | the fault is applied |
| `t_expire` | every lease the victim held has passed `expires_at` |
| `t_takeover` | the survivor holds **all** those shards on a live lease with a **higher fencing token** |
| `t_progress` | the first event is committed on an affected shard after `t_inject` |

"Running again" is not recovered. `t_takeover` means ownership moved;
`t_progress` means useful work resumed. Both are reported.

---

## 2. Measured results

### 2.1 Graceful termination (`kubectl delete pod`, grace 45 s) — 3 trials

| Trial | `t_expire` | `t_takeover` | `t_progress` | Token |
|---|---|---|---|---|
| 1 | 754 ms | 5135 ms | 5135 ms | 6→7 |
| 2 | 689 ms | 6105 ms | 6105 ms | 7→8 |
| 3 | 2177 ms | 3912 ms | 3912 ms | 8→9 |

**Mean 5.05 s, max 6.11 s.** Leases are *explicitly released* on SIGTERM
(`t_expire` under 2.2 s, far below the 10 s TTL), so a rolling update never
waits out the TTL.

### 2.2 Frozen node — true failover, 3 trials (the clean experiment)

Injection: `docker pause` on the `engined` container inside the minikube node.
The process cannot renew **and cannot release**, and the container never exits,
so Kubernetes applies no restart backoff and no restart race occurs.

| Trial | `t_expire` | `t_takeover` | `t_progress` | Taken by | Stale tokens live | Orphans | Dup journal |
|---|---|---|---|---|---|---|---|
| 1 | 8312 ms | 28808 ms | 28808 ms | durexec-0 | **0** | **0** | **0** |
| 2 | 8522 ms | 29318 ms | 29318 ms | durexec-0 | **0** | **0** | **0** |
| 3 | 10187 ms | 31463 ms | 31463 ms | durexec-0 | **0** | **0** | **0** |

**Takeover: mean 29.86 s, sd 1.15 s, max 31.46 s.**
**Acquire phase (`t_takeover − t_expire`): 20.50 / 20.80 / 21.28 s.**

`t_progress` equalled `t_takeover` in every trial: once ownership moves, work
resumes in the same instant. There is no dead time after the claim.

### 2.3 Stale-owner fencing (scenario 7) — 3 trials

After takeover, the frozen node was thawed with `docker unpause`. It woke
holding fencing token *N* while the shards had advanced to *N+1*, and tried to
continue.

| Check | Result |
|---|---|
| Leases still live at the stale token | **0 / 0 / 0** |
| Duplicate journal records | **0 / 0 / 0** |
| Orphaned executions | **0 / 0 / 0** |

Fencing held in all three trials. This exercised the **real** commit-time
`verifyToken ... FOR UPDATE` path, not a simulated crash.

### 2.4 Correctness oracle over every fault run

| Workload | Started | Completed | Orphans | Dup journal | Dup invoke | Stuck |
|---|---|---|---|---|---|---|
| `loadgen-` (baseline) | 112 | 112 | 0 | 0 | 0 | 0 |
| `fault-abrupt-` | 65 | 65 | 0 | 0 | 0 | 0 |
| `ab3-` | 2418 | 2418 | 0 | 0 | 0 | 0 |
| `ab4-` | 1081 | 1075 | 0 | 0 | 0 | 0 |
| `frz-` | 391 | 387 | 0 | 0 | 0 | 0 |
| **Total** | **4067** | **4057** | **0** | **0** | **0** | **0** |

The 10 unaccounted workflows were **in flight at the moment of sampling**, not
stuck: each was 0–3 s old with exactly one pending task, and the generator was
still offering load. Cluster-wide terminal state afterwards: 4062 COMPLETED,
1 FAILED, 0 abandoned.

The single FAILED workflow is `k8s-smoke-1`, a **deliberately malformed input**
of mine (`amount_cents` absent). The engine refused it non-retryably, journalled
`ActivityTaskFailed{non_retryable:true}` then `WorkflowExecutionFailed`, and did
not enter a retry storm. That is correct behaviour, recorded rather than hidden.

---

## 3. Root cause of R-4 — SOLVED, and it is not a defect

The 20.9 s acquire phase is **not** lease arithmetic. It is an explicit
anti-churn guard at `cmd/engined/main.go:273`:

```go
Wants: func(s core.ShardID) bool {
    if ring.Lookup(shard.Key(s)) == cfg.NodeID { return true }
    l, err := leases.Get(ctx, s)
    if err != nil { return false }
    return time.Since(l.ExpiresAt) > 2*cfg.LeaseTTL
}
```

A node adopts a shard the hash ring assigns to **another** node only once that
lease has been expired for more than `2 × LEASE_TTL`. The code states the
reason: *"Waiting two TTLs first stops a brief GC pause or a rolling restart
from causing needless shard churn."*

### The arithmetic matches the measurement

| Term | Value |
|---|---|
| TTL remainder after the last renewal | 8.3 – 10.2 s (measured) |
| `2 × LEASE_TTL` orphan-adoption guard | 20 s (by design) |
| `Tick` (1 s) + `AcquireEvery` (5 s) phasing | 0 – 1.3 s (measured) |
| **Predicted total** | **~29 – 31 s** |
| **Measured total** | **28.8 / 29.3 / 31.5 s** |

So **failover to a different node is approximately `3 × LEASE_TTL`**, not
`1 × LEASE_TTL`. The earlier 29 s Docker observation was the same mechanism.

### Why recovery is bimodal in Kubernetes

The acquire SQL (`internal/lease/store.go:98`) is:

```sql
WHERE shard_id = $1
  AND (owner_node = $2 OR owner_node IS NULL OR expires_at < now())
```

`owner_node = $2` lets a node re-take **its own** lease with no wait, bumping
the token to invalidate its previous incarnation. Because a StatefulSet gives
**stable identity**, a restarted pod *is* the same `owner_node`:

| Path | Condition | Measured |
|---|---|---|
| **Fast** | victim restarts and reclaims its own shards | **2.95 s** |
| **Slow** | a different node must adopt them | **29.9 s** |

**`LEASE_TTL` alone cannot predict recovery time.** Which path occurs depends on
whether the pod restarts before the 2-TTL guard elapses.

### No timing change is recommended

Per spec §4, shortening `LEASE_TTL` would be wrong: it would shrink the
renewal safety margin (`leaseRenewSafetyFactor = 3`) and increase the risk of a
live node being fenced during a GC pause or a slow query. If faster failover is
wanted, the lever is the `2 × LEASE_TTL` adoption guard, and the cost is more
shard churn during rolling restarts — the exact behaviour the guard exists to
prevent. **That is a deliberate trade-off, not a bug to fix.**

---

## 4. Defects and invalid runs

### Defect F9-1 — `AfterLeaseRenew` is a dead crash point

`internal/faultinj/injector.go:51` declares `AfterLeaseRenew` with a comment
saying it is "used to force an" ownership change. **`ShouldCrash(AfterLeaseRenew)`
is never called in production code** — only in the injector's own unit test.
It can never fire in a running engine, and its unit test passes, which gives
false confidence that the capability exists.

Not fixed in this phase: adding a check inside the lease-renewal path is a
behavioural change in correctness-critical code, and the scenario it was meant
to serve (stale-owner fencing) was tested more convincingly by freezing a real
process (§2.3). Recommended fix is to **delete the constant** or implement it
deliberately; the choice should not be made inside a fault-injection phase.

### Gap F9-2 — crash points cannot be armed at runtime

`BeforeDecisionCommit` and `AfterActivityJournaled` **are** checked in
production code (`eventlog/commit.go:328`, `runtime/activity_worker.go:246`),
but `CrashAt` is reachable only from `app/agent/fixture.go`. `FAULT_INJECTION`
is a **bool**, not a point selector, and no admin route arms a crash point.

Consequence: spec scenarios **3, 4 and 5** (termination *at* workflow creation,
*at* decision commit, *between* journalling and acknowledgement) **were not
tested**, and are not claimed. Reaching them needs a small, double-gated
arming endpoint beside the existing `POST /v1/admin/fail-step`.

### Invalid and contaminated runs (preserved, per spec §12)

| Run | Status | Reason |
|---|---|---|
| `abrupt` trial 1 (first set) | **INVALID** | The image is distroless (no `/bin/sh`), so the in-container kill failed and it fell through to `kubectl delete --force`, which still sends SIGTERM. `t_expire = 2091 ms` proves a **graceful release**, not an abrupt failure. The claim probe also excluded the victim's own stable identity. |
| `abrupt` trials 1–2 (second set) | **VALID, LEASE TIMING ONLY** | `kubectl port-forward` binds to **one pod**; killing that pod killed the load path, so no workload ran and `t_progress` was unmeasurable. Fixed by adding a NodePort (`31-engine-nodeport.yaml`). |
| `abrupt` trials 2–5 (loaded set) | **CONTAMINATED** | Killing the *same* pod five times triggered Kubernetes **exponential restart backoff** (28 `BackOff` events on `durexec-1`). `t_claim` rose monotonically 19.3 → 21.9 → 27.5 → 30.1 s because it was gated by container restart, not by lease timers. Superseded by the frozen-node experiment, which cannot be contaminated this way. |

No run was deleted.

---

## 5. Scenario coverage — what was and was not tested

| # | Scenario | Status |
|---|---|---|
| 1 | Graceful engine-pod termination | **TESTED** — 3 trials |
| 2 | Abrupt engine-pod termination | **TESTED** — 3 clean frozen-node trials + 1 valid fast-path |
| 3 | Termination during workflow creation | **NOT TESTED** — no armable hook (F9-2) |
| 4 | Termination during decision commit | **NOT TESTED** — hook exists but is unarmable (F9-2) |
| 5 | Termination during activity result recording | **NOT TESTED** — hook exists but is unarmable (F9-2) |
| 6 | Task retry / timer failure | **COMPLETED** in [05-PHASE9-TESTS-A-AND-B.md](05-PHASE9-TESTS-A-AND-B.md) — dedicated retry storm reached 0 claimable tasks; poll cost constant to 100k rows |
| 7 | Stale-owner writes after lease transfer | **TESTED** — 3 trials, fencing held |
| 8 | Temporary PostgreSQL unavailability | **COMPLETED** in [05-PHASE9-TESTS-A-AND-B.md](05-PHASE9-TESTS-A-AND-B.md) — 4 trials, 2 failure shapes, all clean |
| 9 | Repeated failure / recovery cycles | **TESTED incidentally** — 16 injections, 5 consecutive on one pod; revealed the backoff artifact |
| 10 | Reconciler recovery of stuck workflows | **NOT TESTED** — workflow start is now a single transaction, so orphans could not be produced; **0 orphans in 4,067 workflows** is consistent with the F-01 fix holding |

---

## 6. Limitations

**L-1 — external side effects are not measurable in this deployment.**
`internal/extsim` is test-only; it is **not wired into `engined`**. Therefore
nothing here may be read as evidence of exactly-once *external* effects. Only
journal records and activity invocations were counted, and spec §2's warning
against conflating them is respected.

**L-2 — the host is 2 vCPU / 3.8 GiB.** minikube cannot resize an existing
profile. All recovery results here are **timing of coordination**, which is not
load-sensitive; no throughput figure from this host is publishable.

**L-3 — one victim pod.** `durexec-1` held 9 of 16 shards in every trial (the
ring split is deterministic), so failover was always measured for the
larger half. A 7-shard victim was not measured.

**L-4 — single-node cluster.** Pod failure was injected; **node** failure,
network partition and zone loss were not, because there is one node.

---

## 7. Reproduce

```bash
./experiments/scripts/lab.sh up
IP=$(minikube ip)                       # NodePort survives pod kills
go build -o /tmp/loadgen ./engine/cmd/loadgen
/tmp/loadgen -mode throughput -base http://$IP:30080 \
  -shards 16 -concurrency 6 -duration 1200s -prep 200ms -id-prefix frz- \
  -csv experiments/results/phase9-workload-frozen.csv &
sleep 30
./experiments/scripts/stale-owner.sh 3        # clean failover + fencing
./experiments/scripts/fault.sh graceful 3     # graceful termination
./experiments/scripts/oracle.sh frz-          # reconcile
```

---

## 8. Verdict

### Demonstrated

- Graceful shutdown releases leases explicitly; failover in **5.05 s mean**.
- True failover to another node: **29.9 s mean, 31.5 s max**, reproducible to
  sd 1.15 s, and fully explained by the `2 × LEASE_TTL` adoption guard.
- Progress resumes **in the same instant** ownership transfers.
- **Fencing holds.** A thawed stale owner wrote nothing: 0 stale live tokens,
  0 duplicate journal records, 0 orphans, across 3 trials.
- Fencing tokens increment monotonically on every transfer (observed 1 → 22).
- **0 orphans in 4,067 workflows** through 16 injections — the F-01 single-
  transaction start fix holds under real fault conditions.
- Non-retryable failures are journalled and do not cause retry storms.

### Not demonstrated

- Exactly-once **external** effects (L-1 — not measurable here).
- Crash **at** the decision-commit and activity-journal boundaries (F9-2).
- PostgreSQL outage recovery (scenario 8, not run).
- Reconciler orphan recovery (no orphans could be produced).
- Node failure, partition, multi-node behaviour (L-4).

### Proceed to the metrics phase?

**Yes.** No correctness defect was found in the paths that could be exercised,
and the one unexplained timing anomaly carried in from Phase 4 is now closed
with code-level evidence and a reproducible measurement.

Two items should be carried forward rather than dropped: **F9-2**, because
scenarios 3–5 are the boundaries durable execution exists to protect and they
remain unproven; and **L-1**, because wiring `extsim` into `engined` is the only
way this project can ever substantiate an exactly-once claim about the outside
world.

**This is not a production-readiness statement.** A successful Kubernetes
deployment and a clean fault run are not the same as production readiness, and
the launch blockers recorded in `docs/audit/` still stand.
