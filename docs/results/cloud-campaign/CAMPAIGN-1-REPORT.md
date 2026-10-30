# Cloud campaign 1 — Stages A–D (AWS)

**Date:** 2026-10-30, 12:17–~15:05 UTC · **Region:** us-east-1 (all compute in us-east-1a)
**Code and images:** commit `cf986fb43c636de85d2fa87fc9225dc6626e0cc2`, clean tree.
`durexec/engine@sha256:652717ed…97af`, `durexec/loadgen@sha256:8b0b8317…ad75`
(`images.txt`). Every engine pod was verified running exactly that engine digest.

Every number in this report is an **AWS observation** from the files under
`docs/results/cloud-campaign/` and `docs/results/cloud/2026103012*-campaign1-*`
/ `…143107Z-campaign1-final`. Nothing here is from Docker, Minikube or the laptop.

## Infrastructure

| Role | Instance | Notes |
|---|---|---|
| EKS 1.37 control plane | managed | public endpoint only `157.50.241.83/32`; private endpoint used |
| Engine nodes | 3 × m7i-flex.large (2 vCPU, 8 GiB) | one engine pod per node (anti-affinity) |
| Harness node | 1 × c7i-flex.large (2 vCPU, 4 GiB) | tainted; load generator and DB probe |
| Database | 1 × m7i-flex.large EC2, PostgreSQL 16 (pgvector image) | 30 GB gp3, `synchronous_commit=on`, **single host, no standby** |
| Operator | 1 × c7i-flex.large | in-VPC, SSM only, no inbound; ran the harness |

Run from a cloud session rather than the laptop: the harness ran on the in-VPC
operator against the cluster's private endpoint; the public allowlist was not
changed. Terraform state lived in `s3://durexec-tfstate-183192605828` (encrypted,
versioned, private). Plan: 44 to add, 0 to change, 0 to destroy.

Workload: `DeliveryOrder` (4 activities + one 50 ms durable timer), 3 replicas,
16 shards, 4 workflow × 8 activity workers per shard. Ownership **5/9/2**,
skew 1.69, on every point.

## Stage A — bring-up — PASS

Smoke 10/10; NetworkPolicy enforced and restored
(`networkpolicy_enforced=yes restored_after_delete=yes`); three pods on three
engine nodes; evidence bundle complete (25 artefacts).

## Stage B — idle baselines (no workload) — 15/15 valid

| Config | Idle commits/s (3 trials) | DB host CPU |
|---|---|---|
| S16 w2x4 | 164 / 165 / 166 | ~1.2 % |
| S16 w4x8 (pilot) | 251 / 252 / 251 | ~1.1 % |
| S16 w8x16 | 445 / 442 / 443 | ~2.2 % |
| S32 w2x4 | 304 / 304 / 302 | ~1.6 % |
| S64 w1x2 | 412 / 410 / 410 | ~1.8 % |

Background polling alone accounts for about half of the pilot's 477 commits/s.
It rises with workers and, at constant total workers (192), with shard count —
a per-shard cost. It is a fixed overhead to subtract in Stages F and G, not a
load the database struggles with.

## Stage C — calibration — PASS

| | trials | p50 | p95 | p99 | wf/s | completed |
|---|---|---|---|---|---|---|
| Closed loop c=1 | 3 | 2.61 / 2.21 / 2.21 s | 3.2–3.5 s | 4.3–4.4 s | 0.40–0.43 | 166–181 |
| Open loop 1.0/s | 3 | 2.41 / 2.41 / 2.31 s | — | 3.9–4.4 s | 0.99–1.00 | 300 |

**Latency floor (median c=1 p50): 2.21 s**, with engine and DB CPU near 3 %:
the floor is set by polling and timer scheduling, not by any resource. At
1.0/s offered the p50 stayed within 10 % of the floor (+4 % to +9 %) — the gate
passes. loadgen labelled two of these points `SATURATED`; its own threshold is
tighter than the plan's and nothing was near capacity, so the runner recorded
them `SATURATED_UNATTRIBUTED`, as designed.

Deviations from the sheet, for sample size only: c=1 ran 420 s measured
(300 s would give ~130 < 150 samples) and the low arrival rate was 1.0/s
(0.25 × c=1 throughput ≈ 0.11/s would give ~33 samples).

## Stage D — closed-loop concurrency ladder (r3/S16/w4x8)

