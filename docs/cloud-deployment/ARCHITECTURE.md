# Architecture — durexec AWS validation cluster

**Chosen: one EC2 `t3.medium` in a dedicated VPC, running the existing
Docker Compose stack — three engine nodes, a PostgreSQL leader, a streaming
standby, Prometheus and Grafana — with PGDATA on a separate EBS volume.
No Kubernetes, no load balancer, no RDS, no container registry.**

About **$1.22** for a one-day validation. Full numbers in COST-ESTIMATE.md.

## Why one instance, and not three

Not a cost shortcut — the engine's own code decides it.

`cmd/engined/main.go` builds a peer address as `"http://" + nodeID +
peerPort(...)`. A node id **is** a DNS name. The lease table stores only a node
name, never an address, so a node that does not own a shard forwards the
request by resolving the owner's *name*. Docker Compose's embedded DNS makes
`node-a`, `node-b`, `node-c` resolvable for free.

Three EC2 instances would need a Route 53 private hosted zone ($0.50/mo plus
queries) or hand-maintained `/etc/hosts` entries, three public IPv4 addresses
instead of one, and would validate **nothing in Tier 1** that one instance does
not. Shard ownership, leases, fencing, failover, forwarding and database
replication are all exercised either way.

## What this validates, and what it cannot

| Property | Status here | Why |
|---|---|---|
| Multi-node sharding, lease ownership, fencing | **Validated** | Three real engine processes, 16 shards, one lease table |
| Engine process death and recovery | **Validated** | `docker kill --signal=KILL` on one node |
| Forwarding to the owning node | **Validated** | Requests enter any node; non-owners forward |
| Database restart and reconnection | **Validated** | Restart the leader container |
| Streaming replication and promotion | **Validated** | Leader + standby, as in the compose demo |
| Backup and restore | **Validated** | `pg_dump`/`pg_restore` on the instance |
| **Instance loss** | **Newly testable** | PGDATA is on a detachable EBS volume — see below |
| **EBS volume loss** | **Not tested** | Would need a deliberate volume delete; recoverable only from a snapshot |
| Network partition *between engine nodes* | **Cannot** | They share one kernel and one bridge |
| Cross-AZ latency, AZ failure | **Cannot** | One instance, one AZ |
| Absolute throughput | **Not comparable** | Three nodes and two databases share 2 vCPUs. Worse than the laptop numbers, not better. Report scaling shape and correctness counts, never raw rate. |

**The one genuine improvement over the minikube lab:** Phase 8 recorded
instance loss as NOT TESTED because PGDATA lived on a hostPath on the single
minikube VM's disk. Here it is a separate EBS volume, so the instance can be
terminated and the volume re-attached to its replacement. "The machine died —
did we lose committed work?" becomes a real experiment for $1.60/mo.

## Why not the alternatives

| Option | Verdict |
|---|---|
| **ECS Fargate** | Rejected. ~$0.04/vCPU-hr plus memory, needs a load balancer (~$16/mo) or Service Connect for peer DNS, and task IPs are not stable node ids. More cost, more moving parts, same validation. |
| **EKS** | Rejected. $0.10/hr ($73/mo) for the control plane alone, before any node. Twice the whole budget for infrastructure the engine does not require. |
| **RDS for PostgreSQL** | Rejected, and this one matters. RDS does not give superuser, does not allow `pg_basebackup` against it for a self-managed standby, and hides the `postgresql.conf` the durability audit must read. Phase 8's findings about `archive_mode`, `data_checksums` and `wal_log_hints` are only checkable on a database we own. RDS would also *silently fix* some of them, which would misrepresent what the engine ships with. |
| **3 × t3.small + Route 53** | Rejected for now, documented as Option B. $30.36/mo of instances plus a hosted zone — the same money as one t3.medium — and it adds a DNS dependency on the night of a first deployment. The right next step once Tier 1 passes. |
| **t4g.medium (Graviton)** | Rejected for now. 19% cheaper ($0.0336 vs $0.0416/hr) and the engine is pure Go with `CGO_ENABLED=0`, so it cross-compiles. But every image validated in Phases 6–8 is `linux/amd64`, and `pgvector/pgvector:pg16` would need re-validating on arm64 at the same time as everything else is being tried for the first time. Saves $5.84/mo; revisit after a successful run. |
| **A container registry (ECR)** | Rejected. Two images, ~11 MB gzipped, one instance, SSH already available. `docker save` + `scp` costs nothing, publishes nothing and has no registry auth to get wrong. ECR becomes right with more than one instance or a CI deployer; the commands are prepared in the runbook, unrun. |

## Security, and why it is not a convention

The security group opens **port 22 and nothing else**. No rule for 8081–8083
(engine), 3000 (Grafana), 9090 (Prometheus) or 8090 (the simulator). Every
listener inside the instance binds to `127.0.0.1`, and dashboards are reached
over an SSH tunnel.

That is how the requirement *the test-only external-effect simulator must not
become a publicly accessible service* is met. The simulator's `/arm` endpoint
can make a payment processor fail on demand, so it gets **no published port at
all** — not even on loopback — and starts only under a compose profile. It is
unreachable because there is no route to it, not because a configuration file
asks nicely. `scripts/aws/assert_no_public_bind.sh` re-proves this from the
effective `docker compose config` on every validate and every deploy.

Also: IMDSv2 required (an unauthenticated IMDSv1 `GET` would hand the instance
role's credentials to any SSRF), both volumes encrypted, `operator_cidr` has
**no default** and rejects `0.0.0.0/0` so Terraform refuses to plan rather than
open SSH to the internet by omission, and the instance role carries only
`AmazonSSMManagedInstanceCore` — it cannot see the three unrelated projects
sharing this account.

## Decisions recorded

| Decision | Rationale |
|---|---|
| Terraform, local state | No IaC existed; `terraform` was already installed; `plan` is reviewable without touching the account. Remote S3 state would need a bucket and lock table to exist first — billable bootstrap for a disposable cluster. **State holds the generated passwords and is gitignored.** |
| Amazon Linux 2023 from an SSM `latest` parameter | Ships the SSM agent and has `docker` in-repo. The AMI id is recorded in the outputs because `latest` moves. |
| Separate rule resources, not inline `ingress` blocks | Inline blocks are authoritative and silently delete a rule added by hand during an incident. |
| `FAULT_INJECTION` defaults **false** in the cloud compose | `deploy/docker-compose.yml` hard-codes `true` for the webui demo. The acceptance suite arms it for one named scenario and disarms it after. |
| Engine `restart: "no"`, PostgreSQL `restart: unless-stopped` | A node killed to test failover must stay dead, or it reclaims its own shard and hides the transfer. The database is not the thing being killed. |
| No `extsim` in the default bring-up | Phase 8 scenario H was corrupted by an unrelated workflow reaching a globally-armed crash point. Test-only components start only when a scenario asks. |
| `prep_time_ns` set explicitly on every acceptance workflow | `app/delivery` defaults to a **12-minute** timer. That timer is what fired mid-scenario in Phase 8 and killed both nodes. |
