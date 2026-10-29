# Kubernetes test plan on AWS (EKS)

The rigorous, cloud-only scaling and fault campaign. Supersedes the
single-instance Tier 1 plan for everything except the initial smoke test.

**Correction to earlier advice.** `TEST-ARCHITECTURE-OPTIONS.md` rejected EKS
at "$73/month". That is the figure for leaving it running. The real rate is
**$0.10/hour**, so a 10-hour campaign costs **$1.00** of control plane. For
burst testing EKS is affordable, and the earlier rejection reasoned about the
wrong usage model.

---

## Why Kubernetes is the right call here

| Reason | Detail |
|---|---|
| **The manifests already exist** | `deploy/kubernetes/` has engine and Postgres StatefulSets, NodePort services, extsim, Prometheus **and postgres-exporter**, all from Phase 6 |
| **Pod-to-node placement is explicit** | `nodeSelector` and anti-affinity put exactly N engine pods on exactly M nodes, and let you prove they landed there |
| **Node drain is a real failure mode** | `kubectl drain` is a graceful eviction; `terminate-instances` is a hard loss. Both are testable, and they are different failures |
| **It is what production looks like** | "Would this work in a real deployment?" gets a demonstrated answer rather than an argument |

**One thing to configure deliberately:** Kubernetes restarts killed pods
automatically, which can *mask* the lease-expiry recovery being measured. Use
`kubectl delete pod --force --grace-period=0` so the engine's own recovery is
what you observe, not the kubelet's. Measuring the interaction between the two
is itself a result worth reporting.

---

## The design flaw to avoid

**Shards do not create capacity.** They partition ownership. Throughput is
bounded by whichever of these saturates first:

1. Engine CPU running workflow code and replay
2. **PostgreSQL commit capacity** — WAL flush, `fsync`, connections, locks
3. Network round-trips per decision commit

Every prior measurement saturated on **(2)**. Deploying 48 shards across three
large nodes against **one** Postgres pod reproduces that wall at higher cost.

> **The database must therefore be an experimental variable, not a constant.**
> Otherwise this measures one Postgres pod, not the engine's scalability.

---

## Cluster architecture

```
EKS control plane  ($0.10/hr)
|
+-- nodegroup "engine"   3 x m7i.large   (2 vCPU, 8 GiB each)
|     engine StatefulSet, 3-12 pods, anti-affinity across nodes
|
+-- nodegroup "data"     1 x m7i.xlarge  (4 vCPU, 16 GiB)
|     postgres-leader + standby StatefulSets, gp3 PV
|
+-- nodegroup "harness"  1 x c7i.large   (2 vCPU, 4 GiB)
      loadgen, Prometheus, Grafana, extsim
```

**Three placement rules that make the numbers trustworthy:**

1. **The load generator never shares a node with the engine.** Otherwise you
   measure contention between them. That is how the concurrency-16 run became
   invalid.
2. **Prometheus never shares a node with the engine.** Phase 6 recorded the
   observer evicting the system being observed.
3. **Postgres gets a dedicated node.** Its `fsync` latency is the suspected
   bottleneck; sharing CPU with engine pods makes that unmeasurable.

**Use public subnets with per-node public IPs, not private subnets.** A NAT
gateway costs $0.045/hr plus $0.045/GB — more than the entire engine
nodegroup. Five public IPv4 at $0.005/hr is $0.025/hr.

---

## Cost

Pricing-API rates, us-east-1, on-demand Linux, plus the published EKS rate.

| Component | Qty | $/hr |
|---|---|---|
| EKS control plane | 1 | 0.1000 |
| m7i.large engine nodes | 3 | 0.3024 |
| m7i.xlarge data node | 1 | 0.2016 |
| c7i.large harness node | 1 | 0.0892 |
| gp3 storage, ~250 GB | — | 0.0274 |
| Public IPv4 | 5 | 0.0250 |
| ECR image storage | — | ~0.0001 |
| **Total** | | **≈ $0.746/hr** |

| Session | Hours | Cost |
|---|---|---|
| Shakedown | 4 | **$2.98** |
| Scaling matrix | 8 | **$5.97** |
| Fault injection at scale | 6 | **$4.48** |
| Database scaling | 6 | **$4.48** |
| **Four sessions** | 24 | **$17.91** |

