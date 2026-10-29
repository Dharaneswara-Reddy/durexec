# Test architecture options

What each deployment shape can and cannot prove, costed from the verified
prices in `INSTANCE-COMPARISON.md`.

---

## What the current Terraform actually builds

Traced in `deploy/aws/terraform/main.tf`:

- **One** `aws_instance.engine`. No `count`, no `for_each` on it.
- The only two `count` expressions in the file are `var.enable_ssh ? 1 : 0`,
  on the key pair and the SSH ingress rule. Neither multiplies the instance.
- `docker-compose.cloud.yml` then runs **eight containers on that one host**:
  `node-a`, `node-b`, `node-c`, `postgres-leader`, `postgres-standby`,
  `prometheus`, `grafana`, and `extsim` behind a profile.

**Three engine containers on one VM are one failure domain, not three.**
Stopping the instance stops the engine, the database, the standby and the
monitoring simultaneously. Nothing survives to observe the failure.

---

## What Tier 1 genuinely tests on one instance

Traced from `scripts/aws/acceptance_tier1.sh`, not inferred from test names.

| Case | What the code actually does | What it proves |
|---|---|---|
| T1.5 | `docker kill --signal=KILL node-b`, waits 25 s (> the 10 s lease TTL), asserts 6/6 workflows COMPLETED and that duplicate `(run_id, seq)` groups number 0 | **Genuine.** A real SIGKILL of a real process holding live leases; recovery with no duplicate journal entries |
| T1.6 | Kills `node-c`, diffs `GET /v1/shards` before and after. Asserts shards moved, **no token regressed**, every moved shard's token bumped, none left unowned | **Partial.** Proves tokens advance monotonically on ownership change. Does **not** prove a fenced writer is *rejected* — nothing stale attempts a write |
| T1.7 | `docker restart durexec-postgres-leader-1`, then starts a workflow | Database **process** restart. Not host loss, not standby promotion |
| T1.1–T1.4, T1.8 | Health, durable start and completion, duplicate starts, cross-node concurrency, leftover work | Genuine, and host count is irrelevant to them |

The SIGKILL choice is deliberate and the script says so: a graceful stop would
let the node drain and release its leases, which is the opposite of the
failure being tested.

---

## The gap, stated precisely

| Failure mode | One VM | Two VMs | Three VMs |
|---|---|---|---|
| Engine process death | yes (T1.5) | yes | yes |
| Lease expiry, token advance | yes (T1.6) | yes | yes |
| Replay correctness | yes | yes | yes |
| Database process restart | yes (T1.7) | yes | yes |
| **Fenced writer actually rejected** | **no** | needs a stale-worker thaw | same |
| **Host loss with survivors** | **no** — all dies together | **yes** | **yes** |
| **Network partition between nodes** | **no** — shared bridge | **yes** | **yes** |
| **Database host loss ≠ process loss** | **no** | only if the DB is alone | **yes** |
| **Standby promotion after real leader-host loss** | **no** | partial | **yes** |

**Three of those rows are impossible on one VM at any instance size.** That,
not CPU, is the argument for more machines.

---

## Option A — one t3.medium (the current plan)

```
+-- t3.medium --------------------------------+
| node-a   node-b   node-c                    |
| postgres-leader   postgres-standby          |
| prometheus   grafana   [extsim]             |
+---------------------------------------------+
```

| | |
|---|---|
| Cost | **$0.0510/hr** — $0.31 for 6 h, $1.22 for 24 h |
| Engine gets | ~0.9 GB of 4 GiB; one physical core shared by everything |
| Proves | T1.1–T1.8 in full |
| Cannot prove | Host loss, partition, database-host loss |
| Verdict | **Correct first step.** The cheapest thing that runs the real stack |

---

## Option B — two t3.medium

```
+-- VM-A --------------+   +-- VM-B -------------+
| postgres-leader      |   | node-b    node-c    |
| postgres-standby     |   | prometheus  grafana |
| node-a               |   | [extsim]            |
+----------------------+   +---------------------+
```

