# Recommendation

Audit of 2026-10-28. Read-only throughout: **nothing was deployed, created,
modified, redeemed or enrolled in.**

---

## The ten questions, answered plainly

### 1. Can I realistically get another $20 in AWS credits?

**No.** The offer is real — AWS gives $100 at sign-up plus $20 for each of
five onboarding activities — but the Free Tier FAQ states it is "available
only to new AWS customers", that existing accounts are **ineligible**, and
that an existing account **cannot be converted**. This account carries three
student projects and three months of billed usage.

The only unverified possibility is **AWS Student Rewards** at the Builder
Center (reportedly $10 at 7 badges, $20 more at 14). Worth ten minutes of
your own checking. AWS Educate gives **labs and training only, never account
credits**.

### 2. What is the maximum verified credit budget?

**Unverifiable by API — AWS exposes no credit-balance endpoint.** What I
*can* prove from Cost Explorer: credits covered **100 % of usage for three
consecutive months** — $42.94, $77.84 and $8.05 — with **$0.00 out of
pocket**, across EC2, EBS, VPC, data transfer, ECR, RDS and S3.

Read the balance yourself at **Billing → Credits**
(`console.aws.amazon.com/billing/home#/credits`) and note the **expiry date**.
Plan against your $31 figure until then.

### 3. How does the proposed EC2 hardware compare with my computer?

**Your laptop is bigger.** This is the finding that reframes everything.

| | Physical cores | Threads | RAM |
|---|---|---|---|
| **Laptop** (i5-1335U) | **10** | 12 | **15 GiB** |
| t3.medium | **1** | 2 | 4 GiB |

AWS counts a hyperthread as a vCPU, so "2 vCPU" is **one physical core**
(`DefaultCores = 1`, verified with `describe-instance-types`).

**So do not go to the cloud for speed.** Go for **failure-domain
separation** — the ability to destroy one machine and watch the others
survive, which one laptop cannot provide at any specification.

### 4. Is the current t3.medium configuration sufficient?

**Yes for Tier 1; no for the distributed claim.** It runs all eight T1 cases
in full. But all eight containers — three engine nodes, leader, standby,
Prometheus, Grafana — sit on **one instance, which is one failure domain**.
Stopping it stops everything, so nothing survives to observe the failure.

### 5. One, two, or three EC2 instances?

**Three** for the campaign. Two is the minimum that buys anything new; three
separates the database cleanly from the engine hosts. Four, briefly, for the
database failover drill.

### 6. What is the best test architecture for my budget?

| Stage | Shape | Hours | Cost |
|---|---|---|---|
| A | 1 × t3.medium, validate the stack | 2 | $0.10 |
| C | 3 × t3.medium, fault campaign | 6 | $0.88 |
| D | 4 × t3.medium, one failover drill | 2 | $0.39 |
| | | | **$1.37** |

**4.4 % of $31.** The budget is genuinely not the constraint; operator time
and credit expiry are.

**Not Kubernetes.** EKS costs $0.10/hr for the control plane alone —
$73/month, more than twice the budget — and it would *reschedule killed pods
automatically*, hiding the very failure being measured.

### 7. What tests genuinely validate distributed execution?

Three that are **impossible today at any instance size**:

| Test | Why it needs separate machines |
|---|---|
| **Host loss with survivors** | Stop one VM; the others must keep completing work and absorb its shards |
| **Network partition** | A security-group rule between hosts; a shared Docker bridge cannot be partitioned |
| **Database-host loss ≠ process loss** | Only meaningful once the database has a host of its own |

And one honest correction: **T1.6 does not test fencing.** Traced to the
source, it kills a node and asserts that tokens advance and never regress.
Nothing stale attempts a write, so the *rejection* path is unexercised — the
same limitation Phase 8 scenario I already records.

### 8. How much should the campaign cost?

**Authorise $8** — about six times the plan, 26 % of the balance. Expected
spend **$1.37**. Hard stop if accumulated durexec spend reaches $8.

### 9. What will remain untested?

| Gap | Why |
|---|---|
| **Automatic** database failover | The engine has none; promotion is manual by design |
| Fenced-writer rejection | Needs the stale-worker thaw harness from Phase 8 |
| Cross-region and cross-AZ latency | A single AZ keeps cost and complexity down |
| Byzantine faults | Out of scope |
| Zero data loss | Async replication loses 1–2 events. **Will not be claimed** |
| Sustained multi-hour load | Only if a `t3` burst cliff is actually observed |

### 10. What should I authorize next?

**Three things, in order.**

**(a) Two console checks — five minutes, free, do these first.**
Read **Billing → Credits** for the real balance and expiry date. Create an
**$8 monthly budget alert** at Billing → Budgets (free; `budgets:DescribeBudgets`
is denied to these credentials, so it must be done in the console).

**(b) Authorise Campaign A — $0.10, no code changes.**
The existing plan already builds exactly this:

```sh
cd engine
make aws-validate && make aws-plan        # re-run; your IP may have changed
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/provision.sh
./scripts/aws/wait_ready.sh
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/deploy_stack.sh
./scripts/aws/smoke.sh
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/acceptance_tier1.sh
./scripts/aws/collect_evidence.sh tier1-baseline    # BEFORE any disarm
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/teardown.sh
```

**(c) Authorise the multi-VM Terraform work — half a day, local only.**
`main.tf` builds **one** instance. Campaigns B and C need a counted instance
set plus private DNS so node IDs resolve across hosts. **That code does not
exist yet.** It is local work, validated with `make aws-validate` and
`make aws-plan`, with no apply until you say so.

---

## Reading order

| Document | Answers |
|---|---|
| `CREDIT-OPPORTUNITIES.md` | Every credit programme, verified or dismissed |
| `LOCAL-HARDWARE-BASELINE.md` | What your machine actually is |
| `INSTANCE-COMPARISON.md` | Verified specs and prices; why bigger may not be faster |
| `TEST-ARCHITECTURE-OPTIONS.md` | One VM versus two, three, four; why not EKS |
| `COST-OPTIMIZATION-PLAN.md` | Three budgets, stop conditions, what keeps billing |
| `CLOUD-TEST-CAMPAIGN.md` | The tests, and what each may and may not claim |

---

## The one-paragraph version

Your laptop has ten physical cores and 15 GiB; a `t3.medium` has one core and
4 GiB, so **the cloud buys isolation, not speed**. The $20 credit offer is
real but closed to existing accounts, and it does not matter: credits have
covered 100 % of this account's usage for three months, and the entire
recommended campaign costs **$1.37**. Run the existing single-instance plan
first for $0.10 to prove the stack works, then spend half a day adding
multi-instance support to Terraform, then run a three-VM fault campaign that
can finally test **host loss, network partition and database-host failure** —
the three things one machine can never test. Authorise an **$8 ceiling**, set
a free budget alert, and read your real credit balance in the console before
starting.

---

## Status

- **No AWS resources were created or modified.**
- **No `terraform apply` or `destroy` was run** — plan only, which is
  read-only.
- **No credits were redeemed and no programme was enrolled in.**
- **No paid service was activated.**
- Repository: HEAD unchanged, stash intact, nothing staged, committed or
  pushed. The only writes were these seven documents plus pointer lines.
- Cost incurred by this audit: **three Cost Explorer calls at $0.01 each.**
- Unresolved: the **actual credit balance and its expiry date**, which no AWS
  API exposes.