| c | trials | wf/s | p50 | p99 | DB CPU | engine node max | gen CPU | pool empty | top DB waits |
|---|---|---|---|---|---|---|---|---|---|
| 4 | 1 | 1.62 | 2.41 s | 4.51 s | 4 % | 4 % | 0.02 | 0 | WALSync |
| 16 | 1 | 7.22 | 2.21 | 3.21 | 8 % | 6 % | 0.03 | 0 | WALSync, WALWrite |
| 32 | 1 | 16.72 | 1.91 | 2.81 | 13 % | 8 % | 0.06 | 0.0001 | WALSync |
| 64 | 1 | 36.75 | 1.80 | 2.50 | 22 % | 15 % | 0.12 | 0.0001 | WALWrite, WALSync |
| 128 | 3 | 67.03 / 66.06 / 66.44 (**66.5 ± 0.5**) | 1.9–2.0 | 2.61–2.71 | 41–52 % | ~22 % | 0.20 | 0.001 | WALWrite 39–46 % |
| **256** | 3 | 129.09 / 129.96 / 134.32 (**131.1 ± 2.8**) | 1.9–2.0 | **2.71–3.09** | 74–96 % | 38–42 % | 0.32 | 0.06–0.47 | WALWrite ~45 %, txid+tuple locks ~35 % |
| 512 | 3 | 150.97 / 107.12 / 113.08 (123.7 ± 23.8) | 3.1–4.1 | 8.8–14.0 | **~96 %** | 58–63 % | 0.51 | **~0.60** | WALWrite ~46 %, txid+tuple ~30 % |

Every point ≥ 1,000 completions except c ≤ 16 screens; generator ≤ 0.51 of the
1.6-core saturation threshold everywhere; all verdicts `RECORDED`.

**Findings (this configuration, this hardware):**

1. **Sustainable throughput ≈ 131 workflows/s at c=256** (3 trials, sd 2.8),
   p99 ≤ 3.1 s. Up to there throughput scales near-linearly with concurrency
   and latency stays at the ~2 s floor: the system is latency-bound, not
   capacity-bound, below c≈256.
2. **c=512 is past the knee.** Mean throughput is *lower* (124, unstable from
   151 to 107) while p99 rises 3–5×. The ladder driver named c=512 the knee
   from its first screen (151 > 129); the repeat trials overturn that. This
   report uses c=256.
3. **The limiting resource is the single database host's CPU** (2 vCPU),
   ~96 % at c=512 and up to 96 % at c=256, with WAL-write contention and
   transaction-ID/tuple lock waits dominating its wait profile. ~100 commits
   per completed workflow at the knee (including the ~251/s idle baseline).
4. **The engines' connection pools saturate at the same point** (empty
   6–47 % of acquisitions at c=256, ~60 % at c=512; peak 65 connections =
   3 × (20 + lease)). Pool queueing and DB CPU rise together, so this stage
   cannot say which binds first; engine nodes stayed ≤ 63 % CPU.
5. **Shard imbalance (5/9/2) did not limit this configuration**: the binding
   resource is shared (DB), not the busiest engine.

## Correctness

Journal invariants after every Stage C/D point — about 140,000 runs in total
— found **0** duplicate `(run_id, seq)`, duplicate activity completions,
orphan completions, sequence gaps or multiple terminal events; every run in
every point ended `COMPLETED`. This is evidence for these runs under load
without faults; it is not a fault-tolerance result.

## Not established by this session

* Engine scaling with replicas (Stage H), worker and shard sensitivity
  (F, G), the open-loop curve (E), and any fault recovery.
* Anything about a larger or replicated database: the knee is this DB host's.
* Variance of flex instances over time (one session, one AZ).
* No HA, exactly-once arbitrary external effects or production readiness.

## Teardown

In-cluster objects were deleted from the operator (no PersistentVolumes), then
`terraform destroy` ran from the cloud session. It **failed once**: a VPC-CNI
secondary network interface (`aws-K8S-i-0d13b0c0989b0d3ed`, unattached, from a
terminated engine node) and the EKS-created cluster security group it
referenced blocked one subnet. Both were verified as this pilot's own (pilot
VPC tagged `Project=durexec`, node and group tagged `durexec-bench`), deleted,
and a second destroy removed the subnet and VPC. Terraform state: 0 resources.

The failure exposed a script defect, fixed: `set -e` (from `_common.sh`) ended
`eks_teardown.sh` at the failed destroy, so its billing sweep never ran. The
destroy's exit code is now captured and the sweep always runs (tested with a
failing destroy: 12 sweep calls, exit 1).

Read-only sweep after cleanup, 15:07 UTC: no EKS cluster, no pilot EC2, no
EBS volumes or snapshots, no load balancers, no secrets, no log groups, no
pilot VPC or network interfaces, no `durexec-` IAM roles.

**Remaining, deliberately:** ECR `durexec/{engine,loadgen,extsimd}` (tag
`cf986fb`, reused by sessions 2–3, ~$0.01/month) and the S3 bucket
`durexec-tfstate-183192605828` (evidence and empty state). The Elastic IP
`3.82.134.71` is tagged `churn-ml-system`/`prod` — unrelated, untouched.

## Cost and duration

Infrastructure up from 12:17 UTC (apply) to ~15:05 UTC (compute destroyed),
about **2 h 50 min**. At the ~$0.75/h estimate (including the operator) that
is **about $2.10** — an estimate, not billing data. Cap for this session: $5.

