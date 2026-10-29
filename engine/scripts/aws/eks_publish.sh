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
TAG="${TAG:-$(git rev-parse --short HEAD)}"
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
