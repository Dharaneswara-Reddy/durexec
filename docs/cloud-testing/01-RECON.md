# Phase 1 — Reconnaissance and Baseline

Recorded 2026-10-26. This is the provenance record every later benchmark cites.

## Source under test

| Field | Value |
|---|---|
| Commit SHA | `fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac` |
| Branch | `main` |
| Commit date | 2026-10-26 19:44:05 +0530 |
| Tracked tree | **Clean** |
| Untracked | `Report_Template_AIDS_LaTeX/` (user's LaTeX template — left untouched) |
| Stash | `stash@{0}` CommitDecision batching (D1) — **preserved, not applied** |
| Unpushed commits | 9, from `b599c1f` onward |

## Build and test state (verified, not assumed)

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go build ./...` | OK |
| `go vet ./...` | OK |
| `go test ./...` | **26 packages pass**, 0 failures |
| Go toolchain | go1.25.10 linux/amd64 |

## Host capacity — this is a measurement constraint, not a detail

| Resource | Value |
|---|---|
| CPU | 12 logical cores |
| RAM total | 15 GiB |
| RAM **available** | **2.8 GiB** |
| Disk free | 66 GiB of 451 GiB (86% used) |

Memory is consumed by desktop applications, not the engine: Firefox ~2.9 GB
across content processes, VS Code ~1.3 GB. All ten running Docker containers
together use ~560 MB.

**Consequence.** A minikube cluster running 4 engine replicas plus PostgreSQL
plus Prometheus plus Grafana plus a load generator cannot be given enough
memory on this host to produce throughput numbers that reflect the engine
rather than host contention. There is already direct evidence of this
sensitivity in `docs/results/`: the same binary at one shard produced
**0.00 wf/s and 8.85 wf/s** in different runs depending on background load.

Therefore this host is used for **functional validation, correctness-oracle
runs, and fault injection** — all of which are load-insensitive — and the
**scaling matrix is deferred to cloud**, where replica and database capacity
can be isolated. Absolute throughput measured here is explicitly marked
non-publishable.

## Tooling available

| Tool | State |
|---|---|
| kubectl | v1.32.3 (kustomize v5.5.0 built in) |
| minikube | v1.35.0 — **host currently Stopped** |
| docker | 28.5.1 |
| podman | present |
| kind / k3d / helm | **not installed** |

Kustomize ships inside kubectl, so manifests use kustomize overlays rather
than Helm. No new tooling needs installing.

## Architecture map

Established by code inspection, with file references.

| Concern | Where | Note |
|---|---|---|
| Startup / shutdown | `cmd/engined/main.go:90`, `:443` | `signal.NotifyContext` on SIGINT/SIGTERM, then HTTP `Shutdown` with a grace timeout. Kubernetes-compatible. |
| Hash ring | `cmd/engined/main.go:255` | `shard.NewRing(cfg.ClusterNodes, DefaultVNodes)` |
| Peer forwarding | `cmd/engined/main.go:365-375` | `PeerAddr` builds `http://<NODE_ID>:<port>` |
| Peer health | `cmd/engined/main.go:963-975` | `GET http://<node>/healthz` |
| Decision commit | `internal/eventlog/commit.go` | `verifyToken ... FOR UPDATE`, then all writes, one transaction |
| Execution store | `internal/core/ports.go:90` | **Read-only by design** — `Create` was deliberately removed |
| Task queue | `internal/taskq/` | Lease queue, `FOR UPDATE SKIP LOCKED` |
| Reconciliation | `internal/runtime/reconciler.go` | Recovers orphans; never writes an event or sets a status |
| Migrations | `migrations/`, applied at boot | **No `schema_migrations` table** — every migration re-runs on every boot |
| Metrics / health | `internal/obs/metrics.go`, `/healthz`, `/metrics` | See gap table below |
| External effects | `internal/extsim/` | Append-only ledger independent of the engine journal |

### Configuration surface (what Kubernetes must supply)

`ACTIVITY_WORKERS`, `CLUSTER_NODES`, `DATABASE_URL`, `EMBEDDER_URL`,
`ESCALATION_BLOCKS`, `FAULT_INJECTION`, `LEASE_RENEW`, `LEASE_TTL`,
`LISTEN_ADDR`, `NODE_ID`, `SEMANTIC_HYBRID`, `SHARD_COUNT`, `SYNC_COMMIT`,
`TASK_LOCK`, `TAU_HIGH`, `TAU_LOW`, `WORKFLOW_WORKERS`.

## Finding R-1 — a StatefulSet is architecturally required

`NODE_ID` must be unique, stable, **and DNS-resolvable**. The code says so in
its own comment at `cmd/engined/main.go:362`:

> "Every node listens on the same port, and in the compose network a node id is
> also its DNS name, so the name plus that port is the address."

`CLUSTER_NODES` is a **static list** of those ids, and every node builds its
hash ring from it. A plain Deployment generates random pod names
(`durexec-7d8f9-x2k4p`), so it cannot satisfy either requirement.

A StatefulSet plus a headless Service does: pods get stable ordinals
(`durexec-0`) and resolvable DNS (`durexec-0.durexec-hl`). `NODE_ID` is then
derived with the downward API and `$(VAR)` interpolation.

### Finding R-2 — horizontal autoscaling is impossible as designed

Because ring membership comes from static configuration shared by all nodes,
changing the replica count requires editing `CLUSTER_NODES` and restarting
every pod. An HPA would add pods that no other node has in its ring, and whose
shards nothing would route to.

This is a genuine architectural limitation, recorded here rather than
discovered mid-benchmark. It answers part of mission question 2 before any
measurement: replica scaling is a **planned, disruptive reconfiguration**, not
an elastic operation.

## Existing assets that must be extended, not duplicated

- `engine/Dockerfile` — already multi-stage, distroless, non-root (uid 65532),
  static (CGO off), `-trimpath`, no secrets, exec-form entrypoint, HEALTHCHECK
  via a purpose-built static probe binary. Satisfies nearly all of spec §3.
  Missing only: recorded image **digest** per run.
- `engine/cmd/loadgen/` — ~115 KB across 15 files. `arrival.go` is already an
  **open-loop** generator that tracks `Scheduled`, `Issued`, `Dropped`,
  generator lag p50/p99, and gates on drop fraction, lag versus inter-arrival
  gap, and a declared `-service-floor`. This is coordinated-omission aware.
- `engine/deploy/` — Prometheus config, Grafana dashboard + provisioning,
  replication and promotion scripts, duplicate-audit SQL.

### Load generator: spec §2.3 conformance

| Requirement | State |
|---|---|
| Offered arrival rate | present (`-rates`, `-mode arrival`) |
| Duration, warmup, grace | present |
| Concurrency limit | present (`-concurrency`) |
| Request timeout | present (`-timeout`) |
| Shard / ID distribution | present (`-shards`, `-shard-ids`, `-spread`, `-id-prefix`) |
| Repeated identities | present (`-mode duplicates`) |
| CSV output | present (`-csv`) |
| Open loop + saturation gates | present (`-service-floor`) |
| Scheduled vs actual send time | present (lag p50/p99) |
| **Deterministic seed** | **missing** |
| **Payload size** | **missing** |
| **Total workflow count cap** | **missing** |
| **Ramp-down phase** | **missing** |

### Metrics: spec §6 conformance

Only **7 metric families** exist today:
`dedup_latency_seconds`, `lease_transitions_total`, `replay_duration_seconds`,
`shard_recovery_seconds`, `task_queue_depth`, `worker_task_failures_total`,
`workflow_completed_total`.

Spec §6 asks for roughly sixty. The large gaps are workflow starts
attempted/accepted/rejected, task claim/retry counters, oldest-queued-task
age, poll duration and bytes read, fencing rejections, ownership transitions,
commit duration, and all PostgreSQL and infrastructure metrics. PostgreSQL and
container metrics require exporters that are not deployed.

## Baseline to reproduce, not to assume

Historical figures from `docs/results/scaling-saturated-after-liveness-fix.txt`:

| Shards | Reported wf/s | Claim |
|---|---|---|
| 1 | 12.80 | saturated |
| 2 | 23.15 | saturated |
| 4 | 46.30 | **not proven saturated** |

A separate run in the same file records 8.85 / 7.35 / 16.95 wf/s for the same
shard counts — so the spread between runs on this host is roughly **1.4×**.
These are not reproduced as a comparison baseline here, because the host
condition that produced them cannot be restored. They are retained as
historical record only.

## Phase 1 verdict

Nothing unexpected in the working tree. Build, vet, format and all 26 test
packages pass at the recorded SHA. Proceeding to Phase 2.

Two things were learned that change the plan rather than follow it: the
StatefulSet requirement (R-1) and the autoscaling impossibility (R-2).
