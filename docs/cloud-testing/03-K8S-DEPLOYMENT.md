# Phase 4-5 — Kubernetes Deployment and First Measured Run

Commit `fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac` · recorded 2026-10-26

## Environment (recorded for provenance, spec section 12)

| Field | Value |
|---|---|
| Cluster | minikube v1.35.0, **kvm2** driver (pre-existing profile) |
| Kubernetes | v1.32.0 |
| Node capacity | **2 vCPU, 3.8 GiB** — fixed; minikube cannot resize an existing cluster |
| Engine image | `durexec-engine:local`, id `sha256:b358721da88758f1a18643369d114a39b47761f4f971d10608b0525ebd629c74` |
| PostgreSQL | `pgvector/pgvector:pg16`, 8 GiB PVC, `max_connections=250` |
| Replicas / shards | 2 / 16 |
| Registry | **Docker Hub unreachable from the VM** — images are side-loaded with `minikube image load` |

Note: the cluster also holds abandoned pods from ~590 days ago in `default`
(mongodb, sample-app, stress-pod, prometheus-operator), all stuck in Error or
ContainerCreating. They are **not touched**; the lab lives entirely in the
`durexec-lab` namespace so teardown is one namespace delete.

## Deployment shape

Ten objects, rendered by `kubectl kustomize engine/deploy/kubernetes/base`:
Namespace, ServiceAccount (no token mounted), two ConfigMaps, headless Service
`durexec-hl`, Service `durexec`, Service `postgres`, StatefulSet `durexec`,
StatefulSet `postgres`, PodDisruptionBudget.

Secrets are created at deploy time by `experiments/scripts/lab.sh` from a
generated password. **No credential is committed or baked into an image.**

### Why a StatefulSet (finding R-1, now validated)

`NODE_ID` is set to `\$(POD_NAME).durexec-hl` with the downward API, giving
`durexec-0.durexec-hl` — simultaneously a stable identity and a resolvable DNS
name, which is what `PeerAddr` at `cmd/engined/main.go:365` requires.

**Validated:** 16 shards were claimed and split across both pods by the hash
ring, and the split was **identical across two independent deployments**
(7 / 9), confirming the ring is deterministic.

```
owner_node             shards  fencing_token
durexec-0.durexec-hl        7              1
durexec-1.durexec-hl        9              1
```

The 7/9 split rather than 8/8 is consistent-hashing imbalance with virtual
nodes: **12.5% skew at 2 nodes**. Worth carrying into the hot-shard profile.

### Finding R-5 — a startup race that looked like a crash loop

The first deployment showed **2 restarts on both engine pods**. Cause, from the
previous container's final log line:

```
engined: ping database: failed to connect: hostname resolving error:
  lookup postgres.durexec-lab.svc.cluster.local: no such host
```

With `podManagementPolicy: Parallel`, engine pods start before the postgres
Service has DNS. The engine correctly exits 1, and Kubernetes reports
`CrashLoopBackOff`. It self-heals, **but a restart caused by startup ordering
would be misattributed to an injected fault** in Phase 9.

Fixed with a `wait-for-postgres` initContainer that blocks on `pg_isready`.
The gate is in Kubernetes, not in the engine: fail-fast on an unreachable
database is correct behaviour and must not be softened into a retry loop that
would also mask a genuinely misconfigured database.

**Verified from a clean slate:** teardown, redeploy, **0 restarts**, 18 s to
both pods ready.

### Probes — three distinct concerns, confirmed in the running cluster

`/healthz` returns 200 whenever the server is up and reports ownership as a
**body field**, not a status code:

```json
{"status":"ok","node":"durexec-1.durexec-hl","owned_shards":9,"cluster_shards":16}
```

So readiness genuinely does not imply shard ownership. A pod owning zero shards
stays in the Service and forwards to the owner, which is correct.

## First measured run on Kubernetes

Stage 1 functional, open-loop generator run **on the host** (minikube is a VM,
so generator CPU cannot distort engine measurements — better isolation than an
in-cluster Job).

| Metric | Value | Kind |
|---|---|---|
| Started | 112 | measured |
| **Completed in window** | **96** | measured |
| Errored | **0** | measured |
| Terminal but not COMPLETED | **0** | measured |
| Routing 409 follows | **0** | measured |
| Latency p50 | **2.0287 s** | measured |
| Latency p95 | 2.7212 s | measured |
| Latency p99 | 3.3352 s | 96 samples — worst case, not a true p99 |

Window 25 s after a 5 s warmup; 4 shards driven; concurrency 8; prep timer
forced to 200 ms (the workflow's own default is 12 min); amount 2450 cents.

**Cross-check.** p50 of 2.03 s agrees with the ~2.01 s workload floor measured
previously under docker-compose. The Kubernetes deployment therefore behaves
like the validated compose deployment rather than introducing a new latency
regime.

### What this run does NOT show

The generator said so itself, and it is repeated here rather than buried:

> shards 0,3,1,2 owned by 2 node(s); fewer owners than shards, so this point
> measures one machine more than once and is **NOT evidence of horizontal
> scaling**

No throughput figure from this host is publishable. The node has **2 vCPU**,
shared with the control plane, PostgreSQL and both engine pods. Section
"Host capacity" of `01-RECON.md` applies in full.

## Also validated incidentally

A workflow submitted with a malformed input (`amount_cents` absent) failed
**non-retryably**, recorded `ActivityTaskFailed` with
`non_retryable: true` and then `WorkflowExecutionFailed`, with the cause in
the payload — and did **not** enter a retry storm. Input validation and
non-retryable failure handling both work on Kubernetes.

## Resume

```bash
# bring the lab up / check it / tear it down
./experiments/scripts/lab.sh up
./experiments/scripts/lab.sh status
./experiments/scripts/lab.sh down

# reach the API from the host
kubectl -n durexec-lab port-forward service/durexec 18080:8080

# repeat the Stage 1 functional run
go build -o /tmp/loadgen ./engine/cmd/loadgen
/tmp/loadgen -mode throughput -base http://localhost:18080 \
  -shards 4 -concurrency 8 -duration 30s -warmup 5s -grace 45s \
  -prep 200ms -timeout 20s -csv experiments/results/k8s-stage1-functional.csv
```

## Next

Phase 6 (metrics gap — only 7 families exist of ~60 the spec asks for, and no
PostgreSQL or container exporters are deployed), then Phase 9 fault injection,
which this host **can** run validly because recovery correctness is not
load-sensitive.
