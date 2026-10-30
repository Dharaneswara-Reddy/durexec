# AWS cloud experimentation campaign

**Status, 2026-10-30: PREPARED, NOT RUN.** No AWS resource has been
created, no cloud measurement exists, and nothing in this directory is a cloud
result. The session that prepared it held placeholder AWS credentials (every
call returned `InvalidClientTokenId`), and its network policy blocked the
Terraform provider registry.

| File | Contents |
|---|---|
| [`01-EVIDENCE-INVENTORY.md`](01-EVIDENCE-INVENTORY.md) | Every metric collected so far: what, how, where, result, conclusive or not, and whether the cloud collects it |
| [`02-EXPERIMENT-MATRIX.md`](02-EXPERIMENT-MATRIX.md) | The cloud matrix derived from that evidence: hypotheses, stages, gates, durations, repeats, exclusions, budget |
| [`03-HARNESS-READINESS.md`](03-HARNESS-READINESS.md) | 16 harness defects found and fixed before spending anything, how each fix was verified, and what remains unproven |
| `results.csv`, `faults.csv`, `raw/`, `images.txt` | Created by the first cloud run. **Absent means NOT RUN**, never "passed" |

## The ten deliverables

| # | Deliverable | Status |
|---|---|---|
| 1 | Cloud experimental report | NOT RUN — method and matrix ready (`02`) |
| 2 | Local versus cloud comparison | NOT RUN — local side inventoried (`01`), normalization rules in `02` §7 |
| 3 | Benchmark dataset | NOT RUN — schema fixed: 97 columns per load point, 33 per fault trial |
| 4 | Infrastructure report | NOT RUN — will be read from the evidence bundle (nodes, instance types, zones, settings), not from Terraform intent |
| 5 | Bottleneck analysis | NOT RUN — hypotheses H1–H8 and the rule that decides each are in `02` |
| 6 | Reliability report | NOT RUN — F1–F11 designed; oracle and invariants ready |
| 7 | Validity report | NOT RUN — verdict vocabulary is in the runners; every row carries one |
| 8 | Recommendations | Pre-campaign only, below |
| 9 | Cost report | NOT RUN — $0 spent. Estimate in `02` §5 |
| 10 | Cleanup report | Nothing to clean up: nothing was created |

## What the owner must provide before G0

1. **Valid AWS credentials** in the environment's secret settings
   (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN` if
   temporary), region `us-east-1`. With env credentials set, `_common.sh` no
   longer forces the `aiops-deploy` profile.
2. **Network access to `registry.terraform.io`**, for the AWS and random
   providers.
3. **`operator_cidr`** for `terraform.tfvars`. This session's egress
   address is a shared proxy. The owner decides whether the EKS API may be
   reachable from it.
4. **Spending cap, credit balance and expiry**, read from the Billing →
   Credits page (no API exposes it). The runbook's stop condition 3 requires it.
5. **Which plan**: Core, Full, or Two sessions (`02` §5).

## Run order (each step gated on the one before)

All commands run from `engine/` and need `DUREXEC_CONFIRM=i-understand-this-costs-money`.

```sh
terraform -chdir=deploy/aws/eks init && terraform -chdir=deploy/aws/eks plan   # G0: 28/0/0
./scripts/aws/eks_publish.sh                                                    # G1 (clean tree)
terraform -chdir=deploy/aws/eks apply                                           # G2
aws eks update-kubeconfig --region us-east-1 --name durexec-bench
./scripts/aws/eks_deploy.sh && ./scripts/aws/eks_smoke.sh                       # G3: S1-S9
./scripts/aws/eks_fault.sh verify-policy                                        # G4
KIND=smoke CONC=32 DUR=30s ./scripts/aws/eks_loadgen.sh                         # G5
./scripts/aws/collect_eks_evidence.sh g-validation                              # G6
EXP=C0 KIND=perf MODE=arrival RATE=1 FLOOR=10s DUR=120s TRIAL=1 ./scripts/aws/eks_loadgen.sh   # C0 (x3)
# B, E1-E5: eks_loadgen.sh with EXP/MODE/RATE/CONC/TRIAL; reconfigure with
#   SHARD_COUNT=.. WORKFLOW_WORKERS=.. ACTIVITY_WORKERS=.. [RESET_DB=i-understand-this-deletes-pilot-data] ./scripts/aws/eks_deploy.sh
# F: SCENARIO=<name> TRIAL=<n> FLOOR=<C0 floor> LOAD_RATE=<0.6 x knee> ./scripts/aws/eks_fault_trial.sh
./scripts/aws/eks_teardown.sh                                                   # after the final evidence bundle
```

C0 declares a deliberately high `FLOOR` (10 s) because its only purpose is
to *measure* the floor; its loadgen verdict is ignored.

## Cost reporting

EKS-launched node instances do not carry `Project=durexec` (no launch
template). A Cost Explorer filter on that tag alone therefore omits the node
hours. Report cost from **both** `Project=durexec` and
`eks:cluster-name=durexec-bench`. Cost-allocation tags must be activated in
Billing before they appear, and activation is not retroactive. Where neither
is active, report the account's daily `EC2`, `EKS`, `EBS` and `VPC` (public
IPv4) lines for the campaign days, and say that the attribution is by date,
not by tag.

## Pre-campaign recommendations (from code and existing evidence only)

These are engineering observations, not measured cloud findings.

- **R1. Separate shard count from worker concurrency in any scaling claim.**
  Workers are per shard, so the published shard-scaling ratios also scale
  workers. E1 settles it.
- **R2. Treat the 1 s timer scan and 1 s idle back-off as a latency
  property.** They likely dominate sub-knee latency (H3), which caps
  closed-loop throughput at a fixed concurrency. They don't cap open-loop
  capacity. If C0 confirms it, an event-driven wake-up on commit (or a
  shorter scan when timers are due) is the cheapest latency improvement
  available.
- **R3. Expect the per-node 20-connection pool to be the failure mechanism
  under overload.** If E4 confirms Phase 6's collapse, admission control (shed
  at the front door when pool wait rises) would turn collapse into a plateau.
- **R4. Export lease-renewal latency and per-commit latency.** Neither exists
  today, so lease health under DB latency (F5) can only be seen as
  transitions after the fact.
- **R5. Fix `docs/results/local-shard-sweep/results.csv`'s latency columns and
  commit the raw loadgen output behind FINDINGS.md** if it is still on the
  laptop. Without it, those latency figures can't be checked.
