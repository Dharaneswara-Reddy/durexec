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
enable_evidence_tee
require_confirmation "deploy workloads onto the running EKS cluster"

NS="${NS:-durexec-lab}"
TF="deploy/aws/eks"
REGION="${AWS_REGION:-us-east-1}"
TAG="${TAG:-$(git rev-parse --short HEAD)}"
RENDER="$(mktemp -d)/rendered.yaml"

# --- experimental parameters ---------------------------------------------------
# The base manifest ships replicas=2 with a two-name ring, so deploying it
# unchanged onto a three-node engine group ran a TWO-engine experiment that
# every smoke check still passed. Replicas now default to the number of
# engine-role nodes, and the ring, NODE_COUNT and replicas are always written
# together from the one value.
ENGINE_NODES=$(kubectl get nodes -l durexec.io/role=engine --no-headers 2>/dev/null | wc -l | tr -d ' ')
ENGINE_REPLICAS="${ENGINE_REPLICAS:-$ENGINE_NODES}"
SHARD_COUNT="${SHARD_COUNT:-16}"
WORKFLOW_WORKERS="${WORKFLOW_WORKERS:-4}"
ACTIVITY_WORKERS="${ACTIVITY_WORKERS:-8}"
SYNC_COMMIT="${SYNC_COMMIT:-on}"
[ "$ENGINE_REPLICAS" -ge 1 ] || { echo "FATAL: no engine-role nodes and ENGINE_REPLICAS unset" >&2; exit 1; }
# Required anti-affinity means a replica beyond the node count stays Pending.
[ "$ENGINE_REPLICAS" -le "$ENGINE_NODES" ] \
  || { echo "FATAL: ENGINE_REPLICAS=$ENGINE_REPLICAS > $ENGINE_NODES engine nodes; anti-affinity would leave pods Pending" >&2; exit 1; }
echo "experiment        : replicas=$ENGINE_REPLICAS shards=$SHARD_COUNT workers=${WORKFLOW_WORKERS}x${ACTIVITY_WORKERS} sync_commit=$SYNC_COMMIT"
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
# From Secrets Manager with the OPERATOR's credentials; Terraform no longer
# outputs the password. Parsed in memory and never echoed.
SECRET_ARN="$(terraform -chdir="$TF" output -raw database_secret_arn 2>/dev/null || true)"
[ -n "$SECRET_ARN" ] || { echo "FATAL: terraform output database_secret_arn is empty." >&2; exit 1; }
DB_PASS="$(aws secretsmanager get-secret-value --region "$REGION" --secret-id "$SECRET_ARN" \
            --query SecretString --output text 2>/dev/null \
          | python3 -c 'import json,sys; print(json.load(sys.stdin)["postgres_password"])' 2>/dev/null || true)"
[ -n "$DB_PASS" ] || { echo "FATAL: could not read postgres_password from the database secret." >&2; exit 1; }

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
python3 - "$RENDER" "$ENGINE_REPLICAS" "$SHARD_COUNT" "$WORKFLOW_WORKERS" "$ACTIVITY_WORKERS" "$SYNC_COMMIT" <<'PY'
import sys, yaml
f, reps, shards, wfw, actw, sc = sys.argv[1:]
docs = [d for d in yaml.safe_load_all(open(f)) if d]
for d in docs:
    if d["kind"] == "StatefulSet" and d["metadata"]["name"] == "durexec":
        d["spec"]["replicas"] = int(reps)
    if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "durexec-config":
        d["data"].update({
            "SHARD_COUNT": shards, "NODE_COUNT": reps,
            "CLUSTER_NODES": ",".join(f"durexec-{i}.durexec-hl" for i in range(int(reps))),
            "WORKFLOW_WORKERS": wfw, "ACTIVITY_WORKERS": actw, "SYNC_COMMIT": sc,
        })
yaml.safe_dump_all(docs, open(f, "w"), sort_keys=False)
PY

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
# The overlay passes the host to the gate as env DB_HOST and the command reads
# "$DB_HOST", so the address appears in the env, not in the command text.
# Checking only the command (as this did) failed on EVERY correct render.
inits = spec.get("initContainers", [])
gate_hosts = [e.get("value") for c in inits for e in c.get("env", []) if e.get("name") == "DB_HOST"]
gate_cmd = " ".join(" ".join(c.get("command", [])) for c in inits)
if host not in gate_hosts and host not in gate_cmd:
    sys.exit(f"FATAL: init gate does not reference the external database host (env DB_HOST={gate_hosts})")
