# Deploying durexec to AWS — start here

**Status: READY FOR AUTHORIZED CLOUD DEPLOYMENT. Nothing has been deployed.**

One `t3.medium` in a VPC of its own, running the existing Compose stack:
three engine nodes, a PostgreSQL leader, a streaming standby, Prometheus,
Grafana. About **$1.22** for a one-day validation.

## What to do next, in order

```bash
cd engine

# 1. One-time: a key and your own IP address.
ssh-keygen -t ed25519 -f ~/.ssh/durexec-aws -C durexec-aws
cp deploy/aws/terraform/terraform.tfvars.example deploy/aws/terraform/terraform.tfvars
curl -s https://checkip.amazonaws.com
$EDITOR deploy/aws/terraform/terraform.tfvars      # operator_cidr = "<that>/32"

# 2. Free, local, reaches no AWS write API. Re-run any time.
make aws-validate && make aws-build && make aws-package

# 3. Read the plan. Expect 18 resources and ONE ingress rule (tcp/22, your /32).
make aws-plan

# 4. FIRST BILLABLE STEP.
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/provision.sh
./scripts/aws/wait_ready.sh

# 5. Deploy and check.
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/deploy_stack.sh
./scripts/aws/smoke.sh

# 6. Acceptance, then evidence — in this order.
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/acceptance_tier1.sh
./scripts/aws/collect_evidence.sh tier1-baseline

# 7. STOP THE CHARGES. Same day.
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/teardown.sh
```

## The four things most worth knowing

1. **No free tier was found in this account.** Five `Always Free` entries, zero
   `12 Months Free`, no EC2 or EBS allowance. Every price assumes full
   on-demand. Credit balance is unreadable with these credentials — stated, not
   guessed.
2. **The account is shared** with codearena, churn-ml-prod and aiops-demo, and
   already carries ~$8.61/mo of EBS and an Elastic IP that are not ours. The
   stack uses its own VPC and the teardown never deletes by pattern.
3. **No public HTTP port exists.** Only tcp/22, only from your `/32`. The
   test-only simulator publishes no port at all and starts only under a
   compose profile — it is unreachable because there is no route, not because a
   config file asks nicely.
4. **Collect evidence before disarming or restarting anything.** Phase 7 and
   Phase 8 each destroyed their own evidence by doing it the other way round.

## The documents

| File | What is in it |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | Why one instance (the engine resolves peers by node id), what this cannot validate, why not EKS/ECS/RDS/ECR |
| [COST-ESTIMATE.md](COST-ESTIMATE.md) | Prices from the account's own Pricing API; 1-day, 7-day and 1-month scenarios; what could make them wrong |
| [DEPLOYMENT-RUNBOOK.md](DEPLOYMENT-RUNBOOK.md) | Every command, Tier 2 durability steps, troubleshooting, teardown, unrun ECR commands |
| [ACCEPTANCE-MATRIX.md](ACCEPTANCE-MATRIX.md) | 9 local checks that PASSED, 10 Tier 1 and 7 Tier 2 cases NOT RUN, and what stays deliberately blocked |
| [../cloud-testing/10-PHASE9-AWS-PREP.md](../cloud-testing/10-PHASE9-AWS-PREP.md) | The phase report: files changed, tests run, defects found, blockers |

## What is not ready, and will not be made ready by deploying

The external-effect contract is **at-least-once invocation, with
effectively-once effects only when the recipient honours the idempotency key**.
Cross-run idempotency is designed but not built. Compensation is blocked on
ambiguous outcomes. There is no PITR, no `data_checksums`, no off-node backup,
no SDK and no workflow-versioning story. Database failover is 9–15 s and
manual, which is worse than published work. None of that changed in this phase,
and none of it should be presented as if it had.

## Cloud testing audit (2026-10-28, read-only — nothing deployed)

Start with **[RECOMMENDATION.md](RECOMMENDATION.md)**; it answers the ten
questions and links the rest.

| Document | Answers |
|---|---|
| [RECOMMENDATION.md](RECOMMENDATION.md) | What to authorise next, and why |
| [CREDIT-OPPORTUNITIES.md](CREDIT-OPPORTUNITIES.md) | Every AWS credit programme, verified or dismissed |
| [LOCAL-HARDWARE-BASELINE.md](LOCAL-HARDWARE-BASELINE.md) | What this laptop actually is |
| [INSTANCE-COMPARISON.md](INSTANCE-COMPARISON.md) | Verified EC2 specs and prices; vCPU vs physical cores |
| [TEST-ARCHITECTURE-OPTIONS.md](TEST-ARCHITECTURE-OPTIONS.md) | One VM vs two, three, four; why not EKS |
| [COST-OPTIMIZATION-PLAN.md](COST-OPTIMIZATION-PLAN.md) | Three budgets, stop conditions, what keeps billing |
| **[KUBERNETES-TEST-PLAN.md](KUBERNETES-TEST-PLAN.md)** | **The EKS scaling and fault campaign — read this for the cloud testing plan** |
| [CLOUD-TEST-CAMPAIGN.md](CLOUD-TEST-CAMPAIGN.md) | The tests, and what each may and may not claim |
