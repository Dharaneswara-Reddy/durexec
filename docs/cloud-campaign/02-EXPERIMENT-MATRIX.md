# Cloud experimental matrix — revised after the first AWS pilot

> **Revision note (2026-10-30):** This execution matrix supersedes the
> unexecuted proposal retained at the end of this file. It incorporates the
> real pilot evidence: r3/S16/w4x8 completed 232 workflows in a 140 s window
> (1.66/s; p50/p95/p99 2.306/3.503/4.505 s), while the earlier 72-completion
> point was under-sampled. The pilot's 477.4 PostgreSQL commits/s is not an
> idle baseline. Source inspection finds 16 × (4 workflow + 8 activity)
> pollers, with idle poll delay backing off from 10 ms to 1 s; queue polls
> issue database queries and found work resets the delay. This makes polling a
> credible transaction source, but the mixed pilot also includes workflow
> writes, lease renewals and probes, so the fraction attributable to idle
> polling is **unknown** until Stage B.

## Current campaign protocol

Common fixed setup: three engine nodes (`m7i-flex.large`), one load-generator
node (`c7i-flex.large`), one PostgreSQL `m7i-flex.large`, one AZ, gp3, and
`synchronous_commit=on` unless a separately approved experiment changes it.
Record actual instance type and AZ for each node at every point. Keep flex
types fixed throughout a campaign; host scheduling variance remains a caveat.
Each trial gets a unique evidence directory. No point is averaged in if the
generator saturates, it is under-sampled, or any correctness invariant fails.

For load trials, default warm-up is 15 s and measured duration is specified
below. Capture accepted/completed workflows, offered/completed throughput,
p50/p95/p99, errors, transport failures, engine and generator CPU/memory,
node placement/restarts, PostgreSQL commits/transactions/connections/locks/
CPU/I/O/pool waits, shard ownership and skew, event-log invariants, and raw
logs/metrics/CSV. Per-point PASS requires complete evidence, >=1,000 completions
where required, no missing/duplicate/invalid event history, and load-generator
headroom (<85% of one core, <85% memory). FAIL is an invariant violation or
deployment/security check failure. INCONCLUSIVE is under-sampled, missing
metrics, or generator-saturated; reject it from comparisons. Stop the stage on
any invariant/security failure, sustained generator saturation, unexpected
resource change, or projected session cost reaching the approved cap.

| Stage | Exact configuration and method | Repeats / validity | Estimate |
|---|---|---|---:|
| A Bring-up | Pilot settings r3/S16/w4x8, sync on; publish, apply, deploy, smoke and `verify-policy`; record image digests and node types | One; all smoke/security checks pass | 0.8 h / $0.52 |
| B Idle baseline | For each later configuration, no load for 120 s after 15 s settling; sample every 3 s. Require no active jobs and unchanged execution count. Measure PG commits/s, DB CPU, engine CPU, DB connections/locks/I/O | 3 runs/config: r3 S16 w2x4,w4x8,w8x16; r3 S32 w2x4; r3 S64 w1x2; r1/r2 variants of selected Stage G config. Keep every raw sample | 2.2 h / $1.43 |
| C Calibration | r3/S16/w4x8; closed-loop c=1, then open-loop at 0.25× its measured completed rate; each 300 s measured after 15 s warm-up | 3 each. Floor = median c1 p50; low-rate p50 must be within 10%; generator headroom as common rule | 0.8 h / $0.52 |
| D Closed-loop ladder | r3/S16/w4x8; c=4,16,32,64,128,256,512; 120 s measured after 15 s warm-up. Stop ladder when completed throughput falls, p99 >10× C floor, or generator saturates | 3 valid trials total at the knee and each adjacent step, each with >=1,000 completions. A screen counts only if it reaches that sample minimum and passes all validity gates; otherwise repeat | 2.5 h / $1.63 |
| E Open-loop arrival | r3/S16/w4x8; rates 0.5,0.75,0.9,1.0,1.1 × D closed-loop knee throughput; keep offered rate independent of completions | 3 per rate, >=1,000 completions per point; max 1,800 s then INCONCLUSIVE | 4.0 h / $2.60 |
| F Worker sensitivity | At selected knee, r3/S16; change only workers: w2x4, w4x8, w8x16 | Stage B idle baseline each; bracket each knee then 3 trials at it, >=1,000 completions | 2.0 h / $1.30 |
| G Shard sensitivity | r3, total configured workers held at 192: S16/w4x8, S32/w2x4, S64/w1x2. Record measured ownership and `shard_skew`; ring expectations 1.69/1.31/1.22 respectively | Fresh initialized DB when shard count changes; bracket each knee then 3 knee trials, >=1,000 completions | 2.0 h / $1.30 |
| H Replica sensitivity | Same three engine nodes; `ENGINE_REPLICAS=1,2,3`; choose S/workers from G and hold them fixed within each replica comparison | 3 trials at each configuration's knee, >=1,000 completions; Stage B idle values available | 3.0 h / $1.95 |

