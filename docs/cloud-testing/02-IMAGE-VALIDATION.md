# Phase 3 — Container Image Validation

Commit `fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac` · recorded 2026-10-26

| Field | Value |
|---|---|
| Image | `durexec-engine:local`, `durexec-engine:fde6ae0` |
| Image ID | `sha256:b358721da88758f1a18643369d114a39b47761f4f971d10608b0525ebd629c74` |
| Base | `gcr.io/distroless/static-debian12:nonroot` |
| Build | multi-stage, CGO off, `-trimpath`, `-s -w` |

Validated by running the real image against a throwaway `pgvector/pgvector:pg16`
on an isolated Docker network. The project's demo stack was not touched.

## Spec section 3 checklist — all seven pass

| # | Check | Result |
|---|---|---|
| 1 | Container starts | **PASS** — both nodes `running`, HEALTHCHECK `healthy` |
| 2 | DB connect + schema init | **PASS** — 6 tables created: `events`, `executions`, `tasks`, `timers`, `shard_leases`, `activity_intents` |
| 3 | Multiple containers concurrently | **PASS** — two nodes, no contention errors |
| 4 | Shard ownership | **PASS** — 4 shards split 2/2 by the hash ring, leases live |
| 5 | Graceful stop | **PASS** — see measurement below |
| 6 | Abrupt termination recovery | **PASS** — see measurement below |
| 7 | Config errors fail clearly | **PASS** — exit 1 on all three, with actionable messages |

Also confirmed, because Kubernetes imposes them: the image runs with
`--read-only` root filesystem and `--user 65532:65532`. No write to the
container filesystem is needed at runtime.

### Check 7 detail

```
no DATABASE_URL      -> exit 1
  "DATABASE_URL must be set: the event log is the engine's only durable state"
no NODE_ID           -> exit 1
unreachable database -> exit 1
```

Fatal startup errors are not masked into a misleading healthy state.

## Measured: recovery latency (this is a result, not a checkbox)

| Scenario | Lease TTL | Time to full re-ownership | Fencing token |
|---|---|---|---|
| **Graceful** (SIGTERM) | 10 s | **3 s** | 1 -> 2 |
| **Abrupt** (SIGKILL) | 10 s | **29 s** | 2 -> 3 |

Two findings.

**Graceful shutdown explicitly releases leases.** `docker stop` returned in
**1 second**, and the departing node's rows were left with `owner_node = NULL`
and `expires_at` in the past — not left to expire. A rolling update therefore
does not wait out the lease TTL, which is why
`terminationGracePeriodSeconds: 45` is comfortable rather than tight.

**Finding R-4: abrupt-failure recovery takes ~3x the lease TTL.** 29 s against
a 10 s TTL. The TTL bounds when a lease becomes *claimable*, not when it is
*claimed* — the surviving node discovers it on its own scan interval. Anyone
reading `LEASE_TTL=10s` would reasonably predict ~10 s recovery and be wrong
by 19 s.

This is consistent with, and probably the same mechanism behind, the
9-15 s manual database failover already recorded in
`docs/BASELINE_SUMMARY.md`. It must not be described as "10 second failover".

Fencing tokens incremented monotonically on every transfer (1 -> 2 -> 3), so
the fencing mechanism behaved correctly under both shutdown modes.

## Fixes this phase forced into the manifests

| # | Finding | Fix |
|---|---|---|
| R-3a | Migration 0001 runs `CREATE EXTENSION vector`; `postgres:16-alpine` has no pgvector, so both nodes exited 1 | `20-postgres.yaml` now uses `pgvector/pgvector:pg16` |
| R-3b | Connection budget comes from `NODE_COUNT` (default **3**), not from `len(CLUSTER_NODES)` | `NODE_COUNT` added to `10-config.yaml`, must equal `.spec.replicas` |
| R-3c | Budget is `nodes x 24 + 26`; 8 replicas need **218**, over the 200 first written | `max_connections = 250` |

R-3b is the dangerous one: left unset, an 8-replica run passes a budget check
computed for 3 nodes. The engine refuses to boot when the budget does not fit,
which is good design — but only if the figure it checks is the true one.

## Resume

```bash
# rebuild and revalidate
cd engine && DOCKER_CONFIG=$(mktemp -d) docker build -t durexec-engine:local .
kubectl kustomize deploy/kubernetes/base   # manifests render
```
