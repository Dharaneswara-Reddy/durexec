#!/usr/bin/env bash
# Deploy durexec onto the EKS pilot cluster against the EXTERNAL PostgreSQL
# host. Applies Kubernetes workloads; creates no AWS resource directly, but the
# cluster it targets is billing, so it is gated.
#
# The database endpoint and password are read from Terraform at run time. No IP
# address and no credential is ever written to a tracked file.
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/aws/_common.sh
require_confirmation "deploy workloads onto the running EKS cluster"

NS="${NS:-durexec-lab}"
TF="deploy/aws/eks"
REGION="${AWS_REGION:-us-east-1}"
TAG="${TAG:-$(git rev-parse --short HEAD)}"
RENDER="$(mktemp -d)/rendered.yaml"
trap 'rm -rf "$(dirname "$RENDER")"' EXIT

# --- 1. endpoint from Terraform, never hardcoded ---------------------------
DB_HOST="$(terraform -chdir="$TF" output -raw database_private_ip 2>/dev/null || true)"
if [ -z "$DB_HOST" ]; then
  echo "FATAL: terraform output database_private_ip is empty." >&2
  echo "       Has 'terraform -chdir=$TF apply' been run?" >&2
  exit 1
fi
# A private RFC1918 address is the only acceptable answer. A public address
# here would mean pod-to-database traffic leaves the VPC: slower, billed as
# transfer, and needlessly exposed.
case "$DB_HOST" in
  10.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*|192.168.*) : ;;
  *) echo "FATAL: database_private_ip '$DB_HOST' is not an RFC1918 address." >&2; exit 1 ;;
esac
echo "database endpoint : $DB_HOST (private)"

ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
IMAGE="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com/durexec/engine:${TAG}"
echo "engine image      : ${IMAGE%%.*}...ecr.../durexec/engine:${TAG}"

# Prove the tag exists before deploying. Otherwise the first symptom is
# ImagePullBackOff several minutes later, which reads like a cluster fault.
aws ecr describe-images --region "$REGION" --repository-name durexec/engine \
  --image-ids imageTag="$TAG" >/dev/null 2>&1 \
  || { echo "FATAL: durexec/engine:$TAG not in ECR. Run eks_publish.sh first." >&2; exit 1; }

# --- 2. secret, built in memory --------------------------------------------
# The engine reads DATABASE_URL from secret durexec-db key 'url'
# (deploy/kubernetes/base/30-engine.yaml:142-144). Not a ConfigMap.
DB_PASS="$(terraform -chdir="$TF" output -raw database_password 2>/dev/null || true)"
[ -n "$DB_PASS" ] || { echo "FATAL: database_password output is empty." >&2; exit 1; }

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# --dry-run | apply keeps this idempotent AND keeps the literal off the
# process table of any other user on this machine.
kubectl -n "$NS" create secret generic durexec-db \
  --from-literal=password="$DB_PASS" \
  --from-literal=url="postgres://durexec:${DB_PASS}@${DB_HOST}:5432/durexec?sslmode=disable" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
unset DB_PASS
echo "secret durexec-db : created (value never printed)"

# --- 3. render and substitute ----------------------------------------------
kubectl kustomize deploy/kubernetes/overlays/eks > "$RENDER"
sed -i "s|DB_HOST_PLACEHOLDER|${DB_HOST}|g; s|IMAGE_PLACEHOLDER|${IMAGE}|g" "$RENDER"

# --- 4. refuse to apply a half-substituted manifest ------------------------
# Checking the RENDERED file, not the template: a patch that failed to match
# leaves the template correct and the output wrong, which is precisely the
# failure mode that has bitten this project before.
if grep -q 'PLACEHOLDER' "$RENDER"; then
  echo "FATAL: placeholders survived substitution:" >&2
  grep -n 'PLACEHOLDER' "$RENDER" >&2
  exit 1
fi
# Prove the substitution reached the objects that matter, not merely that the
# token is gone.
python3 - "$RENDER" "$DB_HOST" "$IMAGE" <<'PY'
import sys, yaml
f, host, image = sys.argv[1], sys.argv[2], sys.argv[3]
docs = [d for d in yaml.safe_load_all(open(f)) if d]
sts = [d for d in docs if d["kind"] == "StatefulSet" and d["metadata"]["name"] == "durexec"]
if not sts:
    sys.exit("FATAL: engine StatefulSet missing from rendered output")
spec = sts[0]["spec"]["template"]["spec"]
init = " ".join(" ".join(c.get("command", [])) for c in spec.get("initContainers", []))
if host not in init:
    sys.exit("FATAL: init container does not reference the external database host")
imgs = [c["image"] for c in spec["containers"] if c["name"] == "engined"]
if imgs != [image]:
    sys.exit(f"FATAL: engine image is {imgs}, expected {image}")
if any(d["kind"] == "StatefulSet" and d["metadata"]["name"] == "postgres" for d in docs):
    sys.exit("FATAL: in-cluster postgres still present")
print("verified          : init gate, image tag and postgres removal all correct")
PY

# --- 5. apply ---------------------------------------------------------------
kubectl apply -f "$RENDER"
kubectl -n "$NS" rollout status statefulset/durexec --timeout=420s
echo
kubectl -n "$NS" get pods -o wide