**Against $31 this fits with $13 to spare.** Adding your own money is **not
required**. Revisit that only if session 4 shows you need a much larger
database tier, or if you want a multi-day soak.

---

## Experiment matrix — one variable at a time

Every row runs a concurrency sweep until the knee is found. **A point where no
resource is above 85 % is generator-bound and must be discarded**, exactly as
the concurrency-16 run was.

### E1 — shard granularity (engine nodes fixed at 3)

| Run | Total shards | Shards/node | Question |
|---|---|---|---|
| E1.1 | 12 | 4 | Baseline |
| E1.2 | 24 | 8 | Does finer partitioning help? |
| E1.3 | 48 | 16 | Does it start to hurt? |

> **Hypothesis REFUTED by local measurement, 2026-10-29.** This plan predicted
> flat or negative scaling past the point where shard count exceeds concurrent
> workflows. A local sweep measured the opposite: throughput rose monotonically
> 14.70 → 28.85 → 47.15 wf/s across 4 → 8 → 16 shards with latency falling
> throughout, while the connection budget stayed fixed at 20 per node. Shard
> count is a productive variable. See
> [`../results/local-shard-sweep/FINDINGS.md`](../results/local-shard-sweep/FINDINGS.md).

> **And a methodological correction.** At 16 shards, raising concurrency from
> 128 to 256 raised throughput **2.74×** (47.15 → 129.30 wf/s) while *lowering*
> p99 latency. A shard sweep at fixed concurrency is therefore
> **offered-load-limited** and confounds shard count with offered load.
> **Every shard-count point must get its own concurrency sweep.**

### E2 — node scaling (total shards fixed at 48)

| Run | Engine nodes | Pods | Question |
|---|---|---|---|
| E2.1 | 1 | 4 | Single-node ceiling |
| E2.2 | 2 | 8 | Ratio against E2.1 |
| E2.3 | 3 | 12 | Ratio against E2.2 |

**This is the headline scaling curve.** Report ratios, not absolutes.

### E3 — database scaling (engine fixed at the best config from E2)

| Run | Postgres node | Question |
|---|---|---|
| E3.1 | m7i.large (2 vCPU) | — |
| E3.2 | m7i.xlarge (4 vCPU) | Does throughput move? |
| E3.3 | m7i.2xlarge (8 vCPU) | Does it move again? |
| E3.4 | `synchronous_commit=off` | **Cost of durability, measured** |

**This is the experiment that makes the study honest.** If throughput tracks
the database rather than the engine nodes, the correct finding is *"the engine
scales until the shared database saturates, at N wf/s"* — a stronger and far
more defensible claim than an unqualified scaling number.

### E4 — fault injection at scale (best config)

| Run | Fault | Proves |
|---|---|---|
| E4.1 | `kubectl delete pod --force --grace-period=0` | Lease expiry and replay under load |
| E4.2 | `kubectl drain` an engine node | Graceful eviction, ownership redistribution |
| E4.3 | `terminate-instances` on an engine node | **Hard host loss with survivors** |
| E4.4 | NetworkPolicy isolating one node from the DB | **Partition; no split-brain writes** |
| E4.5 | Delete the Postgres leader pod | Process restart and reconnection |
| E4.6 | Terminate the data node | **Standby promotion after real host loss** |

E4.3, E4.4 and E4.6 are the three failure modes **impossible on one machine**.
They are the reason this campaign exists.

---

## Metrics — collect all of these in the same window

Bottleneck attribution only works if every layer is sampled together.

