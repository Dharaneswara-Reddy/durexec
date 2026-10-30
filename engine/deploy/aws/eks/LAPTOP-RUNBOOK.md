# EKS pilot: what must run from the operator laptop

The Kubernetes API endpoint accepts public traffic only from `operator_cidr`
(`public_access_cidrs` in `main.tf`). Nothing else is allowed, and the shared
Claude proxy address is deliberately NOT added. Any command that talks to the
Kubernetes API therefore has to run from the laptop whose address is
`operator_cidr`. A cloud session can plan and review, but it cannot reach the
cluster.

## Which commands need the Kubernetes API

| Step | Command | Kubernetes API | AWS API | Notes |
|---|---|---|---|---|
| publish images | `scripts/aws/eks_publish.sh` | no | yes (ECR) | Billable. Needs Docker. Does not need the cluster. |
| create cluster | `terraform -chdir=deploy/aws/eks apply` | no | yes | Run on the laptop anyway: state is LOCAL and `eks_deploy.sh` reads `terraform output` from it. |
| kubeconfig | `aws eks update-kubeconfig ...` | no (writes local config) | yes | |
| deploy | `scripts/aws/eks_deploy.sh` | **yes** | yes | `kubectl apply`, secret creation, `rollout status`. |
| smoke | `scripts/aws/eks_smoke.sh` | **yes** | no | `kubectl get` / `kubectl exec`. |
| load point | `scripts/aws/eks_loadgen.sh` | **yes** | yes (account id) | Applies a Job, waits, reads logs, `kubectl top`. |
| evidence | `scripts/aws/collect_eks_evidence.sh <campaign>` | **yes** | yes (identity) | Run BEFORE teardown. |
| teardown | `scripts/aws/eks_teardown.sh` | **yes** | yes | Its `kubectl delete` steps end in `|| true`. Run anywhere else, they fail silently, `terraform destroy` still runs, and PVC-backed EBS volumes / any LoadBalancer are orphaned and keep billing. |
| any ad-hoc `kubectl` | `kubectl get/logs/exec/port-forward/top` | **yes** | no | |

`kubectl kustomize` and `kubectl create --dry-run=client` are local-only, but
they run inside `eks_deploy.sh`, which needs the API anyway.

## Laptop prerequisites

- Terraform >= 1.9 (the lock file pins aws 5.100.0, random 3.9.1), AWS CLI v2,
  kubectl within one minor version of 1.37, Docker with linux/amd64 builds,
  python3 with PyYAML (used by `eks_deploy.sh`).
- AWS profile `aiops-deploy` (the default in `scripts/aws/_common.sh`).
- `deploy/aws/eks/terraform.tfvars` is gitignored, so it does NOT travel with
  the repository. Recreate it on the laptop:

      operator_cidr = "157.50.241.83/32"
      enable_db_ssh = false

- If the laptop's public address changes, the API becomes unreachable. Check
  with `curl -s https://checkip.amazonaws.com`, update `operator_cidr`, and
  re-apply. That is an in-place update of the cluster's `public_access_cidrs`.

## Credentials and Terraform state

- The database passwords live in a Secrets Manager secret
  (`terraform output database_secret_arn`). Only the database host's instance
  role can read it; the EKS node role cannot. `eks_deploy.sh` reads it with the
  laptop's `aiops-deploy` credentials, which therefore need
  `secretsmanager:GetSecretValue` on that secret.
- `user_data` holds only the secret's ARN, never a password.
- **`terraform.tfstate` still contains both passwords in plaintext**
  (`random_password` and the secret version). Treat it as a credential file:
  never commit, copy or share it; delete it after a successful destroy.

## Status of the issues listed here earlier

Verified on 2026-10-30; see `docs/cloud-deployment/PRE-DEPLOYMENT-VERIFICATION.md`.

- Distroless engine vs `kubectl exec ... sh`: fixed upstream (`_k8s.sh` uses the
  API pod proxy and a probe pod). Smoke passes 10/10 locally.
- Results directory mismatch: fixed (`OUT` defaults to `../docs/results/cloud-campaign`).
- `eks_teardown.sh` now refuses to destroy unless the Kubernetes API answers,
  so it must still be run from this laptop.
- NodePort Services (30080-30082) remain in the overlay; keep the EKS cluster
  security group free of added rules.