imgs = [c["image"] for c in spec["containers"] if c["name"] == "engined"]
if imgs != [image]:
    sys.exit(f"FATAL: engine image is {imgs}, expected {image}")
if any(d["kind"] == "StatefulSet" and d["metadata"]["name"] == "postgres" for d in docs):
    sys.exit("FATAL: in-cluster postgres still present")
# Every image must be one EKS can pull: the ECR engine image or the public
# pgvector image the init gate uses. A local tag inherited from the base (as
# extsim's durexec-extsimd:phase7 was) sits in ImagePullBackOff for ever.
pullable = {image, "pgvector/pgvector:pg16"}
for d in docs:
    pod = d.get("spec", {}).get("template", {}).get("spec", {})
    for c in pod.get("initContainers", []) + pod.get("containers", []):
        if c["image"] not in pullable:
            sys.exit(f"FATAL: {d['kind']}/{d['metadata']['name']} uses image {c['image']!r}, which EKS cannot pull")
cm = [d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "durexec-config"][0]["data"]
reps = sts[0]["spec"]["replicas"]
ring = cm["CLUSTER_NODES"].split(",")
if not (int(cm["NODE_COUNT"]) == reps == len(ring)):
    sys.exit(f"FATAL: replicas={reps} NODE_COUNT={cm['NODE_COUNT']} ring={len(ring)} disagree")
if ring != [f"durexec-{i}.durexec-hl" for i in range(reps)]:
    sys.exit(f"FATAL: ring {ring} does not name exactly the StatefulSet's pods")
print("verified          : init gate, image tag and postgres removal all correct")
print(f"verified          : replicas={reps} = NODE_COUNT = ring length; shards={cm['SHARD_COUNT']}")
PY

# --- 4b. reconfiguration safety -------------------------------------------------
. scripts/aws/_k8s.sh
probe_ensure
# Changing the shard count re-hashes every workflow id. Rows written under the
# old count would sit in shards nobody serves, so a change against a database
# that already holds workflows is refused unless a reset is explicitly asked
# for. The reset touches ONLY the pilot's own schema on the pilot's own host.
OLD_SHARDS=$(echo "select count(*) from shard_leases;" | probe_sql 2>/dev/null || echo 0)
ROWS=$(echo "select count(*) from executions;" | probe_sql 2>/dev/null || echo 0)
NEED_RESET=no
if [ "${OLD_SHARDS:-0}" != 0 ] && [ "$OLD_SHARDS" != "$SHARD_COUNT" ] && [ "${ROWS:-0}" != 0 ]; then
  NEED_RESET=yes
fi
if [ "$NEED_RESET" = yes ] || [ "${RESET_DB:-}" = "i-understand-this-deletes-pilot-data" ]; then
  [ "${RESET_DB:-}" = "i-understand-this-deletes-pilot-data" ] || {
    echo "FATAL: SHARD_COUNT $OLD_SHARDS -> $SHARD_COUNT with $ROWS executions in the database." >&2
    echo "       Collect evidence first, then re-run with RESET_DB=i-understand-this-deletes-pilot-data" >&2
    exit 1; }
fi
# A ring change must never be rolled pod by pod: for the length of the
# rollout two different rings would be live and disagree about ownership.
# Stop every engine first, then start them all on the new configuration.
if kubectl -n "$NS" get statefulset durexec >/dev/null 2>&1; then
  echo "stopping engines  : scaling to 0 before reconfiguration"
  kubectl -n "$NS" scale statefulset durexec --replicas=0 >/dev/null
  kubectl -n "$NS" wait --for=delete pod -l app=durexec --timeout=180s >/dev/null 2>&1 || true
fi
if [ "${RESET_DB:-}" = "i-understand-this-deletes-pilot-data" ]; then
  echo "resetting schema  : pilot database only ($ROWS executions, $OLD_SHARDS shards discarded)"
  probe_sql <<'SQL'
drop schema public cascade;
create schema public;
SQL
fi

# --- 5. apply ---------------------------------------------------------------
kubectl apply -f "$RENDER"
kubectl -n "$NS" rollout status statefulset/durexec --timeout=420s
# Per-statement latency for the comparison with the local lab. The library is
# preloaded by the DB host's bootstrap; the extension object lives in the
# schema and must exist after every reset.
echo "create extension if not exists pg_stat_statements;" | probe_sql >/dev/null \
  && echo "pg_stat_statements: enabled" \
  || echo "WARNING: pg_stat_statements unavailable; per-statement latency will be missing" >&2
echo
kubectl -n "$NS" get pods -o wide