Stage durations include warm-up, scale/deploy transitions, evidence capture and
review. The point durations above are measurement windows, not guarantees of
the stage estimate. At the knee, extend trial duration to
`ceil(1000 / expected_completed_wf_per_s)` seconds plus 15 s warm-up; do not
shorten to fit a time estimate. If this pushes projected spend toward the cap,
stop and seek approval for a smaller campaign. PASS supports only observed
behavior at the sampled configuration and duration; these stages do not prove
scaling beyond tested points, multi-AZ/database HA, arbitrary exactly-once
external effects, or production readiness.

### Separate fault campaign (not included in A–H)

Use the selected E knee configuration and steady open-loop load at 50% of
that knee. For graceful pod deletion, pod SIGKILL, engine node drain, DB
connection sever, and one DB host reboot: 30 s warm-up, 300 s load window,
3–5 trials per fault (start at 3; extend to 5 if recovery-time CV >10%).
Record injected/observed timestamps, recovery time, offered/completed rate,
full metrics and the complete event-log invariant suite after every trial.
PASS requires all invariants and eventual recovery; any invariant violation is
FAIL; missing recovery or samples is INCONCLUSIVE. Estimate 3.5 h / $2.28.
This is single-instance recovery evidence only; it cannot establish DB HA or
exactly-once arbitrary external effects.

### Estimated time and cost

| Work | Hours | Cost at $0.65/h |
|---|---:|---:|
| A–H | 17.3 | $11.25 |
| Teardown / final verification | 0.5 | $0.33 |
| A–H plus teardown | 17.8 | $11.57 |
| Optional separate fault campaign | 3.5 | $2.28 |

These are planning estimates, not billing caps. The previous $10 authorization
was for the completed first session and does not authorize this campaign.

---

## Previous matrix proposal (retained for history; superseded above)

The material below is the original pre-pilot proposal. Its status, estimates,
duration assumptions and stage ordering are historical and must not be used
as the current execution protocol.

# Cloud experimental matrix — derived from the evidence

Status: **designed, NOT RUN.** No AWS resource has been created. This matrix
is what the campaign will execute once credentials and a spending cap exist
(see `README.md`). Every number quoted as "previous" is from the laptop or
the 2-vCPU lab and is cited in `01-EVIDENCE-INVENTORY.md`.

The matrix is built to answer questions, not to fill a grid. Each experiment
names the hypothesis it tests, the outcome that would support or refute it,
and the measurement that decides between them.

---

## 1. What the evidence says, and what it leaves open

Four facts from the existing evidence shape everything below.

1. **Shard count and worker count were never separated.** `WORKFLOW_WORKERS`
   and `ACTIVITY_WORKERS` are goroutines *per shard*
   (`internal/runtime/pool.go:49`). Driving 16 shards instead of 1 also ran
   16× the workers. Phase 6 then showed that workers alone, at one shard
   (4 → 16), gave **1.95×**. So the local "9× from 1 to 16 shards" may be
   mostly a worker-count effect.
2. **Every fenced commit holds an exclusive lock on its shard's lease row
   until the transaction ends** (`internal/eventlog/commit.go:38-42`). With
   `synchronous_commit=on` that includes the WAL flush. A shard's commits are
   therefore serialized. Per-shard throughput is capped near
   1 / (commit duration), and that is an alternative explanation for shard
   scaling which predicts something different from (1).
