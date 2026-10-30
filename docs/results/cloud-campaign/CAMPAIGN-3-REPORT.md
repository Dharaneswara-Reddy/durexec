# Cloud campaign 3 — Stage H and fault recovery (AWS)

**Date:** 2026-10-30, 19:16–~22:15 UTC, on the same infrastructure as campaign 2
(kept up between the two sessions). **Images:** `cf986fb` (engine
`sha256:652717ed…`). **Harness:** `4dcdbaa` → `0a34390` → `62264f7`.
Every number is an **AWS observation** from `docs/results/cloud-campaign/`.

## Stage H — engine replicas 1, 2, 3 (S16/w4x8, same three engine nodes)

| Replicas | wf/s at c=256 (3 trials) | p99 at c=256 | wf/s at c=512 (3 trials) | p99 at c=512 | DB CPU at c=512 | pool empty at c=512 |
|---|---|---|---|---|---|---|
| 1 | **130.4** (130.4 / 131.0 / 129.9) | 2.7–3.0 s | 140.3 (130.7 / 145.1 / 144.9) | 6.5–7.6 s | ~88 % | ~97 % |
| 2 | **128.2** (129.0 / 132.4 / 123.2) | 2.6–3.7 s | 121.6 (142.8 / 107.5 / 114.5) | 9.1–13.8 s | ~96 % | ~84 % |
| 3 (Stage D) | **131.1** (129.1 / 130.0 / 134.3) | 2.7–3.1 s | 123.7 (151.0 / 107.1 / 113.1) | 8.8–14.0 s | ~96 % | ~60 % |

**Replica scaling 1→3 ≈ 1.0× on this deployment.** The single 2-vCPU
PostgreSQL host is the shared bottleneck, so more engines share the same
database. One replica is slightly *better* past the knee: its single 20-
connection pool caps how many transactions reach the database (97 % of
acquisitions found it empty), keeping DB CPU and lock contention lower. This
is **not** a statement about Durexec's horizontal scalability in general; that
needs a database that is not the limit.

## Fault campaign — r3/S16/w4x8, 65.6 wf/s open-loop load (0.5 × knee, sustained in Stage E)

Times in ms from injection on the PostgreSQL clock (`t_expire` the victim's
leases expired or moved, `t_claim` all re-held with a higher fencing token,
`t_progress` first new event on those shards, `t_ring` all pods Ready and no
shard unowned).

| Fault | Trial: victim (shards) | t_expire | t_claim | t_progress | t_ring | load done / errored | p99 | Verdict |
|---|---|---|---|---|---|---|---|---|
| graceful pod delete | 1: d-1 (9) · 2: d-2 (2) · 3: d-0 (5) | 4,117 / 1,410 / 4,140 | 4,117 / 5,907 / 4,140 | 11 / 13 / 0 | 31.9 / 32.3 / 32.0 s | 11,480 / 0 · 11,480 / 0 · 11,479 / 1 | 4.4–5.8 s | ok ×3 |
| SIGKILL engined | 1: d-1 (9) · 2: d-2 (2) · 3: d-0 (5) | 5,310 / 4,662 / 4,735 | same | 0 / 14 / 5 | 34.6–36.8 s | 11,479 / 1 · 11,480 / 0 · 11,480 / 0 | 2.2–3.3 s | ok ×3 |
| freeze 35 s (> 10 s TTL) | 1: d-1 (9) · 2: d-2 (2) · 3: d-0 (5) | 12,580 / 12,199 / 12,823 | 47,699 / 32,288 / 34,479 | 2 / 24 / 0 | 51.2–55.2 s | 9,377 / 864 · 10,537 / 118 · 10,246 / 147 | 31.9–46.2 s | ok ×3 |
| DB connection sever | 1: d-1 (9) · 2: d-2 (2) · 3: d-0 (5) | no lease moved | — | 3 / 6 / 2 | ring never broken (see note) | 11,480 / 0 ×3 | 2.2 s | ok ×3 |
| node drain | 1: d-1 (9) · 2: d-2 (2) · 3: d-0 (5) | 4,190 / 8,328 / 8,273 | 23,078 / 23,689 / 23,850 | 8 / 13 / 8 | 38.5 / 47.8 / 49.7 s | 10,742 / 736 · 11,302 / 176 · 11,039 / 431 | 4.8–6.7 s | ok ×3 |
| DB host reboot (EC2) | 1: all 16 shards | 22,420 | 33,195 | 1 | 35.2 s | 9,991 / 1,365 | 5.2 s | ok |

1. **Safety held in every trial.** All 16 valid trials: 0 duplicate
   `(run_id, seq)`, duplicate activity completion, sequence gap or multiple
   terminal event; every accepted run ended `COMPLETED` after drain.
