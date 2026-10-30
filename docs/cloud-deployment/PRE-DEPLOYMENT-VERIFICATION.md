# EKS pre-deployment verification

**Date:** 2026-10-30 · **Commit tested:** `c25ef7bc96eb4db982c271e0c96af8bb12819c03`
(`origin/claude/modest-bardeen-2er9jl`, fast-forwarded locally onto
`claude/peaceful-albattani-82210f`) **plus the uncommitted fixes listed in §3**.

**No AWS resource was created, modified or billed.** Terraform ran `init`,
`validate` and `plan` only. No image was pushed. Every "local" result below
was produced in a cloud container with Docker; a real Kubernetes API was not
available (§5), so nothing here proves EKS networking, IAM, ECR or EC2.

Classification: **PASS** directly verified · **FAIL** confirmed defect ·
**FIXED** confirmed defect fixed locally in this verification, with a test ·
**NOT TESTED** · **CLOUD-ONLY** needs real AWS · **INCONCLUSIVE**.

---

## 1. Decision

**GO for a gated first cloud session (pilot bring-up + stage G checks), NOT
YET for the comprehensive campaign.** Conditions:

1. Commit the uncommitted fixes in §3 first. `eks_publish.sh` refuses a dirty
   tree (correctly), and the fixes change what the campaign measures.
2. Run the session from the operator laptop (`157.50.241.83/32`).
3. Treat the first session as bring-up: deploy, smoke, the five CLOUD-ONLY
   checks in §8, one smoke-kind load point, evidence, teardown. Only if all of
   those pass does the measurement campaign start.

What this decision does **not** mean: a passing local suite is not a working
cluster; a clean plan is not a working apply; a passing smoke test is not
reliability; throughput is not correctness; and the pilot has **one database
with no standby — it is not highly available** and must not be described so.

---

## 2. Changes since the EKS pilot implementation (`d09b461`)

| Commit | Content |
|---|---|
| `873550f`, `ad75cb8` | docs/ ignored then restored (documentation, results, raw sweep outputs) |
| `8487d3e` | harness fixes D1–D16 (`docs/cloud-campaign/03-HARNESS-READINESS.md`): distroless-safe HTTP/SQL (`_k8s.sh`), deploy verification, replica/ring coupling, kubelet CPU sampling, DB instrumentation, publish/teardown provenance; new fault injectors, pg/metrics reducers, journal latency/invariant SQL |
| `c8bbf70` | evidence inventory, experiment matrix, harness readiness docs |
| `c25ef7b` | stop ignoring docs/ |

Terraform changed only in `db_user_data.sh.tftpl` (instrumentation flags).

---

## 3. Defects found by this verification and fixed locally (uncommitted)

| # | Defect | Effect | Fix | Regression evidence |
|---|---|---|---|---|
| V1 | Parked runs (divergent replay, refused code version) were released with full jitter truncated to ms: any draw < 0.01 gave **0 ms** on attempt 1 | `TestWorkflowWorkerParksAndDoesNotJournalOnNonDeterminism` failed ~1 in 100 runs (seen once in two full `-race` runs); a parked run came straight back | `Engine.parkDelayMs`: floor of one backoff base (`internal/runtime/pool.go`); used by both park paths | `TestParkedRunBackoffHasAFloorEvenOnTheMinimumJitterDraw` pins the draw to 0; **fails on the old code** (`visibleIn:0`), passes now |
| V2 | `k8s_usage.py` read kubelet UTC timestamps as **local time** | On a laptop at UTC+05:30 every sample fell outside the window → engine CPU, node CPU and the engine-saturation attribution silently `unavailable` | explicit UTC | `scripts/aws/k8s_usage_test.py` (UTC, Asia/Kolkata, America/New_York); **fails on the old code** |
| V3 | `eks_loadgen.sh` / `eks_fault_trial.sh` wrote to `engine/docs/results/cloud-campaign` (they `cd engine/`); the collector and runbook read `<repo>/docs/results/cloud-campaign` | Campaign CSVs never bundled into evidence | default `OUT=../docs/results/cloud-campaign` | runner + collector end to end: `50-cloud-campaign-results.csv` bundled |
| V4 | Collector copied CSVs as `50-<basename>`; `eks-pilot/results.csv` and `cloud-campaign/results.csv` collide; `faults.csv` never copied | One CSV silently overwritten; fault results never bundled | names include the directory; `faults.csv` added | collector run |
| V5 | EKS overlay inherited the TEST-ONLY `extsim` Deployment with local tag `durexec-extsimd:phase7`, no nodeSelector | Permanent `ImagePullBackOff` pod on an engine node for the whole campaign | deleted in `overlays/eks/kustomization.yaml` (+ its two Services); `eks_deploy.sh` now refuses **any** image other than the ECR engine image or `pgvector/pgvector:pg16` | old render → `FATAL: Deployment/extsim uses image 'durexec-extsimd:phase7'`; new render passes at r1/r3/r4 |
| V6 | `eks_teardown.sh` ran `kubectl delete … || true` then `terraform destroy` even when the API was unreachable (any machine but the laptop) | In-cluster PVC volumes / load balancers orphaned and billing | refuses unless the API answers, or `TEARDOWN_WITHOUT_K8S=i-understand-k8s-objects-may-orphan`; waits for the namespace's PVs to be released | stubs: unreachable → exit 1, **destroy not called**; override → destroy + 11 sweep calls; no confirmation → nothing |
| V7 | CSV recorded only the *number* of shard owners | Ring skew (r3/S16 = 5/9/2) invisible in rows; E2's "S=24 is divisible by 3" premise is false (8/11/5) | columns `shards_per_owner`, `shard_skew` (99 columns); matrix and README corrected | runner row: `durexec-0:5;durexec-1:9;durexec-2:2`, `1.69` |