| Layer | Metric | Source |
|---|---|---|
| **Client** | offered/s, completed/s, p50/p95/p99, errors, 409 rate, in-flight slope | `cmd/loadgen` |
| **Engine** | CPU %, RSS, goroutines, **GC pause p99**, `replay_duration_seconds`, `seq_race`, `task_lock_lost` | `/metrics`, cAdvisor |
| **Queue** | pending tasks, claim latency, `visible_at` lag, attempt distribution | `tasks` table |
| **Postgres** | `xact_commit`/s, WAL bytes/s, **backend fsync time**, checkpoints, active vs max connections, lock waits, buffer hit ratio | postgres-exporter (**already deployed**) |
| **Disk** | IOPS, await, queue depth on the DB volume | node-exporter — **must be added** |
| **Network** | pod→pod and pod→DB RTT, bytes/s | `iperf3` probe — **must be added** |
| **Correctness** | duplicate `(run_id, seq)`, pending tasks, non-terminal workflows, extsim `Performed`/`Unkeyed` | SQL plus the extsim ledger |

**Two gaps to close before the campaign:** node-exporter for disk, and a
network probe. Both are manifest additions, both free, both local work.

### The saturation rule

At the knee, **exactly one** of {engine CPU, DB CPU, DB fsync, disk await,
network} should be at or above 85 %. Write down which. If none is, the run is
**generator-bound and invalid** — raise concurrency and repeat.

---

## Session plan

| # | Session | Hours | Cost | Exit criteria |
|---|---|---|---|---|
| 0 | **Local harness validation** | — | $0 | Manifests apply to a local kind cluster; loadgen writes a CSV; every dashboard populates |
| 1 | Shakedown on EKS | 4 | $2.98 | Images in ECR; cluster up; one E2.3 run completes; every metric source reporting |
| 2 | Scaling matrix E1 + E2 | 8 | $5.97 | 6 configs swept; knee identified **and attributed** for each |
| 3 | Database scaling E3 | 6 | $4.48 | **Bottleneck proven or disproven** |
| 4 | Fault injection E4 | 6 | $4.48 | 6 faults; evidence collected **before** each recovery |
| | | **24** | **$17.91** | |

### On session 0

You said you want nothing tested locally, and for *results* that is exactly
right — no number in the dissertation should come from this laptop. But
session 0 is not measurement; it is **checking the harness has no typo**.
Phase 7 and Phase 8 each destroyed their own evidence through harness defects,
and the Phase 9 smoke script queried a column that does not exist. Finding
that on a $0.75/hour cluster costs money and, worse, a whole session.

**Validate the harness free; measure only in the cloud.**

---

## What must be built before session 1

| Item | Effort | Why |
|---|---|---|
| EKS cluster definition (`eksctl` or Terraform) | half a day | Does not exist |
| Three nodegroups with labels and anti-affinity | 2 h | Pods must land where intended, and be proven to |
| Push images to ECR | 1 h | EKS cannot read the local Docker daemon |
| EBS CSI driver addon | 15 min | PVCs do not bind without it |
| Parameterise `SHARD_COUNT` and replicas in kustomize overlays | 2 h | The matrix needs six or more configurations |
| node-exporter plus a network probe | 2 h | The two metric gaps above |
| Campaign runner: resumable, per-row CSV flush, evidence before recovery | half a day | A run killed at config 7 must leave 7 usable rows |

**Roughly two days of local work.** None of it is billable.

---

## Honest limits of this campaign

| Claim it cannot support | Why |
|---|---|
| "Scales linearly" | A shared database will bound it; E3 shows where |
| "Exactly-once effects" | Unchanged Phase 8 contract: at-least-once invocation, effectively-once effect **only when the recipient honours the key** |
| "Zero data loss" | Async replication loses 1–2 events; E4.6 measures it again |
| "High availability" | Database failover is **manual** |
| "Production ready" | A test cluster under induced failure is not production |
| Absolute throughput against published systems | Different hardware, different year. Report **ratios** and **correctness counts** |

---

## Stop conditions

Tear down immediately if any of these occurs:

1. Accumulated spend reaches **$25**.
2. A session overruns its plan by more than two hours.
3. `kubectl get nodes` shows a node you did not create.
4. A metric source is silently absent — **a missing series makes attribution
   impossible, so fix it before continuing rather than collecting
   uninterpretable rows.**
5. You will be away from the terminal for more than an hour.

Teardown must delete the **nodegroups, the cluster, the PersistentVolumes and
any snapshots**. An EKS cluster with no nodes still bills $0.10/hr, and a
released PV leaves its EBS volume behind.
