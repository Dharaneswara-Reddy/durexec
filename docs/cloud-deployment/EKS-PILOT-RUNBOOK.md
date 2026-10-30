# EKS pilot runbook

One uninterrupted sequence for the first cloud session. Every command below
exists in the repository; none is illustrative.

**This pilot proves the pipeline, not the engine's capacity, and not high
availability.** It runs ONE external PostgreSQL host with no standby, so it
cannot demonstrate database failover, HA, or distributed fault tolerance. Any
throughput number from it is a smoke result (`kind=smoke`), not a measurement.

> **2026-10-30:** before any run, the harness behind this runbook was
> corrected. Smoke and evidence collection could not work against the
> distroless engine image, the deploy check rejected every correct render,
> and the overlay ran 2 engines on 3 nodes. See
> [`../cloud-campaign/03-HARNESS-READINESS.md`](../cloud-campaign/03-HARNESS-READINESS.md).
> The commands below still work. `eks_loadgen.sh` now writes
> `docs/results/cloud-campaign/results.csv` (99 columns) rather than
> `docs/results/eks-pilot/`. The full campaign beyond this pilot is in
> [`../cloud-campaign/02-EXPERIMENT-MATRIX.md`](../cloud-campaign/02-EXPERIMENT-MATRIX.md).

---

## Stop conditions — abandon the session if any occurs

| # | Condition |
|---|---|
| 1 | `terraform plan` shows anything other than **28 add, 0 change, 0 destroy** |
| 2 | Spend reaches **$5** |
| 3 | The credit balance or its expiry cannot be established in the console |
| 4 | `eks_smoke.sh` fails any check |
| 5 | The pipeline-validation run returns a verdict beginning `INVALID` |
| 6 | `kubectl get nodes` shows a node you did not create |
| 7 | You will be away from the terminal for more than an hour |

**If 4 or 5 occurs, do NOT continue to the scaling sweep.** Collect evidence,
then tear down.

---

## 0. Preflight — free, nothing billable

```sh
cd engine
aws sts get-caller-identity --profile aiops-deploy     # expect user/cli-deployer
aws configure get region --profile aiops-deploy        # expect us-east-1
curl -s https://checkip.amazonaws.com                  # refresh operator_cidr; mobile IPs rotate
```

Edit `deploy/aws/eks/terraform.tfvars` so `operator_cidr` is that address with
`/32`. The file is gitignored.

**Then open the console and read the credit balance and expiry:**
`https://us-east-1.console.aws.amazon.com/billing/home#/credits`
No AWS API exposes it. **If you cannot establish it, stop (condition 3).**

```sh
terraform -chdir=deploy/aws/eks init
terraform -chdir=deploy/aws/eks plan        # MUST read 28 add, 0 change, 0 destroy
```

---

## 1. Publish images — FIRST BILLABLE COMMAND

> **Everything above this line is free. This line starts spending.**
> It creates three ECR repositories and transfers roughly 43 MB.

```sh
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/eks_publish.sh
```

Builds and pushes `durexec/{engine,extsimd,loadgen}:<git-sha>`, each verified
`linux/amd64` before push. It only ever writes under the `durexec/` prefix;
the account's `churn-ml-*` and `aiops-demo` repositories are never touched.

## 2. Create infrastructure

```sh
terraform -chdir=deploy/aws/eks apply       # ~12 min; cluster + nodegroups + DB host
```

## 3. Kubernetes context

```sh
aws eks update-kubeconfig --region us-east-1 --name durexec-bench
kubectl get nodes -L durexec.io/role        # expect 3 engine + 1 harness
```

## 4. Deploy

```sh
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/eks_deploy.sh
```

Reads the database private IP and password from `terraform output`, builds the
`durexec-db` Secret in memory, substitutes the host and image into the
rendered overlay, and **refuses to apply if any placeholder survives** or if
the init gate, image tag or Postgres removal is not as intended.

## 5. Smoke — go/no-go

```sh
./scripts/aws/eks_smoke.sh
```

Seven checks, including **pods on distinct nodes proven from `spec.nodeName`**,
no in-cluster Postgres, and a workflow reaching COMPLETED.
**Any failure: go to step 8.**

## 6. Pipeline validation — one short run

```sh
KIND=smoke CONC=64 DUR=20s \
  DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/eks_loadgen.sh
```

Expect `verdict=SMOKE_ok`. **Any `INVALID*` verdict: go to step 8.**

## 7. Bounded scaling campaign — only if 5 and 6 passed

```sh
for c in 64 128 256; do
  KIND=perf CONC=$c DUR=25s \
    DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/eks_loadgen.sh
done
```

Each point writes a row before the next starts, so an interruption leaves
usable evidence. Rows carry `gen_cpu_cores`, `gen_role`, `eng_cpu_pct` and a
verdict; anything `GENERATOR_SATURATED`, `INCONCLUSIVE` or `INVALID` is
**excluded from any capacity claim**.

## 8. Evidence — ALWAYS, before teardown

```sh
./scripts/aws/collect_eks_evidence.sh eks-pilot
```

Exits nonzero if a required artefact is missing. Phase 7 and Phase 8 each
destroyed their own evidence by restarting first; this runs first.

## 9. Teardown

```sh
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/eks_teardown.sh
```

Deletes Kubernetes objects and PVCs first (so no EBS volume is orphaned), then
`terraform destroy`, then a **read-only** sweep of instances, volumes,
untagged available volumes, snapshots, load balancers and `durexec/` ECR
repositories. It never deletes by name pattern — the account is shared.

## 10. Reconcile

```sh
aws ce get-cost-and-usage --time-period Start=<today>,End=<tomorrow> \
  --granularity DAILY --metrics UnblendedCost \
  --filter '{"Tags":{"Key":"Project","Values":["durexec"]}}'   # $0.01 per call
ls docs/results/cloud/                                          # evidence bundle
```

---

## Cost

| Component | $/hr |
|---|---|
| EKS control plane (v1.37, standard support) | 0.1000 |
| 3 × m7i.large engine | 0.3024 |
| 1 × c7i.large harness | 0.0892 |
| 1 × m7i.large database | 0.1008 |
| 110 GB gp3 | 0.0121 |
| 5 × public IPv4 | 0.0250 |
| **Total** | **$0.6295** |

**3-hour pilot ≈ $1.89; $2.65 with 40 % contingency. Ceiling $5.**

These are estimates from the Pricing API, **not a guarantee**. Not included:
ECR storage (~$0.01/month, **persists after teardown**), data transfer out,
and any overrun.

**Do not assume credits cover this.** The balance is unverified.

## Persistent after teardown

| Resource | Action |
|---|---|
| ECR images (~43 MB) | Deliberate, so a re-run need not rebuild. `aws ecr delete-repository --repository-name durexec/engine --force` to stop it |
| EBS snapshots | Terraform creates none. If you take one, it bills **for ever** until deleted |
| Orphaned PVC volumes | The teardown sweep lists untagged available volumes; delete by ID if any appear |