3. **The closed-loop generator measures latency, not capacity, below the
   knee.** Closed-loop throughput = concurrency ÷ latency. The c16 trap
   (8.5 wf/s = 16 ÷ 1.9 s) and the c128 → c256 jump (2.74× with p99
   *falling*) are both this effect. Part of the latency is engine design
   rather than load: durable timers are scanned once a second
   (`internal/timers/scanner.go:37`) and idle workers back off to 1 s
   (`internal/taskq/poller.go:22`). The knee must be located open-loop.
4. **Overload collapses rather than plateaus**, through the 20-connection
   per-node pool (`cmd/engined/main.go:597`). On the lab, 20 offered gave
   **4.0 wf/s** with 15 120 s of cumulative pool wait. The laptop c512 point
   collapsed the same way, but could not be attributed.

Two infrastructure facts change what the cloud can bound:

5. The DB data volume is **gp3 with no provisioned IOPS: 3 000 IOPS and
   125 MB/s** (`deploy/aws/eks/database.tf:72`). In the harness check on
   2026-10-30, about 47 write I/Os per completed workflow occurred at low
   concurrency (local NVMe; not a result). Group commit should lower this
   under load, but the ceiling is real and the laptop never had one.
6. Everything runs in **one AZ** (`public[0]`), so there is no cross-AZ
   latency confound, and AZ failure is not testable.

## 2. Hypotheses under test

| # | Hypothesis | Predicts | Decided by |
|---|---|---|---|
| H1 | Throughput is set by **total worker concurrency** (shards × workers per shard) | Equal knees at equal total workers, whatever the shard count | E1 constant-total arm |
| H2 | Throughput is set by **per-shard commit serialization** through the lease-row lock | Knee ∝ shard count at constant total workers; `Lock` waits and commit latency rise at the knee; `SYNC_COMMIT=off` raises the knee | E1, E3, wait-event profile |
| H3 | Sub-knee latency is dominated by **timer-scan and idle-poll lateness**, not work | timer_lag p50 ≈ 0.5 s on any hardware; the floor is roughly the same in cloud and laptop | C0 decomposition |
| H4 | Past the knee the **pool collapses** throughput | `pool_empty_frac` jumps and completed/s *falls* at 1.5× knee | E4 |
| H5 | **Ownership skew** sets the cluster knee (the busiest node saturates first) | Per-pod CPU ∝ owned shards; hottest node at ≥ 85 % at the knee while others are not | every point (CPU per pod + `/v1/shards`) |
| H6 | The **shared database** is the eventual ceiling | At the best configuration's knee, DB CPU or disk ≥ 85 %, and adding engine nodes stops helping | E2, attribution |
| H7 | The **EBS IOPS limit** binds before DB CPU | `dbhost_disk_write_iops` → ~3 000 and `util` → 100 % at the knee; `SYNC_COMMIT=off` relieves it | E2/E3 attribution |
| H8 | The **generator** stays below saturation on a c7i.large at the rates needed | gen cores < 1.6 at every valid point | every point; C1 sets the ceiling |

## 3. Methodology rules (apply to every point)

- **Open loop for capacity.** The knee comes from `MODE=arrival` points.
  Closed-loop points are kept only to compare with the local runs.
- **A knee is attributed or it is not a knee.** Capacity = completed wf/s at
  the highest offered rate whose loadgen verdict is `SATURATED`, not
  `OVERLOADED`, and whose row names at least one resource ≥ 85 % (`saturated`
  column). `SATURATED_UNATTRIBUTED` triggers a follow-up, not a claim.
- **Windows: 60 s measured after a 10 s warm-up**, plus loadgen's 30 s grace.
  60 s gives ≥ 1 200 completions at ≥ 20 wf/s, enough for a p99 that is not
  just the worst sample (loadgen notes when it is). Calibration points at low
  rates use 120 s. One 600 s soak covers at least one checkpoint
  (`checkpoint_timeout` 5 min) and tests drift.
- **Repeats.** 3 trials at every knee and every baseline point. If the
  coefficient of variation of completed wf/s is above 10 %, run 2 more and
  report all five. Report mean ± sd and every trial.