| | |
|---|---|
| Cost | **$0.0976/hr** — $0.59 for 6 h, $2.34 for 24 h |
| Proves | Everything in A, **plus host loss with survivors** (kill VM-B; node-a and the database live on) and **a real network partition** |
| Limitation | **The database shares VM-A with node-a.** Killing VM-A kills the database too, so "database host loss" and "total cluster loss" become the same event. The topology is asymmetric: one host is destroyable, the other is not |
| Verdict | **Best value per dollar.** Buys the single most important missing capability for $0.28 more per six-hour session |

---

## Option C — three t3.medium

```
+-- VM-DB ---------+  +-- VM-1 ----+  +-- VM-2 ----+
| postgres-leader  |  | node-a     |  | node-b     |
| postgres-standby |  | prometheus |  | node-c     |
+------------------+  +------------+  +------------+
```

| | |
|---|---|
| Cost | **$0.1464/hr** — $0.88 for 6 h, $3.51 for 24 h |
| Proves | Everything in B, **plus** engine-host loss cleanly independent of the database, **plus** a majority of engine nodes surviving any single host failure |
| Limitation | Leader and standby still share VM-DB, so **true database-host failover remains untestable**. Separating them needs a fourth VM |
| Verdict | **The right shape for the dissertation's distributed claim** — and still only $3.51 for a full day |

---

## Option D — four VMs, standby separated

`VM-DB-leader`, `VM-DB-standby`, `VM-1`, `VM-2`.

| | |
|---|---|
| Cost | **$0.1930/hr** — $1.16 for 6 h, $4.63 for 24 h |
| Proves | Everything in C, **plus genuine standby promotion after real leader-host loss** — the one result currently worse than AMBROSIA and marked NOT TESTED in `RESULTS_SUMMARY.md` §10 |
| Limitation | Promotion is still **manual**; this tests the drill, not automation |
| Verdict | **Worth one short session only.** Run C for the campaign, then D for a single two-hour failover drill at about $0.39 |

---

## Why not Kubernetes or EKS

> **Superseded.** The $73/month figure below is the cost of leaving EKS
> running. At **$0.10/hour** a 10-hour campaign costs $1.00, which is
> affordable. See **[KUBERNETES-TEST-PLAN.md](KUBERNETES-TEST-PLAN.md)**.
> The scheduling objection still stands and is handled there.

**Plain EC2 with Docker Compose is sufficient, and EKS would be actively wrong
here.**

- EKS costs **$0.10/hr for the control plane alone** — $73/month, more than
  twice the entire budget, before a single worker node exists.
- The engine resolves peers by node ID as a DNS name
  (`cmd/engined/main.go`). Compose's embedded DNS supplies that for free;
  across VMs an `/etc/hosts` entry or a Route 53 private zone does the same.
- Kubernetes would **reschedule a killed pod automatically, hiding the failure
  being measured**. The point is to observe lease expiry and ownership
  transfer, not to have an orchestrator paper over them.
- The project already has Kubernetes evidence: Phase 6 ran on minikube.
  Nothing new is learned by repeating it at cost.

Orchestration begins to pay only with dozens of nodes, rolling upgrades, or
multi-tenancy. None of those applies.

---

## Recommendation

**Start at A, spend the campaign on C, finish with one short D session.**

| Step | Shape | Hours | Cost |
|---|---|---|---|
| 1 | A — confirm the stack really runs | 2 | $0.10 |
| 2 | C — the real multi-host campaign | 6 | $0.88 |
| 3 | D — one failover drill | 2 | $0.39 |
| | | | **$1.37** |

Under 5 % of a $31 balance. The budget is not the constraint; **operator time
and credit expiry are.**

---

## What this costs in Terraform work

Options B, C and D need the existing module changed from one instance to a
counted set, plus a private-DNS mechanism so node IDs resolve across hosts.
That is a real change to `main.tf`, `user_data.sh.tftpl` and
`docker-compose.cloud.yml`, and it is **not yet written**. Estimated effort:
half a day. It must be done and validated locally before any apply.