2. **Fencing works on the real cluster.** After each freeze the thawed stale
   owner's writes were **rejected by fencing 22 / 4 / 5 times** — the node
   that woke up believing it still owned its shards was refused, not obeyed.
3. **Recovery times.** Graceful delete and SIGKILL have all of the victim's
   shards re-held with a higher token in 4.1–5.9 s; a frozen
   owner's leases expire at ~12.2–12.8 s (TTL 10 s plus renewal slack) and are
   re-claimed by 32–48 s, consistent with the lab's 2×TTL anti-churn guard; a drained
   node's shards are re-owned in ~23 s. Progress on the affected shards
   resumes within 0–24 ms of injection in every trial.
4. **The cost is availability, not correctness.** Freeze, drain and the DB
   reboot produced load errors (118–1,365 starts refused while shards were
   unowned) and, for freeze, p99 of 32–46 s. Refused starts were never
   accepted, so nothing was lost.
5. **DB host reboot:** one EC2 reboot of the single PostgreSQL host; leases
   expired (22.4 s), all 16 shards re-claimed (33.2 s), ring complete at
   35.2 s, every accepted run completed, invariants 0. One trial only, and on a
   single database host — **not** an HA result.

Note on DB connection sever: no lease is expected to move, so the oracle's
claim wait ran its full 150 s and the reported `t_ring` (~152 s) measured
that wait, not an outage; progress resumed within 2–6 ms. Fixed in the
harness (`0c9f191`) for future runs.

### Engine finding (open, not fixed)

During drain trial 3, `durexec-0` restarted and its **boot-time migration
failed with `deadlock detected (SQLSTATE 40P01)`** against the other engines'
live traffic; the process exited and Kubernetes restarted it (restart count
1). The trial still recovered and passed, but a migration that can deadlock
with live traffic turns every rolling restart under load into a possible
crash-and-retry. Evidence: `raw/20261030T213700Z-fault-drain-t3/51-victim-previous.log`.

## Harness defects found and fixed during the session

| Defect | Effect | Fix |
|---|---|---|
| `eks_fault.sh` checked targets with `kubectl get pod NAME -l …` | kubectl refuses name+selector: no pod fault could be injected | by name, then the `app` label (self-test with a kubectl stub) |
| `eks_fault_trial.sh` left its load Job running when the injector failed | following trials' loads stacked (~200 wf/s), overloading the cluster | Job deleted on every exit; failure recorded as `INVALID_injection_failed` |
| busybox `pgrep -x engined` never matched `/engined` | every kill/freeze failed | `pidof engined` (verified on an engine node) |
| drain cleanup resolved the node through the evicted pod | node stayed cordoned; pod Pending; later trials refused | uncordon the node recorded before injection |
| oracle waited out MAX_WAIT for sever-db | inflated `t_ring` | leave the loop on progress + heal for no-lease-move scenarios |

All 21 INVALID rows from the broken-injector attempts are kept in
`faults.csv` as recorded. Every injection that ran was scoped to this pilot
(engine pods/nodes labelled `durexec.io/role=engine`, the DB instance tagged
`Project=durexec`).

## Not established

Horizontal scaling with a non-bottleneck database; HA of any kind (one DB
host, no standby); exactly-once arbitrary external effects; behaviour under
the untested scenarios (netem, NetworkPolicy partition, EC2 node termination,
rolling restart); variance across days or availability zones.

## Teardown and cost

In-cluster objects deleted from the operator (no PersistentVolumes), then
`terraform destroy` from the cloud session: **44 destroyed**, no error (no
leaked network interface this time). Terraform state: 0 resources.

Read-only sweep, 21:58 UTC: no EKS cluster, no pilot EC2, no EBS volumes or
snapshots, no load balancers, no secrets, no log groups, no pilot VPC or
network interfaces; no `durexec-` IAM roles or instance profiles. Running
instances in the account belong to CodeArena and churn-ml-system — untouched.

**Remaining, deliberately:** ECR `durexec/{engine,loadgen,extsimd}` (tag
`cf986fb`, ~$0.01/month) and the S3 bucket `durexec-tfstate-183192605828`
(evidence, collector archives, empty state). Elastic IP `3.82.134.71` is
`churn-ml-system`/`prod` — unrelated, untouched.

**Duration and cost** (estimates at ~$0.75/h, not billing data):

| Session | Infrastructure up | Estimate |
|---|---|---|
| 1 (Stages A–D) | 12:17–15:05 UTC, 2 h 50 min | ~$2.10 |
| 2 (Stages E–G) | 15:21–19:16 UTC, 3 h 55 min | ~$2.95 |
| 3 (Stage H, faults) | 19:16–21:58 UTC, 2 h 42 min | ~$2.00 |
| **Campaign total** | **9 h 27 min** | **~$7.05** |
