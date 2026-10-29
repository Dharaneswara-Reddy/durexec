# EC2 instance comparison, us-east-1

Specifications from `aws ec2 describe-instance-types`; prices from the AWS
Pricing API (Linux, shared tenancy, on-demand). Both queried 2026-10-28.
Nothing here is copied from a web page.

---

## The finding that matters most

**"2 vCPU" means one physical core.** AWS counts a hyperthread as a vCPU, so
`t3.medium`, `t3.large`, `m7i.large` and `c7i.large` each report
**`DefaultCores = 1`**. The laptop has **10 physical cores**.

| | Physical cores | Threads | RAM |
|---|---|---|---|
| **This laptop** (i5-1335U) | **10** | 12 | **15 GiB** |
| t3.medium (the current plan) | **1** | 2 | 4 GiB |
| m7i.2xlarge (8× the price) | 4 | 8 | 32 GiB |

**The laptop out-cores every instance affordable at this budget.** Going to
the cloud is not an upgrade in compute. It is an upgrade in *failure
isolation*.

---

## Verified specifications and prices

| Instance | vCPU | **Cores** | RAM | Burstable | Network | EBS Mbps | $/hr |
|---|---|---|---|---|---|---|---|
| **t3.medium** | 2 | **1** | 4 GiB | **Yes** | Up to 5 Gb | 2 085 | **$0.0416** |
| t3.large | 2 | **1** | 8 GiB | **Yes** | Up to 5 Gb | 2 780 | $0.0832 |
| t3.xlarge | 4 | 2 | 16 GiB | **Yes** | Up to 5 Gb | 2 780 | $0.1664 |
| c7i.large | 2 | **1** | 4 GiB | No | Up to 12.5 Gb | 10 000 | $0.0892 |
| m7i.large | 2 | **1** | 8 GiB | No | Up to 12.5 Gb | 10 000 | $0.1008 |
| c7i.xlarge | 4 | 2 | 8 GiB | No | Up to 12.5 Gb | 10 000 | $0.1785 |
| m7i.xlarge | 4 | 2 | 16 GiB | No | Up to 12.5 Gb | 10 000 | $0.2016 |
| m7i.2xlarge | 8 | 4 | 32 GiB | No | Up to 12.5 Gb | 10 000 | $0.4032 |

Supporting costs, same source: **gp3 storage $0.08/GB-month**, **public IPv4
$0.005/hour while running**, EBS snapshot $0.05/GB-month until deleted.

---

## Session costs, including storage and IPv4

One instance with 40 GB of gp3 and one public IPv4:

| Instance | $/hr all-in | 6 h | 24 h | 7 days | 30 days |
|---|---|---|---|---|---|
| **t3.medium** | **$0.0510** | **$0.31** | **$1.22** | $8.57 | $37.22 |
| t3.large | $0.0926 | $0.56 | $2.22 | $15.56 | $67.60 |
| t3.xlarge | $0.1758 | $1.05 | $4.22 | $29.53 | $128.33 |
| c7i.large | $0.0986 | $0.59 | $2.37 | $16.56 | $71.98 |
| m7i.large | $0.1102 | $0.66 | $2.64 | $18.51 | $80.45 |
| m7i.xlarge | $0.2110 | $1.27 | $5.06 | $35.44 | $154.03 |

*Storage is $0.0044/hr for 40 GB and keeps billing while an instance is
stopped. IPv4 is $0.005/hr and stops when the instance stops.*

Against a **$31 ceiling**, every six-hour session above is affordable. Only
the 7-day and 30-day columns threaten the budget. **Duration, not instance
size, is what spends the money.**

---

## Why a bigger instance may not be faster

Four reasons, each specific to this workload.

**1. The engine is database-bound, not CPU-bound.** Every durexec measurement
so far saturated on PostgreSQL — commit latency, WAL flush, connection
pressure — not on available cores. The Phase 6 sweep reached 25.78 wf/s on two
nodes of a 2-vCPU Kubernetes node. Doubling vCPU does not double commits per
second when the bottleneck is `fsync`.

**2. Burstable instances distort sustained benchmarks.** `t3` instances earn
CPU credits while idle and spend them under load. A long run can exhaust its
credits and silently drop to baseline — **t3.medium's baseline is 20 % of 2
vCPU**, roughly 0.4 of a core. A throughput curve that bends downward at
minute 30 may be measuring credit exhaustion rather than the engine. **`m7i`
and `c7i` are not burstable and have no such cliff.**

**3. More RAM only helps if memory was the limit.** The cloud stack declares
about 2.5 GB of container limits. On a 4 GiB instance that leaves little
slack; on 8 GiB it leaves plenty. Beyond 8 GiB, extra memory buys page cache
for PostgreSQL and nothing else.

**4. Architecture differs.** The laptop is Raptor Lake hybrid at 15 W; `m7i`
and `c7i` are Sapphire Rapids server parts at a sustained clock, with AVX-512
that the laptop lacks. Single-thread performance is in the same region;
sustained all-core performance favours the server part. **Neither claim can be
quantified without measuring.**

> **No speedup figure appears anywhere in this document**, because none has
> been measured. Any number would be invented.

---

## The short benchmark that would settle it

Run once, after deployment is authorised, on both the laptop and the instance:

```sh
# 1. Replay cost: CPU and memory only, no database
go test ./internal/bench -run TestNFR6ReplayBudget -v
go test ./internal/bench -bench BenchmarkReplayLongHistories -benchtime 200x

# 2. Database commit latency: the suspected real bottleneck
pgbench -i -s 10 durexec && pgbench -c 8 -j 4 -T 60 durexec

# 3. End to end: the number that actually matters
make loadgen SHARDS=4 CONCURRENCY=128 DURATION=20s
```

Local p99 for 200-event replay is **5.65 ms** (`docs/RESULTS_SUMMARY.md` §3).
That one figure makes the comparison falsifiable: if the instance is slower on
test 1 but faster on test 3, the bottleneck is the database, exactly as
predicted.

---

## Recommendation for this project

| Purpose | Instance | Why |
|---|---|---|
| **Tier 1 correctness** | **t3.medium** | Correctness does not need speed. The cheapest thing that runs the stack |
| **Multi-host fault testing** | **t3.medium × 2–3** | Three small machines beat one large one when the point is killing a machine |
| **Sustained throughput run** | **m7i.large** | Only if a burst-credit cliff is actually observed; otherwise unnecessary |
| Anything larger | — | **Not justified.** The laptop already has more cores and more RAM |

Full architecture costings are in `TEST-ARCHITECTURE-OPTIONS.md`.