Also corrected in documentation (originals untouched): `docs/results/local-shard-sweep/CORRECTED-ANALYSIS.md`.

---

## 4. The seventeen previously identified items

| # | Item | Result | How verified |
|---|---|---|---|
| 1 | EKS-native evidence collection | **PASS** (local) / CLOUD-ONLY (proxy) | `collect_eks_evidence.sh` end to end via kubectl test double against 3 real engines + DB: 28 artefacts, `COMPLETE`, drained, invariants, redaction (0 password hits). V3/V4 fixed |
| 2 | Bounded authenticated init gate | **PASS** | real gate script, pgvector image, read-only, UID 65532, 64 MiB: OK in 0 s; wrong password → `GATE FAILED [AUTH]` after 3 tries (7 s); unroutable host → `[TIMEOUT]` at deadline; missing DB → `[NO_DATABASE]` 1 s; password never in log |
| 3 | Secret-based `DATABASE_URL` | **PASS** (render) / CLOUD-ONLY | render: `secretKeyRef durexec-db/url` (engine), `durexec-db/password` via env (gate); deploy builds the Secret with `--dry-run=client \| apply` |
| 4 | External DB endpoint substitution | **PASS** | deploy's own verify blocks: host in gate env, RFC1918 check, surviving placeholder stops the deploy |
| 5 | ECR publication / image pull | **PASS** (build) / CLOUD-ONLY | both images build from the real Dockerfiles (`linux/amd64`, UID 65532); dirty-tree refusal by reading; push, node pull via `AmazonEC2ContainerRegistryReadOnly` are cloud-only |
| 6 | Distroless compatibility | **PASS** | image has no `sh`/`wget` (`docker run --entrypoint sh` fails); smoke S1–S9 **10/10 PASS** via pod-proxy path to real engines |
| 7 | DB diagnostics without engine shell | **PASS** | probe-pod `psql` with the engine's Secret: invariants, pg_stat snapshot, settings, top statements, RTT all collected |
| 8 | Consistent benchmark/evidence directories | **FAIL → FIXED** (V3, V4) | was still wrong at `c25ef7b` |
| 9 | Three-engine replicas and ring | **PASS** | deploy patch at r1/r3/r4; 3 engines boot with ring of 3, `NODE_COUNT=3`, budget 98/400; smoke S2b |
| 10 | Anti-affinity / distinct nodes | **PASS** (manifest) / CLOUD-ONLY (scheduling) | `requiredDuringScheduling…`, `topologyKey: kubernetes.io/hostname`, `nodeSelector role=engine`; deploy refuses replicas > engine nodes. Real scheduling not exercised |
| 11 | Harness scheduling / generator placement | **PASS** (logic) / CLOUD-ONLY | Job toleration + nodeSelector; runner marks a generator on an engine node `INVALID_generator_on_engine_node` (forced test) |
| 12 | Generator CPU during the Job | **PASS** (local) / CLOUD-ONLY (kubelet proxy) | real cumulative cgroup counters sampled during the run: `gen_cpu_cores=0.227`, peak 0.242; V2 fixed |
| 13 | Validity classification | **PASS** | observed verdicts: `RECORDED`, `SMOKE_ok`, `INCONCLUSIVE_only_140_samples`, `INVALID_generator_on_engine_node`, `GENERATOR_SATURATED_0.238_cores`, `SATURATED_UNATTRIBUTED` (arrival) |
| 14 | PostgreSQL instrumentation | **PASS** | exact bootstrap flags on PG16: `pg_stat_statements` preloaded, `track_io_timing=on`, superuser probe; smoke S9 PASS; pg deltas, wait profile, DB-host `/proc` figures in rows |
| 15 | Cleanup / teardown ordering | **FAIL → FIXED** (V6) | k8s objects → PV wait → destroy → read-only sweep; exit code propagated |
| 16 | Fresh / concurrent migration | **PASS** | `TestFreshDatabaseMigration` (fresh, rerun, 3 concurrent); 5 rounds × 3 concurrent migrators on a **fresh** DB (the shipped test only runs them after a first migrate); 3 engines booting together on an empty DB |
| 17 | EKS API restricted to operator CIDR | **PASS** (plan) / CLOUD-ONLY | `public_access_cidrs = ["157.50.241.83/32"]`, private endpoint on |