- **Drift control.** Each configuration block starts and ends with the same
  anchor point (0.8 × that block's knee). A difference above 10 % between the
  two anchors marks the block as drifting; it is reported, not averaged away.
- **Fresh database per shard configuration.** `RESET_DB` on every shard-count
  change. This also tests fresh initialization and migrations each time. The
  journal grows within a block; `g_db_size_bytes` is recorded per point.
- **Validity first.** Rows that are `INVALID*`, `GENERATOR_SATURATED*`,
  `INCONCLUSIVE*`, `CORRECTNESS_*` or `RECORDED_PARTIAL` are kept in the
  dataset and excluded from every capacity figure, with the reason.

## 4. Stages

Durations are wall-clock estimates including deploys, probes and the 30 s grace.

### G — deployment validation (gates; nothing else runs until all pass)

| Step | What | Pass condition | ≈ min |
|---|---|---|---|
| G0 | Preflight: identity, region, spending cap confirmed by the owner, `terraform plan` = 28 add / 0 change / 0 destroy, clean tree | all true | 5 |
| G1 | `eks_publish.sh` | 3 images linux/amd64; digests in `images.txt` | 8 |
| G2 | `terraform apply` | 3 engine + 1 harness nodes Ready; DB bootstrap marker | 15 |
| G3 | `eks_deploy.sh` (r3, S16, 4×8, sync on) + `eks_smoke.sh` | S1–S9 all PASS | 6 |
| G4 | `eks_fault.sh verify-policy` | `networkpolicy_enforced=yes` (else F6 is NOT TESTABLE) | 1 |
| G5 | Pipeline: `KIND=smoke CONC=32 DUR=30s` | `SMOKE_ok`, every invariant 0, no `missing` | 3 |
| G6 | `collect_eks_evidence.sh g-validation` | `COMPLETE` | 3 |

### C — calibration

| Id | Config | Points | Question |
|---|---|---|---|
| C0 | r3 S16 4×8 | arrival 1 wf/s, 120 s, ×3 | The **service floor** on this hardware (median p50), and the latency decomposition at no load (H3). Every later `FLOOR` is this figure ×1.0, re-measured if a config changes worker counts |
| C1 | same | derived from B2 | Generator cost per wf/s (cores ÷ wf/s). Sets the highest rate one generator can offer below 1.6 cores. If needed rates exceed it, stop and decide (harness instance type is a Terraform variable; changing it is an infrastructure change for the owner to approve) |

### B — cloud baseline, comparable with the laptop

| Id | Config | Points | Question |
|---|---|---|---|
| B1 | r3 S16 4×8 | closed loop c128 and c256, **25 s** windows (the laptop's), ×3 each | Direct comparison with 47.15 / 129.30 wf/s. Read with §7 caveats |
| B2 | same | open loop: coarse 10, 20, 40, 80, 160, 320 wf/s until `SATURATED`/`OVERLOADED`; then 3 rates between the last NOT_SATURATED and the first SATURATED; 3 trials at the knee | Baseline knee, attributed |

### E1 — shards versus workers (the central experiment)

All r3, sync on. Workers per shard are `WORKFLOW×ACTIVITY`.

| Id | Shards | Workers / shard | Total wf workers | Arm |
|---|---|---|---|---|
| E1.a | 8 | 8×16 | 64 | constant total |
| E1.b | 16 | 4×8 | 64 | constant total (= B2, not re-run) |
| E1.c | 32 | 2×4 | 64 | constant total |
| E1.d | 8 | 4×8 | 32 | constant per-shard |
| E1.e | 32 | 4×8 | 128 | constant per-shard |

Each: anchor, knee search seeded from B2 (×0.5, ×1, ×1.5, ×2, then refine),
3 trials at the knee, anchor.

| Outcome | Reading |
|---|---|
| a ≈ b ≈ c, and d < b < e | **H1**: concurrency is what matters; shards are a partitioning unit only |
| a < b < c with total workers fixed | **H2**: per-shard serialization; confirm with Lock waits and commit latency at the knee |
| all equal | neither: look at the attributed resource (DB, disk, CPU) |
| the knee rises with total workers but `pool_empty_frac` > 5 % | the 20-connection pool is the next constraint (Phase 6 saw it at w16) |

S = 8 / 16 / 32 is not divisible by 3, so ownership is uneven at every point.
That is recorded per point (H5) and used, not hidden.

### E2 — engine-node scaling

Best E1 configuration, re-deployed with S = 24 (divisible by 1, 2, 3) if
E1's best was 16 or 32. r1, r2, r3. Knee each, 3 trials.

> **Correction (2026-10-30 verification):** divisibility does not make ownership
> even. The ring is a consistent hash; r3 with S = 24 gives 8/11/5 (busiest node
> 1.38× its fair share). Read each point's `shards_per_owner` and `shard_skew`
> columns and report the ratio against them. See
> `docs/results/local-shard-sweep/CORRECTED-ANALYSIS.md` §3.

**Question:** the scaling ratio r1→r2→r3, and **where it stops**. If the
knee stops rising while DB CPU or disk reaches ≥ 85 %, the finding is "the
engine scales until the shared database saturates at N wf/s on this DB
host" (H6/H7). That is a stronger claim than a bare ratio.

### E3 — cost of durability

Best configuration, `SYNC_COMMIT=off` (per engine connection; the server
setting is unchanged). Knee, 3 trials.

| Outcome | Reading |
|---|---|
| Knee rises a lot and disk write IOPS fall | the WAL flush is on the critical path: H2's serialization is fsync-bound, or H7 (IOPS) |
| Knee unchanged | the commit path is not the limit |

This measures what durability costs. It is **not** a recommended setting:
the engine's guarantee depends on `on`.

### E4 — overload and recovery

Best configuration. Open loop at **1.5 × knee for 120 s**, then immediately
**0.5 × knee for 120 s**. ×2.

**Questions:** does throughput collapse (H4) or plateau? Do the empty-acquire
fraction and pool wait explain it? Does the backlog drain afterwards, and
how long does that take (executions RUNNING over time; `drained` check)?
Correctness invariants must still be 0. The laptop's c512 point showed
degradation without failures; this tests that on attributable hardware.

### E5 — soak

Best configuration, **0.7 × knee for 600 s**, once. Drift in p99, RSS and
goroutines (leak check), checkpoint effects (`ckpt_*`), throughput against
journal size.

### F — faults under load (best config, 0.6 × knee open loop, 180 s load)

Every trial uses `eks_fault_trial.sh`: Phase 9 oracle on the PG clock,
victims rotated, cool-down between trials, CrashLoopBackOff = CONTAMINATED,
invariants on that trial's workflows after a drain.

| Id | Scenario | Trials | Question | Previous (lab) |
|---|---|---|---|---|
| F1 | `delete-graceful` | 3 | Lease release on SIGTERM | 5.05 s mean |
| F2 | `delete-force` (grace 0) | 3 | Lease **expiry** recovery when nothing is released | contaminated in Phase 9 |
| F3 | `kill` (SIGKILL engined, same pod restarts) | 3 | Process crash, restart, replay; restart vs adoption race | — |
| F4 | `freeze` 35 s (> TTL) | 3 | True failover, plus **stale-owner fencing** after thaw (`fencing_rejections` of the victim) | takeover 29.86 s (sd 1.15) |
| F5 | `netem delay 200ms 50ms loss 20%`, DB traffic only, 30 s | 2 | Degraded engine→DB link: renewals fail? ownership flaps? | — |
| F6 | `partition` (NetworkPolicy egress deny to DB), 30 s | 2 | A node cut off from the DB loses its leases; no split-brain writes | — |
| F7 | `sever-db` (terminate that pod's backends) | 2 | Connection loss and pool re-establishment | 10–13 ms (whole-DB outage) |
| F8 | `drain` an engine node | 1 | Pod cannot reschedule (required anti-affinity, 3 pods on 3 nodes): survivors adopt; uncordon returns it | — |
| F9 | `terminate-node` (EC2) | 1 | Hard host loss; node-group replacement; time until the ring is complete | — |
| F10 | `reboot-db` (EC2 reboot of the DB host) | 1 | DB host restart; **persistence across DB restart**; recovery of in-flight work | 25 s restart cost ~8 s (lab) |
| F11 | `kubectl rollout restart` under load | 1 | Rolling deployment behaviour | — |

### Closing

`collect_eks_evidence.sh` after every stage and **before teardown**, then
`eks_teardown.sh`, the read-only sweep, and Cost Explorer.

## 5. Budget

| Block | ≈ min |
|---|---|
| G0–G6 | 41 |
| C0 + B1 + B2 | 6 + 10 + 30 = 46 |
| E1 (4 new configs × ~35 min incl. reset/deploy) | 140 |
| E2 (2–3 deploys) | 75 |
| E3 | 25 |
| E4 + E5 | 10 + 13 = 23 |
| F1–F11 (~25 trials × ~6 min) | 150 |
| Evidence, teardown, sweep | 25 |
| **Total** | **≈ 525 min ≈ 8.8 h** |

At the runbook's **$0.63/h** that is **≈ $5.50**, or **≈ $7.70 with 40 %
contingency**. That is above the runbook's $5 ceiling. Options for the
owner, best first:

| Plan | Contents | ≈ hours | ≈ cost (+40 %) |
|---|---|---|---|
| **Core** | G, C, B, E1, E3, F1–F4, F7, F10 | 5.5 | $3.50 ($4.90) |
| Full | everything above | 8.8 | $5.50 ($7.70) |
| Two sessions | Core, then E2/E4/E5/F5–F11 after reading the results | 5.5 + 4 | $6.00 total |

Two sessions is the better experiment: session 2 is designed from session
1's attributed knee instead of from a guess.

## 6. Excluded, and why

| Excluded | Reason |
|---|---|
| Database failover / HA / standby promotion | One PostgreSQL host, no standby. Claiming any of it would be false |
| DB instance scaling (m7i.large → xlarge → 2xlarge) | Replaces the DB instance and its data, and changes cost. Proposed only if E2 attributes the knee to DB CPU; decided by the owner then |
| Provisioned IOPS on the data volume | Same: only if H7 is supported; a Terraform change to approve then |
| Shard **subset** sweeps (`-sweep N` < deployed) | The local sweep showed they confound deployment with driving (H7 of FINDINGS); E1 deploys real counts |
| Closed-loop concurrency ladders as capacity evidence | Measure latency below the knee (fact 3); kept only in B1 for comparison |
| Concurrency 16 | The documented trap |
| Phase 7 external-effect matrix | Its runner (`experiments/scripts/extsim-matrix.sh`) drives minikube; porting it is work not yet done. Optional stage if time remains; otherwise it stays a lab result |
| Semantic idempotency | No infrastructure dependence; the encoder is not deployed |
| AZ failure | Single-AZ deployment by design |

## 7. Comparing with the laptop — what must be normalized

| Factor | Laptop | Pilot | Consequence |
|---|---|---|---|
| CPU | i5-1335U, 2 P-cores + 8 E-cores, 12 threads, shared by everything | engine m7i.large ×3 (2 vCPU each, Sapphire Rapids), harness c7i.large, DB m7i.large, all dedicated | Compare **per-engine-vCPU** throughput and ratios, never raw wf/s |
| Contention | generator, 3 engines, PostgreSQL and standby on one host | separated by node role | Laptop numbers at high load include contention the cloud does not have |
| DB storage | laptop NVMe (btrfs 86 % full), fsync latency unknown | gp3 3 000 IOPS / 125 MB/s, network-attached | Commit latency likely higher in the cloud; per-shard ceilings (H2) may be LOWER |
| DB memory | `shared_buffers` 256 MB (lab) | 1 GB | both hold the working set; recorded in every bundle |
| Network | loopback / docker bridge | VPC, same subnet; harness→DB RTT measured per point | add RTT to the decomposition before comparing |
| Generator | 1 process on the shared host | 1 pod on a dedicated c7i.large | cloud generator saturation is measured, laptop's was only flagged |
| Window | 20–25 s | 60 s (B1 uses 25 s for the like-for-like comparison) | — |
| Journal size at start | 162 420 events (sweep) | empty after each reset | a suspected laptop slowdown; recorded, not corrected |
