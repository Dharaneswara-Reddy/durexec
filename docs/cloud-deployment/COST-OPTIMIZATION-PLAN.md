# Cost optimisation plan

Treats **$31 as a hard ceiling**, not a target. No hypothetical promotional
credit is assumed anywhere — `CREDIT-OPPORTUNITIES.md` establishes that none
is available to this account.

---

## The budget is not the constraint

| Scenario | Cost | % of $31 |
|---|---|---|
| Conservative | **$0.41** | 1.3 % |
| Balanced | **$1.37** | 4.4 % |
| Maximum testing value | **$5.60** | 18.1 % |

Even the largest plan leaves **$25 untouched**. The real limits are operator
time, the credit expiry date you still need to read from the console, and the
**$0.29/day the other three projects already burn** — about $8.70/month,
which would exhaust $31 in roughly 3.5 months with no durexec usage at all.

---

## Scenario 1 — Conservative

*Prove the stack runs in the cloud, and nothing more.*

| | |
|---|---|
| Shape | 1 × t3.medium, 40 GB gp3, 1 public IPv4 |
| Duration | 8 hours (2 h setup, 4 h testing, 2 h evidence and teardown) |
| Rate | $0.0510/hr |
| **Total** | **$0.41** |
| **Remaining** | **$30.59** |
| Covers | T1.1 – T1.8 in full |
| Does not cover | Host loss, network partition, database-host failure |
| Terraform change needed | **None.** The existing plan is exactly this |

---

## Scenario 2 — Balanced  (recommended)

*The smallest spend that makes the distributed claim defensible.*

| Stage | Shape | Hours | Cost |
|---|---|---|---|
| A | 1 × t3.medium — validate the stack | 2 | $0.10 |
| C | 3 × t3.medium — multi-host campaign | 6 | $0.88 |
| D | 4 × t3.medium — one failover drill | 2 | $0.39 |
| | | | **$1.37** |

| | |
|---|---|
| **Remaining** | **$29.63** |
| Covers | Everything in Scenario 1, **plus** host loss with survivors, a real network partition, engine-host loss independent of the database, and genuine standby promotion after leader-host loss |
| Does not cover | Automatic failover (the engine has none), cross-region, sustained multi-hour load |
| Terraform change needed | **Yes — multi-instance support is not yet written.** About half a day, validated locally first |

---

## Scenario 3 — Maximum testing value

*A short, deliberately heavier campaign with a hard stop.*

| Stage | Shape | Hours | Cost |
|---|---|---|---|
| A | 1 × t3.medium validation | 2 | $0.10 |
| C | 3 × t3.medium fault campaign | 10 | $1.46 |
| D | 4 × t3.medium failover drill | 3 | $0.58 |
| E | 1 × m7i.large sustained load, no burst cliff | 8 | $0.88 |
| F | 3 × t3.medium repeat after bug fixes | 6 | $0.88 |
| Snapshots | 3 × ~0.3 GB retained one week | — | $0.01 |
| Data transfer out | evidence bundles, well under 1 GB | — | $0.09 |
| **Subtotal** | | | **$4.00** |
| Contingency at 40 % | | | $1.60 |
| **Budgeted total** | | | **$5.60** |

| | |
|---|---|
| **Remaining** | **$25.40** |
| Adds | A non-burstable sustained-throughput curve that CPU-credit exhaustion cannot distort, and one full repeat run after fixing whatever the first campaign finds |

---

## Recommended ceiling

**Authorise $8.** That is 26 % of the balance, nearly six times the Balanced
plan, and still leaves $23 for the other projects and for mistakes.

**Stop conditions — tear down immediately if any one occurs:**

1. Accumulated durexec spend passes **$8**.
2. A session runs more than **2 hours** past its plan.
3. `describe-instances` shows an instance you did not plan.
4. A test fails in a way that invalidates the rest of the session — fix it
   locally and redeploy rather than leaving instances idling.
5. You will be away from the terminal for more than an hour.

---

## What keeps charging after you stop an instance

| Resource | Stops when the instance stops? | Stops on teardown? |
|---|---|---|
| EC2 compute | **Yes** | Yes |
| Public IPv4 (auto-assigned) | **Yes** | Yes |
| **EBS volumes** | **No — $0.08/GB-month regardless** | Yes, because `data_volume_retain_on_destroy = false` in the current tfvars |
| **EBS snapshots** | **No** | **No — they survive `terraform destroy` and bill for ever until deleted by hand** |
| VPC, subnet, IGW, security group, IAM role | Free | Yes |
| EIP, NAT gateway, load balancer, EKS | None is created | — |

**Snapshots are the one real trap.** Terraform never creates them, so they can
appear only if you take one during a Tier 2 restore test. Delete them
yourself afterwards.

---

## Verifying nothing is left

Run after every teardown. All read-only:

```sh
export AWS_PROFILE=aiops-deploy
aws ec2 describe-instances --region us-east-1 \
  --filters Name=tag:Project,Values=durexec \
  --query 'Reservations[].Instances[].{Id:InstanceId,State:State.Name}' --output table

aws ec2 describe-volumes --region us-east-1 \
  --filters Name=tag:Project,Values=durexec \
  --query 'Volumes[].{Id:VolumeId,GB:Size,State:State}' --output table

aws ec2 describe-snapshots --owner-ids self --region us-east-1 \
  --query 'Snapshots[].{Id:SnapshotId,GB:VolumeSize,Started:StartTime}' --output table

aws ec2 describe-addresses --region us-east-1 \
  --query 'Addresses[].{IP:PublicIp,Assoc:AssociationId}' --output table
```

The first three should come back empty. **The fourth will not** — another
project owns an Elastic IP in this account. That one is not yours; leave it.

`scripts/aws/teardown.sh` already performs this sweep read-only, and
deliberately never deletes by pattern, because the account is shared.

---

## Budget alerts

A **zero-spend budget is free** — AWS provides two cost budgets at no charge.
It is the single best protection against a forgotten instance.

**I did not create one**, both because this audit forbids creating resources
and because `budgets describe-budgets` returned **AccessDenied** for these
credentials. Create it yourself:

**Billing → Budgets → Create budget → Cost budget → Monthly → $8 → alert at
50 %, 80 % and 100 %**, with your email address.

If the console also denies it, the account root user must do it.

---

## Cost assumptions, stated

1. Prices are us-east-1 on-demand Linux from the Pricing API, 2026-10-28.
2. Tax is **$0.00** — all three observed months show a zero tax line.
3. Data transfer out is treated as under 1 GB per campaign; evidence bundles
   are logs and JSON.
4. CloudWatch is not used; Prometheus and Grafana run in-cluster.
5. gp3 bills the 40 GB provisioned size, not used blocks.
6. Compute bills per second with a 60-second minimum.
7. **Credits are assumed to cover all of it**, as they have for three
   consecutive months — but every figure is stated in real dollars so the plan
   survives the credits running out.