---

## 5. Test results

### Go

| Command | Result |
|---|---|
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `gofmt -l` | clean |
| `scripts/test-integration.sh` (PostgreSQL 16 + pgvector, `-p 4`, strict), `MIGTEST_DSN` set, with fixes | **1223 passed, 0 failed, 0 skipped, 2 blocked**, exit 0 |
| same at `c25ef7b` without `MIGTEST_DSN` | 1221 passed, 0 failed, 1 skipped (`TestFreshDatabaseMigration`), 2 blocked |
| `RACE=1` run 1 at `c25ef7b` | 1220 passed, **1 failed** (V1), 0 data races |
| `-race` run 2 at `c25ef7b` | 1221 passed, 0 failed, 0 data races |
| `go test ./...` with **no** database | 1150 passed, **71 skipped** (70 PostgreSQL tests + the migration test), 2 blocked |

**Blocked (not passing):** `internal/acceptit` `TestAcceptSagaRecoveryOnLiveClusterBlocked`,
`TestAcceptNoDuplicateExternalEffectBlocked` — they need fault hooks at real
transaction boundaries and the external-effect simulator. The claims they
would cover (compensation neither skipped nor repeated under a mid-way crash;
no duplicate external effect) are **not established by this suite**.

The "73 PostgreSQL-dependent tests" are 71 at this commit; all run and pass
against a real database.

### Kubernetes

**kind could not run** in this container (nested `runc`: `can't get final
child's PID from pipe`). Substitute, clearly not Kubernetes:

| Verified with real components | Canned (test double) |
|---|---|
| `kubectl kustomize` render (12 objects after V5), deploy's patch + verify code at r1/3/4 and negative cases | pod/node/ConfigMap metadata, scheduling, node names |
| 3 engine containers from the real image, rendered ConfigMap, read-only, UID 65532, 2 CPU / 512 MiB | Job lifecycle (mapped to a container) |
| real init-gate script in the real image | kubelet Summary JSON (built from real cgroup counters) |
| real loadgen image against per-pod DNS names, real probe `psql`, real SQL and reducers | |

`eks_smoke.sh` 10/10; `eks_loadgen.sh` (scaling and arrival) rows of 99
columns, header = row; `collect_eks_evidence.sh` COMPLETE; `eks_teardown.sh`
guard.

**NOT TESTED:** real scheduling/anti-affinity/taints, PDB, probes driven by a
kubelet, StorageClass, `rollout status`, `kubectl apply` of the render
against an API server, `eks_fault*.sh` injectors, shellcheck (download not
permitted in this session).

### Terraform

| Check | Result |
|---|---|
| `fmt -check -recursive` | PASS |
| `init -backend=false` | PASS (aws 5.100.0, random 3.9.1 from the lock file) |
| `validate` | PASS |
| lock file satisfies `~> 5.60`, `~> 3.6`, Terraform `>= 1.9` | PASS |
| lock file **tracked** | **NOT YET** — present, no longer ignored, but uncommitted |
| `.terraform/`, `terraform.tfvars` ignored | PASS |
| `plan` (`operator_cidr=157.50.241.83/32`, `enable_db_ssh=false`) | **27 to add, 0 to change, 0 to destroy** |

27, not 28: `enable_db_ssh=false` removes `aws_vpc_security_group_ingress_rule.db_ssh[0]`.
The 27 addresses are identical to the previous plan; all are `create`, root
module, no existing state, so nothing unrelated can change.

---

## 6. Benchmark and evidence integrity

