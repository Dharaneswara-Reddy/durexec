# Cost estimate — durexec AWS validation cluster

**Assume no free tier. Budget ceiling $15 for the whole campaign; destroy the
stack the same day you finish with it.**

## The one fact that drives everything

The target account shows **no EC2 free-tier allowance**. `aws freetier
get-free-tier-usage` returned five entries, all of type `Always Free`
(Glue, CloudFront and similar), and **zero of type `12 Months Free`** — no
EC2, no EBS, no public IPv4. AWS's July 2025 change put accounts created
after 15 July 2025 on a credit-based plan and left older ones on the legacy
12-month trials, and this account's 12-month window is evidently over or never
applied.

**Limitation, stated rather than guessed:** `GetFreeTierUsage` reports only the
current month's usage against free-tier limits, so an unused allowance can be
absent simply because nothing used it. The remaining **credit balance cannot be
read** without Billing console access, which these CLI credentials do not have.
So eligibility is *unproven in both directions*. Every number below therefore
assumes **full on-demand price**. If an allowance or credit does exist, the real
bill is lower — never higher.

## Prices, and where they came from

All from the account's own Pricing API (`aws pricing get-products`,
us-east-1), not from a web page:

| Item | Rate | Billed |
|---|---|---|
| t3.medium Linux on-demand | **$0.0416/hr** | while running |
| gp3 storage, 40 GB (20 root + 20 data) | **$0.08/GB-mo** = $3.20/mo | **while the volume exists, running or stopped** |
| Public IPv4 address | **$0.005/hr** | while running (auto-assigned, released on stop) |
| EBS snapshot | **$0.05/GB-mo** | until deleted, for ever |

Two others, for comparison, same source: t3.small $0.0208/hr (2 GiB — too
small, see ARCHITECTURE.md), t4g.medium $0.0336/hr (arm64, 19% cheaper,
images unvalidated).

Public IPv4 at $0.005/hr is the one rate not from the Pricing API — that
product family returned no priced row — and it was confirmed against third-party
reporting of AWS's 1 February 2024 change.

## Scenarios

Split into "while running" ($0.0466/hr = instance + IPv4) and "while the volume
exists" ($0.004384/hr = 40 GB gp3), because **stopping the instance does not stop
the storage charge**.

| Scenario | Running | Volume exists | Cost |
|---|---|---|---|
| **One-day test, then destroyed** | 24 h | 24 h | **$1.22** |
| **Seven days, running continuously, then destroyed** | 168 h | 168 h | **$8.57** |
| **One month, left running** | 730 h | 730 h | **$37.22** |
| Seven calendar days, stopped overnight (8 h/day) | 56 h | 168 h | $3.35 |
| **Forgotten: destroyed instance, volume kept** | 0 h | 730 h | **$3.20/mo for ever** |

The realistic plan — provision in the morning, run Tier 1 and Tier 2, collect
evidence, destroy by evening — is the **first row: about $1.22**.

## What could make these wrong

| Assumption | If it breaks |
|---|---|
| No free tier, no credits | Bill is **lower**. This is the safe direction. |
| us-east-1 | Other regions cost more. ap-south-1 is closer to the operator but dearer. |
| One instance, one AZ | Cross-AZ traffic is billed per GB; this stack has none. |
| Data transfer out is a few MB of evidence | Even at the full $0.09/GB with no free allowance, under $0.01. |
| No load balancer, NAT gateway, RDS, EKS, Secrets Manager | A NAT gateway alone is ~$32/mo — more than the whole cluster. None is used. |
| Snapshots deleted after the Tier 2 test | A 20 GB volume's snapshot bills on used blocks (~0.3 GB ≈ $0.015/mo) **until deleted, for ever**. |
| Instance really is destroyed | A forgotten t3.medium is $30.37/mo. |

**Not included, and not ours:** the account already carries 62 GB of EBS across
three stopped instances (codearena, churn-ml-prod, aiops-demo) and one
associated Elastic IP — roughly **$8.61/mo of pre-existing charges** that
durexec neither creates nor removes. Do not read them as this project's cost,
and do not delete them.

## Cost controls that are actually built in

| Control | Where |
|---|---|
| No Elastic IP, so stopping the instance stops the IPv4 charge | `main.tf`, `associate_public_ip_address` with no `aws_eip` |
| No ECR, no registry charge — images go over `scp` | `scripts/aws/package_images.sh` |
| No Secrets Manager ($0.40/secret-mo); passwords generated into state | `random_password` in `main.tf` |
| Prometheus retention capped at 48 h / 2 GB so the TSDB cannot fill the volume | `docker-compose.cloud.yml` |
| `data_volume_retain_on_destroy` defaults **false**, so teardown really ends charges | `variables.tf` |
| Teardown prints every still-billing tagged resource afterwards | `scripts/aws/teardown.sh` |
| Every resource tagged `Project=durexec` for attribution in a shared account | `provider default_tags` |

**A budget alarm is not created by this Terraform.** AWS Budgets is free for the
first two budgets, but an alarm needs an email address and alarms are
account-wide rather than project-scoped, so it is the account owner's call, not
this stack's. Create one before the first deployment:

```
aws budgets create-budget --account-id <id> --budget \
  '{"BudgetName":"durexec-lab","BudgetLimit":{"Amount":"15","Unit":"USD"},"TimeUnit":"MONTHLY","BudgetType":"COST"}'
```
