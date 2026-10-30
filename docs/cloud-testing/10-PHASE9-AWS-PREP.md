# Phase 9 — AWS cloud deployment preparation

**Verdict: READY FOR AUTHORIZED CLOUD DEPLOYMENT. Nothing was deployed, no
cloud resource was created, no artifact was published, nothing was committed or
pushed.** `HEAD` is still `fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac` and
`stash@{0}` is untouched.

Not production readiness. Ready to be deployed and validated.

## What was found before anything was built

| Finding | Evidence | Consequence |
|---|---|---|
| **No EC2 free tier in this account** | `aws freetier get-free-tier-usage`: 5 entries, all `Always Free`; **zero** `12 Months Free`; no EC2/EBS/IPv4 row | Every cost figure assumes full on-demand price |
| Credit balance unreadable | These IAM credentials have no Billing access | Stated as a limitation, not guessed |
| **The account is shared** | 3 stopped instances (codearena, churn-ml-prod, aiops-demo), a second VPC on 10.20.0.0/16, 62 GB of EBS, 1 associated Elastic IP, 3 ECR repos | Dedicated VPC on 10.90.0.0/16; teardown scoped to Terraform state, never by pattern |
| ~$8.61/mo already accruing | 62 GB gp3 + 1 EIP, none of it durexec's | Recorded so it is not misread as this project's cost |
| `cli-deployer` has PowerUserAccess + IAMFullAccess | `iam list-attached-group-policies AIOps_Copilot` | Deployment is genuinely possible; no permission blocker |
| `org-admin` (ap-south-1) has no IAM read access | `AccessDenied` on three IAM calls | Use `aiops-deploy`/us-east-1 |
| **No IaC existed in the repository** | no `.tf`, no CloudFormation, no Ansible | Terraform added from scratch |
| **`extsimd` had no Dockerfile** | `durexec-extsimd:phase7` was built by hand and never committed | The image behind the Phase 7/8 external-effect evidence was not reproducible. `Dockerfile.extsimd` added |

The last one matters for the paper: Phases 7 and 8 ran against a 27.8 MB
locally-built image. The committed Dockerfile produces **8.05 MB** — same
`cmd/extsimd` source, distroless and stripped. Behaviour should be identical and
the local verification below is consistent with that, but the *exact* artifact
those phases used is gone. Rebuilt and re-verified rather than assumed.

## The architecture, in one line each

| Decision | Why |
|---|---|
| One `t3.medium`, Docker Compose | `cmd/engined/main.go` builds a peer URL as `http://<node_id>:8080`, so a node id must be a resolvable name. Compose's DNS gives that free; three EC2 instances would need Route 53 and validate nothing more in Tier 1 |
| Dedicated VPC, one public subnet | The account is shared |
| **Port 22 only**, everything on `127.0.0.1`, dashboards via SSH tunnel | The test-only simulator cannot become public because there is no route to it |
| PGDATA on a **separate EBS volume** | Makes instance loss a real experiment for $1.60/mo. Phase 8 recorded it NOT TESTED because minikube's hostPath died with its VM |
| No ECR, no RDS, no EKS, no load balancer, no Secrets Manager | Each adds cost without adding validation. RDS would also hide the `postgresql.conf` the durability audit must read, and would silently fix gaps the project must report |
| Terraform, local state | `plan` is reviewable without touching the account. State holds the generated passwords and is gitignored |

Rejected options and their prices are in `docs/cloud-deployment/ARCHITECTURE.md`.

## Local verification — what actually ran

| Check | Result |
|---|---|
| `terraform fmt -recursive` | Reformatted 3 files, then clean |
| `terraform validate` | **Success** (after two real defects, below) |
| `terraform plan` (read-only AWS calls) | **18 resources**; exactly **1 ingress rule** (tcp/22 from the given /32); **no `0.0.0.0/0` ingress**; both volumes encrypted; IMDSv2 `required`; only `AmazonSSMManagedInstanceCore` |
| `assert_no_public_bind.sh` | **PASS** — 7 published ports, all loopback; `extsim` publishes none; no Terraform rule names a service port |
| `build_images.sh` | engine **21.9 MB**, extsimd **8.05 MB**, both `linux/amd64`, tagged `fde6ae0-dirty` |
| extsimd behaviour, local container | Same key twice → **one** effect, `replayed:true`. Two unkeyed calls → **two** distinct effects |
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `go test ./... -p 2` | all 27 test packages ok |
| `go test -count=1 -race` on extsim, faultinj, config, api, runtime, activity, taskq | **clean** |
| `make test-integration` (throwaway PostgreSQL) | **1220 passed, 0 failed, 0 skipped, 2 blocked** |
| Teardown refusal without confirmation | Refused, exit 1, **reached no AWS API** |
| Cloud acceptance (Tier 1 and Tier 2) | **NOT RUN** — nothing is deployed. Not recorded as PASS |

## Defects found and fixed during this phase

