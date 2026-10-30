#!/usr/bin/env bash
# Build and publish durexec images to ECR. BILLABLE: creates an ECR repository
# ($0.10/GB-month) and transfers layers.
#
# This account already holds churn-ml-api, churn-ml-training and aiops-demo,
# which belong to OTHER student projects. This script only ever creates and
# writes to repositories under the durexec/ prefix, and never deletes any.
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/aws/_common.sh
require_confirmation "create an ECR repository and push images"

REGION="${AWS_REGION:-us-east-1}"
# The tag is the commit the image is built from. A dirty tree builds code that
# commit does not contain, so an image tagged with the bare SHA would carry a
# false provenance. Refuse unless the operator names the tag explicitly.
if [ -z "${TAG:-}" ]; then
  git diff --quiet && git diff --cached --quiet \
    || { echo "FATAL: working tree is dirty; commit first or set TAG=<sha>-dirty-<desc> explicitly" >&2; exit 1; }
  TAG="$(git rev-parse --short HEAD)"
fi
ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
REGISTRY="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com"

for repo in durexec/engine durexec/extsimd durexec/loadgen; do
  aws ecr describe-repositories --region "$REGION" --repository-names "$repo" >/dev/null 2>&1 \
    || aws ecr create-repository --region "$REGION" --repository-name "$repo" \
         --image-scanning-configuration scanOnPush=false \
         --tags Key=Project,Value=durexec Key=Campaign,Value=k8s-scaling >/dev/null
done

aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$REGISTRY"

# --platform is explicit. EKS nodes here are x86_64; a silently-arm64 image
# produces CrashLoopBackOff with "exec format error", which reads like an
# application fault rather than a build mistake.
docker build --platform linux/amd64 -t "$REGISTRY/durexec/engine:$TAG" -f Dockerfile .
docker build --platform linux/amd64 -t "$REGISTRY/durexec/extsimd:$TAG" -f Dockerfile.extsimd .
# The generator must run in-cluster on its own node; without this image it
# could only run over a port-forward, putting internet RTT in every sample.
docker build --platform linux/amd64 -t "$REGISTRY/durexec/loadgen:$TAG" -f Dockerfile.loadgen .

for img in engine extsimd loadgen; do
  got="$(docker image inspect "$REGISTRY/durexec/$img:$TAG" --format '{{.Os}}/{{.Architecture}}')"
  [ "$got" = "linux/amd64" ] || { echo "FATAL: $img is $got, expected linux/amd64"; exit 1; }
  docker push "$REGISTRY/durexec/$img:$TAG"
done
echo "published $REGISTRY/durexec/{engine,extsimd,loadgen}:$TAG"
# Provenance: the immutable digest of every pushed image, beside the commit.
PROV="$(git rev-parse --show-toplevel)/docs/results/cloud-campaign/images.txt"
mkdir -p "$(dirname "$PROV")"
for img in engine extsimd loadgen; do
  d="$(aws ecr describe-images --region "$REGION" --repository-name "durexec/$img" \
        --image-ids imageTag="$TAG" --query 'imageDetails[0].imageDigest' --output text)"
  printf '%s durexec/%s:%s %s git=%s tree=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$img" "$TAG" "$d" \
    "$(git rev-parse HEAD)" "$(git diff --quiet && echo clean || echo dirty)" >> "$PROV"
done
echo "digests recorded in $PROV"
