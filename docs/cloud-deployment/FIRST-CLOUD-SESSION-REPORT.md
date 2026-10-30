# Durexec First Cloud Session Report

**Date:** 2026-10-30

**AWS account / region:** `183192605828` / `us-east-1`
**Result:** Initial AWS deployment, smoke validation, one usable small load point, evidence capture, and teardown completed.

## Source and published images

- Branch: `claude/peaceful-albattani-82210f`
- Commit: `9c07a5532640ba3ea81c83d8d260cf02dba099dd`
- Images were built from the clean tracked application source at this commit. No uncommitted application code was published.

| ECR image | Tag | Digest |
| --- | --- | --- |
| `durexec/engine` | `9c07a55` | `sha256:42f47b2c89c3027355f9adc3f2ea06ce4bebe4623c42a92959c098c355b7b270` |
| `durexec/extsimd` | `9c07a55` | `sha256:b444b48a1fdc273be05b00f1fcff9908d679f2723fbab8bae7b2f6d71e667449` |
| `durexec/loadgen` | `9c07a55` | `sha256:f70f60873fad665a5d8102ee6b6483f8216cd2c71196f0a1319393cff8da72c3` |

## Infrastructure and deployment

Terraform initially attempted the planned 32 creates. AWS rejected the original database instance type under the account's instance-type restriction, and the first EBS CSI policy ARN was invalid. The local ignored `terraform.tfvars` was set to the allowed `m7i-flex.large` and `c7i-flex.large` types; the IAM policy ARN and EBS CSI add-on identity wiring were corrected. Terraform then applied a recovery plan for the remaining resources.

The pilot ran on EKS cluster `durexec-bench` (Kubernetes 1.37), with three `m7i-flex.large` engine nodes, one `c7i-flex.large` harness node, and a `m7i-flex.large` PostgreSQL host. The public Kubernetes API allowlist remained `157.50.241.83/32`. Database SSH ingress remained disabled. PostgreSQL initialized, the workload retrieved its database credential from Secrets Manager without printing the secret, authenticated, connected, and completed migrations. Three engine replicas became Ready on three distinct engine nodes, serving 16 shards.

The deploy script's probe pod initially lacked a toleration for the harness node taint. The local Kubernetes helper was corrected and the probe then scheduled successfully. The EBS CSI add-on initially lacked scoped AWS credentials; it was repaired with EKS Pod Identity and reached Ready.

The EKS smoke test passed **10/10 checks**, including node readiness, replica and shard ring consistency, health, persisted smoke workflow, metrics, database credential access, and PostgreSQL instrumentation. NetworkPolicy behavior was checked separately: database access was blocked for the probe when the policy was applied and restored after its removal (`networkpolicy_enforced=yes`, `restored_after_delete=yes`).

## Initial load test

Two short points were run with three engine replicas, 16 driven shards, concurrency 4, and synchronous commits. The 60-second point completed 72 workflows and was labeled `INCONCLUSIVE_only_72_samples`; it is not treated as a valid performance result. The 150-second point passed the script's `SMOKE_ok` criteria and is a small deployment-validation result, not a capacity or production claim.

| Measurement (150-second point) | Observed |
| --- | ---: |
| Started / completed | 251 / 232 |
| Throughput | 1.66 completed workflows/s |
| p50 / p95 / p99 latency | 2.3064 / 3.5034 / 4.5045 s |
| Failed / non-completed / ownership-follow errors | 0 / 0 / 0 |
| Engine CPU total / largest pod | 0.093 / 0.043 cores |
| Peak engine memory (largest pod) | 15 MB |
| Engine node peak CPU | 3.9% |
| Load generator CPU / peak memory | 0.011 cores / 8 MB |
| Load generator restarts / exit code | 0 / 0 |

PostgreSQL observations during the point: peak 50 connections and 2 active connections; 0 lock waits and 0 deadlocks; approximately 477.397 commits/s; 0 rollbacks/s; WAL 0.030 MB/s; buffer hit ratio 1.000; mean statement time 0.026 ms. Database RTT was p50 0.285 ms and maximum 1.106 ms. The DB host reported 4.433% CPU, 1.857% I/O wait, 41.731 disk write IOPS, and 4.5% disk utilization.

All 16 shards were owned across the three engine pods with distribution **5 / 9 / 2** (skew 1.69). The 251 observed event-log runs were completed; duplicate run sequences, duplicate activity completions, orphan completions, sequence gaps, multiple terminal events, and pending tasks were all zero. No pod restarts or nonzero exit codes were observed. The generator was not saturated.

No engine fault-injection trial was run, so there is no fault-test CSV or result to claim. This session did not establish database high availability, exactly-once behavior for arbitrary external effects, or production readiness. These are actual AWS observations and are distinct from prior Docker simulations and local experiments.

## Evidence

- Evidence directory: [`docs/results/cloud/20261030T091915Z-first-cloud-session`](../results/cloud/20261030T091915Z-first-cloud-session/)
- Evidence archive: `docs/results/cloud/20261030T091915Z-first-cloud-session.tar.gz`
- Campaign CSV and raw run data: [`docs/results/cloud-campaign`](../results/cloud-campaign/)
- The collector completed with 26 artifacts and drain confirmation. The bundle contains pod descriptions and logs, placement and restart/exit evidence, engine metrics, shard and health snapshots, database invariants and PostgreSQL statistics, and the campaign results CSV. Image digests, infrastructure summary, NetworkPolicy proof, and fault-test status were added to the bundle.

## Teardown, duration, and cost

Terraform teardown completed. The cluster, database host and data volumes, node groups and EC2 workers, add-ons, workload secret, and campaign networking resources were removed. The three campaign ECR repositories were also deleted after the teardown sweep. A remaining Elastic IP seen during the sweep was verified as tagged `churn-ml-system` / `prod` and was left untouched. A final tag-filtered query found no Durexec VPC. No unrelated AWS resources were deleted.

The observed session window from the first published image (08:28 UTC) through final teardown checks (about 09:39 UTC) was approximately **71 minutes**; the infrastructure was active for roughly an hour. Estimated AWS cost is approximately **$0.75** for the pilot session, based on the recorded runtime and the pre-session hourly estimate of about $0.63/hour. This is an estimate, not billing data or an AWS-enforced cap; actual charges can differ. The authorized $10 target was not approached. The campaign ECR images and repositories no longer remain to incur ongoing storage charges.

## Local working tree

No commits were created or pushed. The existing stash and historical files under `docs/results/cloud/` were left untouched. Local Terraform configuration and helper fixes remain uncommitted in `engine/deploy/aws/eks/main.tf` and `engine/scripts/aws/_k8s.sh`; campaign evidence remains in the paths above. Other untracked local files were not cleaned or discarded.