| Check | Result |
|---|---|
| CSV header vs row (runner) | PASS: 99 = 99, every field mapped by name, separators `;` `+` `\|` only |
| Latency extraction | PASS: p50/p95/p99 equal the loadgen table |
| Throughput | PASS: completed ÷ window |
| Sample counts, min-sample rule | PASS (`INCONCLUSIVE_only_140_samples`) |
| Raw preservation | PASS: 24 raw files per point |
| Generator / engine CPU and memory | PASS locally after V2; CLOUD-ONLY via kubelet proxy |
| PostgreSQL statistics | PASS |
| Shard distribution | PASS after V7 |
| Journal invariants | PASS, and they **detect** injected corruption (deleted event → `seq_gaps=1`, extra terminal → `multi_terminal=1`) |
| Reproducibility metadata | PASS: git SHA, tree state, image tag, ConfigMap, args, pods/nodes per point |
| Loadgen transport errors | INCONCLUSIVE: printed in raw notes (5 in one run, all starts journalled) but not a CSV column |
| `local-shard-sweep/results.csv` latency columns | **FAIL (historical)**: hold the shard count; `owner_nodes` empty. Original preserved |
| Raw local loadgen outputs | **PASS**: tracked; every published p50/p99/wf/s re-derived exactly (`CORRECTED-ANALYSIS.md`). `FINDINGS.md` §9 item 2 ("never committed") is wrong |

---

## 7. Security findings

| Finding | Class |
|---|---|
| Kubernetes API public only to `157.50.241.83/32`; proxy IP not added | PASS |
| 5432 only from the EKS cluster security group; no CIDR rule | PASS |
| SSH rule absent (`enable_db_ssh=false`) | PASS |
| IMDSv2 required; EBS encrypted; least-privilege node policies | PASS |
| DB egress `0.0.0.0/0`, public subnets with public IPs (deliberate, no NAT) | accepted risk |
| NodePorts 30080–30082 survive in the overlay; unreachable only while nothing is added to the EKS cluster SG | accepted risk |
| DB password was in rendered `user_data` and is in local Terraform state | **user_data: FIXED** after this report (Secrets Manager, instance-role read of one secret; see `FIRST-CLOUD-SESSION-REPORT.md`). **State: still sensitive**, documented in `main.tf` and `LAPTOP-RUNBOOK.md` |
| Engine↔DB `sslmode=disable` inside the VPC | accepted risk, pilot only |
| Evidence redaction | PASS (0 hits for the live password) |

---

## 8. Cloud-only unknowns (stage G must check before measuring)

1. API-server pod proxy to `:8080` and `create --raw` POST on EKS.
2. kubelet Summary API through the node proxy on EKS 1.37.
3. Kubernetes 1.37 available in us-east-1 standard support (re-run `aws eks describe-cluster-versions`).
4. `pgvector/pgvector:pg16` pulls from Docker Hub on nodes and the DB host; this session hit Docker Hub **429** rate limiting.
5. VPC CNI NetworkPolicy enforcement (`eks_fault.sh verify-policy`).
6. `pg_read_file('/proc/...')` on the EC2 host (here it read the container host, not a dedicated DB host).
7. Real scheduling: three engine pods on three nodes, generator on the harness node.

## 9. Remaining blockers and open risks

* **Blocker:** §3 fixes uncommitted (publish refuses a dirty tree).
* Two acceptance tests remain BLOCKED (§5).
* Ring skew (V7) confounds engine-scaling ratios; report against `shard_skew`.
* Single database, no standby: no HA claim is possible from this pilot.
* Local observation, not a result: timer lag p50 ≈ 0.7 s for a 50 ms timer under 64 concurrent workflows.

## 10. First AWS deployment (from the operator laptop, in `engine/`)

```bash
curl -s https://checkip.amazonaws.com
```
```bash
printf 'operator_cidr = "157.50.241.83/32"\nenable_db_ssh = false\n' > deploy/aws/eks/terraform.tfvars
```
```bash
terraform -chdir=deploy/aws/eks init
```
```bash
terraform -chdir=deploy/aws/eks plan -out=eks.tfplan
```
```bash
DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_publish.sh
```
```bash
terraform -chdir=deploy/aws/eks apply eks.tfplan
```
```bash
aws eks update-kubeconfig --region us-east-1 --name durexec-bench
```
```bash
kubectl get nodes -L durexec.io/role
```
```bash
DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_deploy.sh
```
```bash
scripts/aws/eks_smoke.sh
```
```bash
DUREXEC_CONFIRM=i-understand-this-costs-money KIND=smoke scripts/aws/eks_loadgen.sh
```
```bash
scripts/aws/collect_eks_evidence.sh pilot-bringup
```
```bash
DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_teardown.sh
```

The plan must say 27/0/0 before apply. Stop at any FAIL and tear down.

## 11. Cost and cleanup

Approximate on-demand us-east-1 rates — verify against `COST-ESTIMATE.md`
before starting: EKS control plane $0.10/h; 3 × m7i.large engines, 1 ×
c7i.large harness, 1 × m7i.large database ≈ $0.49/h; ~6 public IPv4
addresses ≈ $0.03/h; ~130 GB gp3 ≈ $0.01/h. **About $0.63/h while up.**
ECR storage and any snapshot persist after teardown. Cost reports must filter
by `eks:cluster-name` as well as `Project` (node instances do not carry the
Project tag). Teardown must run from the laptop (V6); read every sweep line.