| Defect | How it surfaced | Fix |
|---|---|---|
| `templatefile` rejected `${VAR}` | `terraform validate` | The comment *explaining* the escape was itself unescaped. Escaped it |
| `ami_id` output illegal | `terraform plan` | `aws_ssm_parameter.value` is always sensitive; a public AMI parameter wants `insecure_value` |
| Prometheus could not have scraped the host | review | Hard-coded `172.17.0.1` is the **default** bridge gateway; Compose uses a project network with a different one. Now `host.docker.internal` via `extra_hosts: host-gateway` |
| `smoke.sh` would have errored | reading the schema | Queried `tasks.completed_at`, **which does not exist** — a finished task is deleted, so any remaining row is work owed |
| `smoke.sh` shard check was wrong | reading `api/handlers.go:125` | `/healthz` returns `owned_shards` as an **integer count**, not a list. Rewritten to use `/v1/shards`, which is authoritative and carries owner and fencing token |
| f-string backslash | running the script | Python 3.11 rejects a backslash inside an f-string expression — the same trap as Phase 6's `promq.sh` |
| `.gitignore` claimed "every command directory is listed"; four were missing | audit | Added `extsimd`, `fieldeval`, `rephrase`, `testinventory`. A stray `extsimd` binary really did appear in Phase 8 |
| `require_confirmation` printed the description as a flag | running it | `$*` inside a function is the *function's* args |

Two of those — the missing `completed_at` column and the integer
`owned_shards` — would each have produced a cloud "failure" that was really a
bug in my own check. Both were caught by reading the engine's code instead of
trusting the shape I expected.

## A finding that understates the engine

`make test-integration` reports **2 BLOCKED** tests. One of them is no longer
blocked by missing plumbing:

`TestAcceptNoDuplicateExternalEffectBlocked` skips with *"needs the
external-effect simulator (a stub service counting calls by dedup key) and a
crash between the effect and its journal write."* **Both now exist** —
`cmd/extsimd` counts by dedup key (verified above) and
`faultinj.BeforeResultJournal` is exactly that boundary, which Phase 8 scenario
H exercised with a real `SIGKILL`. The engine can demonstrate this; the suite
still says it cannot.

**It was deliberately not implemented here.** The brief forbids adding
capability to turn the matrix green, and automating it is a change to the
acceptance suite, not deployment preparation. It is the highest-value next task.

`TestAcceptSagaRecoveryOnLiveClusterBlocked` is **genuinely** blocked: it
asserts a compensation is "neither skipped nor repeated against a real effect",
which is the ambiguous-outcome compensation question Phase 8 left open by
decision. That is a product call, not missing plumbing.

## Files added

```
engine/Dockerfile.extsimd                         reproducibility gap closed
engine/deploy/aws/docker-compose.cloud.yml        self-contained; all ports loopback
engine/deploy/aws/prometheus.cloud.yml            15s interval + host node_exporter
engine/deploy/aws/terraform/{versions,variables,main,outputs}.tf
engine/deploy/aws/terraform/user_data.sh.tftpl    refuses to format an existing filesystem
engine/deploy/aws/terraform/terraform.tfvars.example
engine/deploy/aws/terraform/.gitignore            state holds the passwords
engine/scripts/aws/_common.sh                     require_confirmation
engine/scripts/aws/{preflight_local,assert_no_public_bind,build_images,package_images}.sh
engine/scripts/aws/{provision,wait_ready,deploy_stack,smoke,acceptance_tier1,collect_evidence,teardown}.sh
docs/cloud-deployment/{README,ARCHITECTURE,COST-ESTIMATE,DEPLOYMENT-RUNBOOK,ACCEPTANCE-MATRIX}.md
docs/cloud-testing/10-PHASE9-AWS-PREP.md
```

Modified: `engine/Makefile` (5 local-only `aws-*` targets), `engine/.gitignore`,
`docs/results/README.md`. **No Go source was changed in this phase** — which is
why the test suite is the same suite Phase 8 left passing.

## Cost

| Scenario | Cost |
|---|---|
| One-day validation, then destroyed | **$1.22** |
| Seven days running, then destroyed | **$8.57** |
| One month left running | **$37.22** |
| Instance destroyed but data volume retained | **$3.20/mo for ever** |

$0.0416/hr t3.medium, $0.08/GB-mo gp3, $0.005/hr public IPv4 — all from the
account's own Pricing API. Budget ceiling **$15**. No budget alarm is created by
this Terraform: alarms are account-wide and need an email address, so it is the
account owner's call. The command is in COST-ESTIMATE.md.

## Remaining blockers

| Blocker | Nature |
|---|---|
| Nothing is deployed, so every Tier 1 and Tier 2 result is NOT RUN | Needs authorization. The only blocker to a green matrix |
| Cross-run idempotency (`dx2:`) | Designed, not built. A silent scheme switch would turn a safe retry into a duplicate effect |
| External-effect compensation | Blocked on compensating an ambiguous outcome. Tripwire test guards it |
| No PITR, no `data_checksums`, no off-node backup | Would change the database's durability config; needs explicit authorization |
| EBS volume loss | Not tested; would need a deliberate delete with only a snapshot to recover from |
| Automatic DB failover | 9–15 s and manual, worse than AMBROSIA's 1.8 s automatic. State first |
| No SDK, no workflow versioning | No story exists; none invented |
| 9 commits unpushed, CI never run | Unchanged from Phase 8 |

## Host notes

The development laptop hit **0 MB free swap** again during this phase, with
`/tmp/uv-cache` holding **971 MB of RAM** (`/tmp` is tmpfs). `minikube stop`
freed ~3.8 GB and the Phase 8 lab's disk and PVCs are preserved — restart it
with `minikube start` when needed. `/home` has **62 GB free**, re-verified, so
the durable fix is `mkdir -p ~/.cache/uv && export UV_CACHE_DIR=~/.cache/uv`.
No cache was deleted; moving it is the user's call.

Prometheus and `postgres-exporter` in the minikube lab remain scaled to **0**.
Restore with `kubectl -n durexec-lab scale deploy/prometheus deploy/postgres-exporter --replicas=1`
**after** `minikube start`, and only if memory allows.
