# Durexec Cloud Testing Campaign

Consolidated guide to the real AWS pilot and the earlier local, Docker, and Minikube experiments that informed it. Results are labeled by environment throughout; local results are not AWS measurements.

**AWS pilot source:** branch `claude/peaceful-albattani-82210f`, commit `9c07a5532640ba3ea81c83d8d260cf02dba099dd`, AWS `us-east-1`. The pilot has been torn down. No commits were created or pushed for the session.

## Contents

- [Executive Summary](#1-executive-summary)
- [Cloud Architecture](#2-cloud-architecture)
- [Deployment Results](#3-deployment-results)
- [Smoke Tests](#4-smoke-tests)
- [Performance Results](#5-performance-results)
- [Resource Utilization](#6-resource-utilization)
- [Shard Distribution](#7-shard-distribution)
- [Network and Security](#8-network-and-security)
- [Fault Tolerance and Recovery](#9-fault-tolerance-and-recovery)
- [Correctness and Durability](#10-correctness-and-durability)
- [Problems Encountered](#11-problems-encountered)
- [Final Results and Conclusions](#12-final-results-and-conclusions)
- [Limitations](#13-limitations)
- [Infrastructure and Cost](#14-infrastructure-and-cost)
- [Reproducing Experiments](#15-reproducing-experiments)
- [Evidence Index](#evidence-index)

### Overall results at a glance

| Experiment | Environment | Runs / samples | Key result | Outcome |
|---|---|---:|---|---|
| EKS deployment + smoke suite | AWS EKS, 3 engine replicas | 1 deployment; 10 checks | 10/10 checks passed; three engine pods Ready on distinct nodes | **PASS** |
| Initial load point, 60 s configured | AWS EKS, c=4, 16 shards | 72 completed samples in 50 s measured window; 85 accepted starts | 1.44 completed workflows/s; 0 failures; below the runner's 150-sample minimum | **INCONCLUSIVE** |
| Initial load point, 150 s configured | AWS EKS, c=4, 16 shards | 232 completed in 140 s; post-run invariant query covered 251 runs | 1.66 workflows/s; p50/p95/p99 2.306/3.503/4.505 s; 0 failed or non-COMPLETED terminal runs | **PASS as deployment smoke only** |
| NetworkPolicy enforcement | AWS EKS | 1 deny/restore check | Probe database traffic blocked by policy and restored after policy removal | **PASS** |
| Engine fault injection | AWS EKS | 0 trials | No engine fault trial was run; no cloud fault CSV exists | **NOT RUN** |
| Shard / concurrency sweep | Local Fedora laptop | 7 points | Best observed 129.30 wf/s at 16 driven shards and c=256; c=512 generator/host saturated | **Exploratory; not cloud evidence** |
| Recovery and external-effect drills | Local Docker / Minikube | Multiple scenarios; see Section 9 | Useful bounded recovery observations, including failures and limitations | **PASS for named checks; not AWS** |

The AWS pilot validates that this configuration can be provisioned, deployed, exercised, observed, and torn down. The one valid cloud load point is too small to establish a capacity curve or production performance. All measurements below are direct observations from the cited files; blank or absent metrics remain unreported.

## 1. Executive Summary

On 2026-10-30, the engine and load generator were built from clean tracked application source at commit `9c07a5532640ba3ea81c83d8d260cf02dba099dd`, published to ECR, and deployed to a temporary EKS cluster. PostgreSQL ran on one separate EC2 host. Deployment and smoke checks passed after several infrastructure and harness corrections. Two short load points ran; only the 150-second configured point met the script's sample threshold. NetworkPolicy enforcement was independently verified. The evidence collector completed, and Terraform plus the campaign ECR repositories were torn down.

The cloud point observed 232 completions in its 140-second measurement window (1.66/s), with p50 2.3064 s, p95 3.5034 s, and p99 4.5045 s. The generator was lightly loaded, all three engine pods remained healthy, and the correctness query found zero duplicate or missing-event invariants. This is a small **AWS deployment-validation smoke result**, not a benchmark establishing scalability, a throughput ceiling, availability, or production readiness.

Earlier experiments ran on a Fedora laptop, Docker Compose, or a local Minikube VM. They include useful exploratory throughput, process-failure, database-outage, restore, and external-effect tests. Those results are summarized separately and are never combined with the AWS figures.

## 2. Cloud Architecture

| Layer | AWS pilot configuration | Observed state |
|---|---|---|
| Control plane | EKS cluster `durexec-bench`, Kubernetes 1.37 | ACTIVE during test; subsequently deleted |
| Engine workers | Engine node group: 3 × `m7i-flex.large`; engine replicas: 3 | Each replica Ready on a distinct engine node |
| Load generator | Harness node group: 1 × `c7i-flex.large` | Job ran on the harness role, separate from engine nodes |
| Database | One PostgreSQL 16.15 host on `m7i-flex.large`, private to the pilot VPC; `pg_stat_statements`, `track_io_timing`, and synchronous commit enabled | Initialized, authenticated, migrated, queried; one database host only |
| Sharding | 16 deployed and driven shards; consistent-hash ring across 3 engine pods | Ownership split 5 / 9 / 2 during the measured point |
| Images | ECR repositories `durexec/engine`, `durexec/extsimd`, and `durexec/loadgen`, tag `9c07a55` | Pulled successfully by the workload; repositories later removed |
| API exposure | Public and private EKS endpoints; source allowlist `157.50.241.83/32` | The allowlist was not broadened; database SSH ingress was disabled |

This was not an RDS or HA database deployment. It had no PostgreSQL standby. The collector's [`53-infrastructure.txt`](../results/cloud/20261030T091915Z-first-cloud-session/53-infrastructure.txt) records the cluster, node-group, database host, security group, and allowlist observed in AWS. [`01-nodes.txt`](../results/cloud/20261030T091915Z-first-cloud-session/01-nodes.txt), [`02-pods-wide.txt`](../results/cloud/20261030T091915Z-first-cloud-session/02-pods-wide.txt), and [`08-placement-proof.txt`](../results/cloud/20261030T091915Z-first-cloud-session/08-placement-proof.txt) show actual node and pod placement.

The deployment used EKS Pod Identity for the EBS CSI controller. Engine database credentials were retrieved from Secrets Manager into Kubernetes without printing the password in the deployment output or report.

## 3. Deployment Results

| Check | Result | Evidence / note |
|---|---|---|
| Source revision and image build | **PASS** | Image records show commit `9c07a55`, clean source tree, and the full commit SHA; see [`images.txt`](../results/cloud-campaign/images.txt). |
| ECR publish and workload pulls | **PASS** | Engine and load-generator images were published and pulled for the run. The extsimd image was published and its digest recorded, but the simulator was not deployed because no AWS fault trial ran. Repositories were deleted at teardown. |
| Terraform apply | **PASS after recovery plan** | Initial apply partially failed; fixes and the applied recovery are detailed in [Problems Encountered](#11-problems-encountered). |
| PostgreSQL bootstrap and migration | **PASS** | Database accepted credentials, the engine connected and migrated, and the smoke workflow was visible in PostgreSQL. |
| Engine replicas | **PASS** | 3/3 Ready, on 3 distinct engine nodes; no restarts or nonzero exit codes in captured evidence. |
| EKS system add-ons | **PASS after IAM fix** | EBS CSI and Pod Identity agent became healthy. |
| Teardown | **PASS** | Cluster removed, database host terminated, pilot volumes and secret removed, tagged pilot VPC absent, and Durexec ECR repositories deleted. A separately tagged production Elastic IP was identified as unrelated and left untouched. |

Published image digests:

| Image | SHA-256 digest |
|---|---|
| `durexec/engine:9c07a55` | `sha256:42f47b2c89c3027355f9adc3f2ea06ce4bebe4623c42a92959c098c355b7b270` |
| `durexec/extsimd:9c07a55` | `sha256:b444b48a1fdc273be05b00f1fcff9908d679f2723fbab8bae7b2f6d71e667449` |
| `durexec/loadgen:9c07a55` | `sha256:f70f60873fad665a5d8102ee6b6483f8216cd2c71196f0a1319393cff8da72c3` |

The build used clean tracked application code. Local infrastructure and harness helper changes were made during deployment and were not included in the published application images. Campaign provenance labels the load-test checkout `dirty` because of those local helper changes; the image provenance itself says `tree=clean`.

## 4. Smoke Tests

The EKS smoke script passed all **10** checks (S1–S9, with S2b counted separately). There is no separate archived smoke stdout transcript; the check list below comes from the executed [`eks_smoke.sh`](../../engine/scripts/aws/eks_smoke.sh), and the bundle preserves supporting pod, health, shard, database, metrics, and event evidence.

| Smoke check | Outcome | Observation |
|---|---|---|
| S1 — engine pods Ready | **PASS** | All 3 engine pods Ready. |
| S2 — pods on distinct nodes | **PASS** | 3 replicas on 3 different engine nodes. |
| S2b — replica count, ring, node count agree | **PASS** | 3 replicas, 3 ring members, `NODE_COUNT=3`. |
| S3 — PostgreSQL is external to Kubernetes | **PASS** | No in-cluster PostgreSQL StatefulSet; DB was on EC2. |
| S4 — each engine healthy against external DB | **PASS** | `/healthz` was healthy on every engine pod. |
| S5 — all shards owned and ownership spread | **PASS** | No unowned shards; all 3 pods owned shards. |
| S6 — workflow durably completes | **PASS** | Smoke workflow reached `COMPLETED` and was visible in PostgreSQL. |
| S7 — required engine metrics present | **PASS** | Health and metrics snapshots retained for each engine pod. |
| S8 — probe reaches DB with engine credentials | **PASS** | Credential-backed query found the smoke execution in PostgreSQL. |
| S9 — PostgreSQL comparison instrumentation | **PASS** | `pg_stat_statements`, `track_io_timing`, and probe permissions verified. |

## 5. Performance Results

### Actual AWS load points

Both AWS points used `MODE=scaling` (closed-loop), `KIND=smoke`, concurrency 4, 16 driven/deployed shards, 4 workflow workers and 8 activity workers, synchronous commit, a 10-second warmup, and the `DeliveryOrder` workload with 50 ms preparation timer. The 60-second configured duration therefore measured 50 seconds; the 150-second configured duration measured 140 seconds.

| Experiment | Config | Starts accepted | Completed in measured window | Throughput | p50 / p95 / p99 | Errors / routing follows | Verdict and interpretation |
|---|---|---:|---:|---:|---:|---:|---|
| `r3-S16-drive16-w4x8-scon-scaling-c4-d60s-t1` | 3 replicas, 16 shards, c=4, 60 s + 10 s warmup | 85 | 72 | 1.44/s | 2.9060 / 4.0045 / 4.2062 s | 0 / 0 | **INCONCLUSIVE** — only 72 latency samples, below the 150-sample minimum. Its reported p99 is effectively the worst sample, not a stable 99th percentile. |
| `r3-S16-drive16-w4x8-scon-scaling-c4-d150s-t2` | Same topology and workers, c=4, 150 s + 10 s warmup | 251 | 232 | 1.66/s | 2.3064 / 3.5034 / 4.5045 s | 0 / 0 | **PASS as smoke** — passed `SMOKE_ok`; one short point, not a capacity result. The end-of-run invariant query found all 251 runs `COMPLETED` after the measurement window. |

The two points are not a scaling comparison: configuration was unchanged, they were short runs, and one failed the minimum sample threshold. No open-loop arrival-rate sweep, concurrency sweep, engine-node-count sweep, database-size sweep, or statistically repeated performance matrix was run in AWS. The CSV leaves fields blank where a measurement was unavailable; this README does not estimate them.

### Earlier local performance work (not AWS)

The local shard sweep ran on one Fedora laptop (Intel i5-1335U, 10 physical cores, 15 GiB RAM) with a dirty checkout and competing host load. The engine remained deployed with 16 shards: the “shards” column is the number driven, not a redeployment to that shard count. Later analysis corrected the corrupt latency columns in its CSV from the preserved raw load-generator output; use the table below and the corrected analysis, not the old CSV latency fields.

| Experiment | Config | Runs / completed | wf/s | p50 / p95 / p99 (s) | Outcome / interpretation |
|---|---|---:|---:|---:|---|
| Local shard subset 1 | 1 driven shard, c=128, 25 s | 105 | 5.25 | 22.78 / 24.59 / 24.88 | **INCONCLUSIVE** — sample and window too small; latency exceeded the 20 s measurement window. |
| Local shard subset 2 | 2 driven shards, c=128, 25 s | 97 | 4.85 | 13.27 / 15.66 / 15.78 | **INCONCLUSIVE** — too few completions; difference from 1 shard is within run noise. |
| Local shard subset 4 | 4 driven shards, c=128, 25 s | 294 | 14.70 | 5.37 / 8.79 / 9.20 | **INCONCLUSIVE for capacity** — fixed concurrency was offered-load-limited. |
| Local shard subset 8 | 8 driven shards, c=128, 25 s | 577 | 28.85 | 2.94 / 16.95 / 19.59 | **INCONCLUSIVE for capacity** — fixed concurrency; tail latency high. |
| Local shard subset 16 | 16 driven shards, c=128, 25 s | 943 | 47.15 | 2.57 / 4.36 / 5.27 | **INCONCLUSIVE for capacity** — increasing concurrency later raised throughput, showing c=128 did not locate the knee. |
| Local concurrency 256 | 16 driven shards, c=256, 25 s | 2,586 | 129.30 | 1.92 / 2.61 / 2.82 | **PASS as an exploratory local point** — generator at 63%; not an AWS or product capacity claim. |
| Local concurrency 512 | 16 driven shards, c=512, 25 s | 890 | 44.50 | 3.29 / 15.37 / 18.66 | **INCONCLUSIVE** — generator used 101% of a core and host load reached 21.97; generator, engine, and host saturation were confounded. |

An earlier local open-loop Phase 6 sweep observed 12.98 completed workflows/s per node at offered rate 14, and 25.78/s on two nodes (1.99×) on a 2-vCPU Minikube lab. At offered rate 20, throughput collapsed to 4.0/s and p99 to 76 s, with many empty DB pool acquisitions. These observations motivate measuring database wait and generator saturation together; they do not predict AWS throughput. See the [evidence inventory](../cloud-campaign/01-EVIDENCE-INVENTORY.md) and [cloud experiment matrix](../cloud-campaign/02-EXPERIMENT-MATRIX.md) for context and the caveats on worker-count confounding.

## 6. Resource Utilization

Measurements below are from the 150-second configured AWS point and its 140-second load window unless noted. CPU is in cores unless shown as percent. Node memory is the kubelet-reported peak; process RSS and pod working set are different metrics and are labeled separately.

| Resource | Observed | Interpretation |
|---|---:|---|
| Engine pod CPU, total across 3 | 0.093 cores | Low use during this small c=4 smoke load. |
| Engine pod CPU, largest pod | 0.043 cores | No evidence engine CPU was saturated. |
| Engine pod working-set peak, largest | 15.5 MB | Kubelet sample; `/metrics` process RSS peak was 24.2 MB. |
| Engine node CPU, maximum | 3.9% | Highest node utilization reported by the run reducer. |
| Engine node CPU, avg / peak by node | 0.054 / 0.061; 0.076 / 0.082; 0.067 / 0.079 cores | No node approached saturation. |
| Engine node memory peak | 624–873 MB | Kubelet-reported node/container memory over the sample interval. |
| Harness node CPU, avg / peak | 0.034 / 0.042 cores | Includes the load generator and harness-side probe activity. |
| Harness load-generator pod CPU, avg / peak | 0.011 / 0.012 cores | Generator was not saturated. |
| Load-generator pod memory peak | 7.9 MB | Kubelet working set. |
| Engine pod restarts / loadgen restarts | 0 / 0 | Captured pod exit and restart evidence reported zero. |

The pilot did not reveal a CPU or generator bottleneck at this low offered load. That does not identify the system's bottleneck at higher throughput. GPU metrics, CPU throttling, and a saturated operating point were not measured. Detailed samples: [`61-k8s-reduced.txt`](../results/cloud-campaign/raw/20261030T091408Z-r3-S16-drive16-w4x8-scon-scaling-c4-d150s-t2/61-k8s-reduced.txt), [`63-metrics-reduced.txt`](../results/cloud-campaign/raw/20261030T091408Z-r3-S16-drive16-w4x8-scon-scaling-c4-d150s-t2/63-metrics-reduced.txt), and [`14-restarts-exits.txt`](../results/cloud/20261030T091915Z-first-cloud-session/14-restarts-exits.txt).

### PostgreSQL and database-host measurements

| Metric | AWS observed value |
|---|---:|
| PostgreSQL version / `synchronous_commit` | 16.15 / on |
| Peak connections / active connections | 50 / 2 |
| Peak lock waits / ungranted locks / deadlocks | 0 / 0 / 0 |
| Transaction commits / second | 477.397 |
| Rollbacks / second | 0 |
| WAL bytes / second | 30,444 bytes/s (0.030 MB/s) |
| Buffer hit ratio | 1.000 |
| Mean statement time | 0.026 ms |
| Database RTT p50 / maximum | 0.285 ms / 1.106 ms |
| DB-host CPU busy / iowait / steal | 4.433% / 1.857% / 0.021% |
| DB-host disk write IOPS / write throughput / utilization | 41.731 / 0.490 MB/s / 4.5% |
| Wait-event profile | `ClientRead` 100% of sampled wait events |

These are measurements at one small load point. Low DB CPU and disk utilization indicate the database was not under pressure in this run; they do not prove a DB ceiling or rule out database contention at higher load. See [`60-pg-reduced.txt`](../results/cloud-campaign/raw/20261030T091408Z-r3-S16-drive16-w4x8-scon-scaling-c4-d150s-t2/60-pg-reduced.txt), [`10-db-rtt-ms.txt`](../results/cloud-campaign/raw/20261030T091408Z-r3-S16-drive16-w4x8-scon-scaling-c4-d150s-t2/10-db-rtt-ms.txt), and the collector's [PostgreSQL snapshot](../results/cloud/20261030T091915Z-first-cloud-session/42-pgstat.txt).

## 7. Shard Distribution

| Experiment | Deployed / driven shards | Owners | Distribution | Skew | Outcome |
|---|---:|---:|---:|---:|---|
| AWS 60-second point | 16 / 16 | 3 | 5 / 9 / 2 | 1.69 | **PASS** — every shard had a live owner. |
| AWS 150-second point | 16 / 16 | 3 | 5 / 9 / 2 | 1.69 | **PASS** — stable ownership across the point; no unowned shard was reported. |

For three replicas and 16 shards, consistent hashing yielded an uneven 5/9/2 split. The busiest owner's shard count was 1.69 times the fair share. This is an observed ownership imbalance, not evidence of corresponding CPU or throughput imbalance: measured pod CPU remained low. The AWS session did not vary replica count or shard count, so it did not measure rebalancing or scaling behavior.

The local code-derived ring table also shows shard ownership is not necessarily even when shard count is divisible by replicas. Refer to [`CORRECTED-ANALYSIS.md`](../results/local-shard-sweep/CORRECTED-ANALYSIS.md) and the actual AWS snapshot [`31-shards.json`](../results/cloud/20261030T091915Z-first-cloud-session/31-shards.json).

## 8. Network and Security

| Check | Environment | Outcome | Scope / caveat |
|---|---|---|---|
| Operator API allowlist | AWS EKS | **PASS** | `157.50.241.83/32`; no shared proxy IP or broad CIDR was added. |
| Database SSH ingress | AWS | **PASS** | Disabled. PostgreSQL access was limited to the pilot engine/harness security group. |
| Secrets Manager credential retrieval | AWS EKS | **PASS** | The deployer and engine probe retrieved and used credentials; password values were not recorded in the report. |
| IAM for EBS CSI | AWS EKS | **PASS after fix** | Dedicated scoped role via EKS Pod Identity; add-on became Ready. |
| NetworkPolicy enforcement | AWS EKS | **PASS** | Probe SQL failed while deny policy applied and succeeded after it was deleted: `networkpolicy_enforced=yes restored_after_delete=yes`. |
| Engine↔DB measured RTT | AWS EKS | **PASS** | Harness-to-DB SQL RTT samples collected. The distroless engine image was not instrumented with a direct node-to-DB probe. |
| Network partition / engine-node isolation | AWS EKS | **NOT RUN** | The probe policy check proves CNI policy enforcement for that probe; it is not an engine partition fault trial. |

Evidence: [`54-networkpolicy-verification.txt`](../results/cloud/20261030T091915Z-first-cloud-session/54-networkpolicy-verification.txt) and the copy under [`cloud-campaign`](../results/cloud-campaign/networkpolicy-verification.txt). The successful reachability test is a functional verification, not a broad security audit.

## 9. Fault Tolerance and Recovery

### AWS fault tests

| Fault scenario | AWS trials | Result |
|---|---:|---|
| Engine process kill / SIGKILL | 0 | **NOT RUN** |
| Engine pod delete or node drain | 0 | **NOT RUN** |
| Engine EC2 node termination | 0 | **NOT RUN** |
| Engine-to-DB partition or latency injection | 0 | **NOT RUN** |
| PostgreSQL host reboot / failover | 0 | **NOT RUN**; only one DB host existed |
| NetworkPolicy probe deny/restore | 1 | **PASS**, but not an engine recovery test |

There is no AWS `faults.csv` because the cloud engine fault matrix was never executed. The evidence bundle's [`56-fault-test-status.txt`](../results/cloud/20261030T091915Z-first-cloud-session/56-fault-test-status.txt) explicitly records this.

### Earlier local Docker / Minikube recovery experiments

| Experiment | Environment and samples | Observed result | Verdict / limit |
|---|---|---|---|
| Engine lease recovery | Local two-node Docker image validation; graceful and abrupt termination | Graceful full re-ownership 3 s; SIGKILL full re-ownership 29 s at 10 s lease TTL; tokens advanced 1→2→3. | **PASS for this local setup**; not AWS. |
| Phase 9 recovery campaign | Minikube, 2 vCPU / 3.8 GiB; 16 fault injections across 4,067 workflows | 4,057 completed at the sampling point; the remaining 10 were in flight. The aggregate oracle found 0 orphaned, duplicate-journal, duplicate-invocation, or stuck workflows. Graceful, frozen-node, and stale-owner trials are summarized below. | **PASS locally for observed invariants**; not cloud evidence. |
| Phase 9 lease/failover tests | Minikube, 2 vCPU / 3.8 GiB, 2 engine replicas, 16 shards; 3 graceful, 3 frozen-node, 3 stale-owner trials | Graceful takeover mean 5.05 s (max 6.11 s); frozen-node takeover mean 29.86 s (max 31.46 s) at 10 s TTL; zero stale live tokens, duplicate journal records, or orphans in the trials. | **PASS locally**; takeover includes a 2×TTL anti-churn guard. Abrupt-kill trials had restart-backoff contamination and should not be used as clean failover timing. |
| PostgreSQL outage | Minikube; 4 pause/restart trials under workload | DB-return-to-engine recovery 10–13 ms; progress resumed 135–188 ms later; 0 orphans, duplicates, duplicate invocations, or stuck workflows. | **PASS locally**; database restart itself took about 32.8–32.9 s. |
| Retry storm and poll depth | Minikube; retry storm against 20 real task rows; separate depth probe to 100k rows | No poll-latency degradation in the storm, but query plan was a sequential scan at 20 rows. At 1k–100k rows, indexed poll query stayed at 2 buffers and ~0.012 ms. | **PASS with caveat locally**; the index claim comes from the separate depth probe, not the 20-row storm. |
| Phase 7 external-effect matrix | Local Docker engine and separate effect simulator; 7 scenarios | Ambiguous response, pre-effect transient retry, journaled-then-crash, before-invoke, claim expiry, and owner SIGKILL each recorded one effect per dedup key; no duplicate journal tuples or pending tasks. Scenario E ended FAILED with an uncompensated effect. | **PASS for listed scenarios, with compensation gap**. Supports at-least-once invocation and effectively-once effect only if recipient honors the idempotency key. |
| Phase 8 effect-before-journal real SIGKILL | Local Minikube; final H attempt valid after earlier blocked/invalid attempts | `Requests=2`, `Performed=1`, `Replayed=1`; one journal completion, zero duplicate sequence groups. | **PASS locally for final H attempt only**; earlier H attempts were invalid/blocked and are preserved in the raw log. |
| Phase 8 ownership moved during external call | Local Minikube; owner paused, lease moved, stale worker resumed | Token 63→64 during pause; one external effect; stale task claim prevented the stale worker from journaling; fencing rejection itself was not exercised. | **PASS locally for task-claim protection**; not proof that fencing rejected this specific stale write. |
| PostgreSQL standby promotion | Local Docker Compose; 2 retained runs | Manual promotion took 0.14 s and 0.13 s; first new workflow completed at 19.51 s and 14.02 s; promoted DB lacked 2 and 1 committed events respectively; one workflow had shortened history in each. | **PASS for manual promotion/recovery path, with data loss observed**; async standby, not AWS, no automatic HA. |

Earlier failed/invalid attempts remain part of the record: Phase 8 H first failed to reach the intended seam, then a quiescence check blocked a run, then an evidence read happened after restart; only the final H block is valid. The Phase 7 raw transcript has `(ledger unreadable)` in per-key summaries because of a harness reader defect, although its separately printed counters remain available. Do not turn those attempts into successful fault trials.

## 10. Correctness and Durability

### AWS journal and workload invariants

The load runner's post-window SQL invariant check covered all accepted runs after they drained:

| Invariant | 60-second configured point | 150-second configured point |
|---|---:|---:|
| Runs checked | 85 | 251 |
| Duplicate `(run_id, seq)` | 0 | 0 |
| Duplicate activity completion | 0 | 0 |
| Orphan completion | 0 | 0 |
| Sequence gaps | 0 | 0 |
| Multiple terminal events | 0 | 0 |
| Pending tasks | 0 | 0 |
| Final execution status | `COMPLETED:85` | `COMPLETED:251` |

This is evidence about these pilot runs, not a proof against every concurrency or failure mode. The CSV's `failed=0` and `non_completed=0` are load-generator terminal counters; the larger accepted-start counts include runs that completed after the timed measurement window. The run-specific SQL check occurred after the point and found all recorded runs completed.

### Other local correctness and durability evidence

| Experiment | Outcome | Important qualification |
|---|---|---|
| Docker duplicate audit without fault | 1,000/1,000 completed; 4,000 activity completions; 0 duplicate step completions | **PASS locally**, but no-fault zero duplicates is weak evidence by itself. |
| Docker duplicate audit during node kills | Original: 878/1,000 completed, 122 refused/failed admission, 0 duplicate step completions across 3,512 activity completions. Rerun: 763/1,000 completed, 237 admission failures, 0 duplicates across 3,052 completions. | **Safety check passed for observed completions; full 1,000-run target NOT MET.** Requests to temporarily unowned shards received 409 and exhausted retries; they were refused, not lost accepted work. |
| Phase 8 same-instance dump/restore | 46,793 executions, 561,170 events, 16 lease rows; counts and status breakdown preserved; 0 duplicate journal tuples | **PASS for dump usability only**. Restore was into another DB on the same instance and disk. |
| Local integration and acceptance suites | Existing files report integration tests and acceptance checks run on local environments | See [results index](../results/README.md) and [acceptance matrix artifacts](../results/acceptance-matrix-final-image.txt); individual MANUAL, BLOCKED, and NOT RUN rows remain distinct from PASS. |

The AWS PostgreSQL settings included `fsync=on`, `full_page_writes=on`, and `synchronous_commit=on`. The observed cloud host reported `archive_mode=off`; no cloud backup/restore drill was run. The local restore experiment does not establish recovery from loss of the AWS database host or volume.

## 11. Problems Encountered

| Problem | Cause | Fix and outcome |
|---|---|---|
| First image publish failed | Local Docker credential helper could not access its locked GPG database. | Used a separate temporary `DOCKER_CONFIG`; all three images built and pushed, with digests recorded. |
| Initial Terraform apply rejected DB type | AWS account restrictions rejected the originally planned instance type. | Set ignored local Terraform variables to allowed `m7i-flex.large` and `c7i-flex.large`; applied a reviewed recovery plan. |
| EBS CSI IAM policy attach failed | Managed policy ARN omitted its `service-role/` path. | Corrected the ARN and used a dedicated scoped controller role. |
| EBS CSI controller crash-looped | Add-on had no scoped AWS credentials for EBS operations. | Installed Pod Identity agent and associated the EBS role; add-on became Ready. |
| Deploy probe remained Pending | Probe pod lacked a toleration for the harness node's `NoSchedule` taint. | Added the toleration to the local Kubernetes helper; probe scheduled and DB checks passed. |
| Historical mock evidence contains degraded/placeholder data | Oct. 29 mock harness run used a test double; the “degraded” capture had a Ready node but no workload pods. | Preserved and labeled as harness/mock evidence only; neither is an AWS result. |
| Short AWS load point under-sampled | Only 72 completed latency samples in the 50-second window; configured runner minimum was 150. | Marked `INCONCLUSIVE`; a longer 150-second configured point produced 232 in-window completions and passed `SMOKE_ok`. |
| Older local results have known validity issues | Fixed-concurrency tests were offered-load limited; c=512 saturated generator/host; local shard-sweep CSV latency columns were corrupt. | Used raw loadgen output plus [`CORRECTED-ANALYSIS.md`](../results/local-shard-sweep/CORRECTED-ANALYSIS.md); retained invalid rows and did not use them for cloud conclusions. |

The first AWS smoke/deploy issue and all recovery changes are also described in the [session report](FIRST-CLOUD-SESSION-REPORT.md). No failure required broadening network access or touching unrelated AWS resources.

## 12. Final Results and Conclusions

- The EKS deployment path works with the recorded configuration after correcting the Terraform policy identity and probe scheduling details.
- The application initialized PostgreSQL, retrieved credentials, migrated, placed three replicas on three nodes, and served all 16 shards.
- The 150-second configured AWS smoke point completed 232 workflows in-window at 1.66/s, with p50/p95/p99 of 2.3064/3.5034/4.5045 seconds. All 251 run records checked after drain completed with zero measured journal invariants violated.
- Resource use was low at concurrency 4, so this run does not identify a throughput bottleneck or scaling curve.
- A separately applied NetworkPolicy demonstrably blocked and restored probe-to-database access.
- The evidence collector captured logs, metrics, placement, PostgreSQL state, invariants, CSV, and raw samples. The cluster and campaign resources were then removed; unrelated production resources were left alone.
- Local fault and recovery studies provide useful engineering evidence but must not be reported as AWS tests. They show measured recovery delays, async replication loss, and conditional external-effect idempotency rather than universal guarantees.

**Assessment:** the campaign establishes a successful, bounded AWS deployment and a small end-to-end smoke result. It does not establish cloud scalability, high availability, arbitrary exactly-once external effects, or production readiness.

## 13. Limitations

1. **One small AWS workload point passed the sample threshold.** There are no repeats or offered-load sweeps to estimate variance, a knee, or capacity.
2. **One PostgreSQL host, no standby.** AWS database failover, host-loss recovery, backup restore, and HA were not tested.
3. **No AWS engine fault injection.** Pod kill, drain, node termination, DB outage, and engine-specific network partition trials were not run.
4. **The AWS run was short and lightly loaded.** Low CPU, memory, and DB use cannot be extrapolated to saturation.
5. **The shard split was uneven.** The 5/9/2 ring ownership was measured but not varied or rebalanced under failure.
6. **External effects are conditional.** Invocation is at least once; an effectively-once effect depends on the recipient honoring the idempotency key. This is not exactly-once arbitrary side effects.
7. **No AWS PITR or restore drill.** Cloud `archive_mode` was off, and the local restore was same-instance only.
8. **No cost billing export was reconciled.** The session cost is an estimate; the $10 target was not an AWS-enforced cap.
9. **Smoke output limitation.** The standalone smoke script stdout was not saved as a transcript; supporting state is in the evidence bundle and the session report records the 10/10 result.
10. **Local results have environment and method limits.** They used laptop/Minikube/Compose environments and, depending on the run, dirty trees, shared CPU, or sample/runner defects. They are context, not cloud comparisons.

## 14. Infrastructure and Cost

| Resource | Created for the pilot | Final status |
|---|---|---|
| EKS cluster `durexec-bench` | EKS 1.37 control plane | Deleted |
| Engine node group | 3 × `m7i-flex.large` | Scaled to zero and deleted |
| Harness node group | 1 × `c7i-flex.large` | Deleted |
| PostgreSQL EC2 host | 1 × `m7i-flex.large`, private address | Terminated |
| PostgreSQL EBS data/root volumes | Pilot-tagged | Deleted |
| EBS CSI / VPC CNI / Pod Identity add-ons and IAM | Scoped pilot resources | Deleted with cluster/Terraform resources |
| Secrets Manager DB secret | Pilot credential | Deleted |
| Pilot VPC, security groups and campaign networking | Created by Terraform | Tagged pilot VPC absent after destroy; campaign networking removed |
| ECR repositories and images | Three `durexec/*` repos | Deleted after evidence collection |
| Pre-existing Elastic IP | Tagged `churn-ml-system`, `prod` | Unrelated; intentionally untouched |

The observed execution window from first image publication at 08:28 UTC through final cleanup checks at about 09:39 UTC was approximately 71 minutes; infrastructure was active for roughly an hour. **Estimated session cost: about $0.75**, based on the recorded runtime and the pre-run estimate of roughly $0.63/hour. This is not billing data or a guaranteed amount. The authorized $10 spending target was not approached. No campaign ECR repositories remain to create ongoing image-storage charges.

Teardown evidence is summarized in [FIRST-CLOUD-SESSION-REPORT.md](FIRST-CLOUD-SESSION-REPORT.md). The final read-only AWS sweep found no Durexec EKS cluster and the database instance was terminated. An unrelated production Elastic IP was verified by its tags and left intact.

## 15. Reproducing Experiments

The pilot has been torn down and its ECR repositories deleted. Reproduction therefore requires rebuilding/publishing images and provisioning fresh infrastructure. The original saved Terraform plan is not a reusable deployment plan after teardown; generate and inspect a fresh plan before applying. Preserve the operator's current public IP as a `/32`, and use the same restricted AWS profile/region. Do not broaden the EKS API allowlist.

From `engine/`, the campaign configuration used an ignored `deploy/aws/eks/terraform.tfvars` similar to:

```hcl
operator_cidr = "<current-operator-public-ip>/32"
enable_db_ssh = false
engine_instance_type = "m7i-flex.large"
harness_instance_type = "c7i-flex.large"
db_instance_type = "m7i-flex.large"
```

The allowed instance types reflect the AWS account restriction observed in this session. They may vary by account and time; review the resulting plan rather than assuming they are universally available.

```bash
cd engine
export AWS_PROFILE=aiops-deploy
export AWS_REGION=us-east-1

aws sts get-caller-identity
terraform -chdir=deploy/aws/eks init
terraform -chdir=deploy/aws/eks validate
terraform -chdir=deploy/aws/eks plan -out=eks.tfplan
# Inspect the fresh plan before applying. Applying incurs AWS charges.

DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_publish.sh
terraform -chdir=deploy/aws/eks apply eks.tfplan

aws eks update-kubeconfig --region us-east-1 --name durexec-bench
kubectl get nodes -L durexec.io/role

DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_deploy.sh
scripts/aws/eks_smoke.sh
scripts/aws/eks_fault.sh verify-policy
```

Run the two original load points (these were `KIND=smoke`, not performance-grade `KIND=perf` runs):

```bash
EXP=first-cloud-session KIND=smoke MODE=scaling CONC=4 DUR=60s WARMUP=10s TRIAL=1 \
  DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_loadgen.sh

EXP=first-cloud-session KIND=smoke MODE=scaling CONC=4 DUR=150s WARMUP=10s TRIAL=2 \
  DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_loadgen.sh

scripts/aws/collect_eks_evidence.sh first-cloud-session
DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_teardown.sh
```

The load runner records metrics and raw evidence under `../docs/results/cloud-campaign/`. For a real scaling study, follow the [cloud experiment matrix](../cloud-campaign/02-EXPERIMENT-MATRIX.md): calibrate load, sweep offered rate/concurrency per configuration, repeat trials, require adequate sample counts, and reject generator-saturated or incomplete runs. The two short smoke invocations above reproduce deployment validation only.

## Evidence Index

| Evidence | Contents |
|---|---|
| [AWS evidence bundle](../results/cloud/20261030T091915Z-first-cloud-session/) and [archive](../results/cloud/20261030T091915Z-first-cloud-session.tar.gz) | Provenance, nodes/pods, placement, logs, metrics, health, shard state, PostgreSQL snapshots, invariants, campaign CSV, and fault-test status. |
| [AWS campaign CSV](../results/cloud-campaign/results.csv) | Both actual AWS load points, verdicts, measured metrics, and raw-data locations. |
| [AWS raw runs](../results/cloud-campaign/raw/) | Per-run load-generator output, SQL invariants, PostgreSQL snapshots, database RTT, kubelet samples, pod/node reductions, and engine metrics. |
| [Image digests](../results/cloud-campaign/images.txt) | ECR tags, digests, source SHA, and clean image build tree. |
| [NetworkPolicy proof](../results/cloud-campaign/networkpolicy-verification.txt) | Policy enforcement and connectivity restoration. |
| [Local/mock evidence directories](../../engine/docs/results/cloud/) | Oct. 29 mock harness capture and degraded capture; not AWS data. |
| [Local shard sweep](../results/local-shard-sweep/) | Laptop load points and corrected analysis; not cloud data. |
| [Local recovery and correctness index](../results/README.md) | Docker/Minikube, duplicate, crash, external-effect, restore, and acceptance records, each with validity notes. |
| [AWS session report](FIRST-CLOUD-SESSION-REPORT.md) | Deployment fixes, image digests, cloud measurements, teardown, duration, and cost estimate. |

Raw logs, CSVs, JSON, metrics, historical bundles, and experiment directories remain in place. This README is an index and interpretation layer; it does not replace those artifacts.
