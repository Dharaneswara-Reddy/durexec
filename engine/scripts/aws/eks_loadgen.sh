#!/usr/bin/env bash
# Run ONE load point in the cloud and persist its evidence before returning.
#
# Replaces the Compose-only path in experiments/scripts/shard_sweep.sh, which
# calls `docker exec deploy-postgres-leader-1` and cannot work against EKS.
#
# Results are LOCAL EXPLORATORY until the engine, database and generator are
# each on their own node AND the run is attributed; see
# docs/results/local-shard-sweep/FINDINGS.md for why an unattributed point is
# worse than a missing one.
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/aws/_common.sh
require_confirmation "run a load point against the EKS cluster"

NS="${NS:-durexec-lab}"
REGION="${AWS_REGION:-us-east-1}"
TAG="${TAG:-$(git rev-parse --short HEAD)}"
SHARDS="${SHARDS:-16}"; CONC="${CONC:-128}"; DUR="${DUR:-25s}"
# A pipeline SMOKE run proves the chain works; a PERF run is a measurement.
# They must never merge into one curve, so the kind is recorded per row.
KIND="${KIND:-smoke}"
# Generator saturation threshold, in CPU cores. The Job requests 500m and has
# no limit, so a generator using at or above this much of a 2-vCPU c7i.large
# is plausibly the constraint rather than the engine. Configurable because the
# right value depends on the harness instance; 1.6 of 2 cores is the default.
GEN_SATURATED_CORES="${GEN_SATURATED_CORES:-1.6}"
# Below this many completions a point is too small to interpret.
MIN_SAMPLES="${MIN_SAMPLES:-150}"
OUT="${OUT:-docs/results/eks-pilot}"; mkdir -p "$OUT/raw"
CSV="$OUT/results.csv"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
CFG="shards${SHARDS}-c${CONC}-d${DUR}"
JOB="loadgen-$(echo "$CFG" | tr -cd 'a-z0-9-')-${RUN_ID: -6}"

[ -f "$CSV" ] || {
  echo "# durexec EKS pilot. resume: re-run; completed config_ids are skipped." > "$CSV"
  echo "run_id,config_id,ts_start,ts_end,git_sha,shards,concurrency,duration,started,completed,wf_per_s,p50_s,p99_s,errors,owner_nodes,gen_node,gen_role,gen_cpu_cores,gen_restarts,gen_exit,eng_cpu_pct,job_status,kind,verdict,raw_file" >> "$CSV"
}
grep -q ",$CFG," "$CSV" && { echo "$CFG already recorded, skipping"; exit 0; }

ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
IMAGE="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com/durexec/loadgen:${TAG}"

# Per-pod headless DNS, not the ClusterIP Service: loadgen follows 409 NotOwner
# redirects to a named owner, and a round-robin Service would send the retry
# to an arbitrary pod instead of the one that owns the shard.
PODS=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{.items[*].metadata.name}')
[ -n "$PODS" ] || { echo "FATAL: no engine pods in $NS" >&2; exit 1; }
BASES=""; for p in $PODS; do BASES="${BASES},http://${p}.durexec-hl.${NS}.svc.cluster.local:8080"; done
BASES="${BASES#,}"

ARGS=$(printf '["-mode","scaling","-sweep","%s","-concurrency","%s","-duration","%s","-base","%s"]' \
        "$SHARDS" "$CONC" "$DUR" "$BASES")

TS_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RAWFILE="$OUT/raw/${RUN_ID}-${CFG}.txt"

sed -e "s|LOADGEN_JOB_NAME|$JOB|" -e "s|LOADGEN_IMAGE|$IMAGE|" -e "s|LOADGEN_ARGS|$ARGS|" \
    deploy/kubernetes/loadgen/job.yaml | kubectl apply -f - >/dev/null

kubectl -n "$NS" wait --for=condition=complete --timeout=900s "job/$JOB" 2>/dev/null \
  || kubectl -n "$NS" wait --for=condition=failed --timeout=10s "job/$JOB" 2>/dev/null || true
kubectl -n "$NS" logs "job/$JOB" > "$RAWFILE" 2>&1 || true
TS_END="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# PROVE the generator ran where it was meant to. A Job that landed on an engine
# node measures contention, not capacity.
GEN_POD=$(kubectl -n "$NS" get pods -l job-name="$JOB" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
GEN_NODE=$(kubectl -n "$NS" get pods -l job-name="$JOB" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)
GEN_ROLE=$(kubectl get node "$GEN_NODE" -o jsonpath='{.metadata.labels.durexec\.io/role}' 2>/dev/null)
GEN_RESTARTS=$(kubectl -n "$NS" get pods -l job-name="$JOB" -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)
GEN_EXIT=$(kubectl -n "$NS" get pods -l job-name="$JOB" -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null)
JOB_STATUS=$(kubectl -n "$NS" get job "$JOB" -o jsonpath='{.status.conditions[0].type}' 2>/dev/null)

# Generator CPU. metrics-server is NOT installed by default on EKS, so this is
# frequently unavailable -- recorded as empty rather than guessed, because an
# invented utilisation figure would let a generator-bound run be published as
# engine capacity.
GEN_CPU_RAW=$(kubectl -n "$NS" top pod "$GEN_POD" --no-headers 2>/dev/null | awk '{print $2}')
GEN_CPU_CORES=$(printf '%s' "${GEN_CPU_RAW:-}" | awk '/m$/{sub(/m$/,"");printf "%.3f",$1/1000;exit} /^[0-9.]+$/{printf "%.3f",$1;exit}')
ENG_CPU_PCT=$(kubectl -n "$NS" top pod -l app=durexec --no-headers 2>/dev/null \
  | awk '{c=$2; sub(/m$/,"",c); s+=c} END{if(NR>0) printf "%.0f", s/10}')

lat() { awk -v k="latency_p$1 @" 'index($0,k) && !s { for(i=1;i<=NF;i++) if($i ~ /^[0-9]+\.?[0-9]*$/){print $i;s=1;exit} }' "$2"; }
STARTED=$(grep -oE 'started [0-9]+' "$RAWFILE" | tail -1 | awk '{print $2}')
COMPLETED=$(grep -oE 'completed [0-9]+' "$RAWFILE" | tail -1 | awk '{print $2}')
WFPS=$(grep -oE '= [0-9.]+ wf/s' "$RAWFILE" | tail -1 | awk '{print $2}')
ERRS=$(grep -oE 'failed [0-9]+' "$RAWFILE" | tail -1 | awk '{print $2}')
OWNERS=$(sed -n 's/.*owned by \[\([^]]*\)\].*/\1/p' "$RAWFILE" | tail -1 | wc -w)

# Ordered so the most damning problem wins. A run that is invalid for ANY of
# these reasons must never reach a capacity curve.
if [ "$JOB_STATUS" = "Failed" ]; then
  VERDICT="INVALID_job_failed"
elif [ -n "${GEN_EXIT:-}" ] && [ "${GEN_EXIT:-0}" != "0" ]; then
  VERDICT="INVALID_generator_exit_${GEN_EXIT}"
elif [ "${GEN_RESTARTS:-0}" != "0" ]; then
  VERDICT="INVALID_generator_restarted_${GEN_RESTARTS}x"
elif [ "$GEN_ROLE" != "harness" ]; then
  VERDICT="INVALID_generator_on_${GEN_ROLE:-unknown}_node"
elif [ ! -s "$RAWFILE" ]; then
  VERDICT="INVALID_no_output"
elif [ -z "$WFPS" ]; then
  VERDICT="UNPARSEABLE"
elif [ -z "${COMPLETED:-}" ] || [ "${COMPLETED:-0}" -lt "$MIN_SAMPLES" ]; then
  VERDICT="INCONCLUSIVE_only_${COMPLETED:-0}_samples"
elif [ -n "$GEN_CPU_CORES" ] && awk "BEGIN{exit !($GEN_CPU_CORES >= $GEN_SATURATED_CORES)}"; then
  VERDICT="GENERATOR_SATURATED_${GEN_CPU_CORES}_cores"
elif [ "$KIND" = smoke ]; then
  VERDICT="SMOKE_ok"
else
  VERDICT="RECORDED"
fi

printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
  "$RUN_ID" "$CFG" "$TS_START" "$TS_END" "$(git rev-parse --short HEAD)" \
  "$SHARDS" "$CONC" "$DUR" "${STARTED:-}" "${COMPLETED:-}" "${WFPS:-}" \
  "$(lat 50 "$RAWFILE")" "$(lat 99 "$RAWFILE")" "${ERRS:-}" "${OWNERS:-}" \
  "${GEN_NODE:-}" "${GEN_ROLE:-}" "${GEN_CPU_CORES:-}" "${GEN_RESTARTS:-}" "${GEN_EXIT:-}" \
  "${ENG_CPU_PCT:-}" "${JOB_STATUS:-}" "$KIND" \
  "$VERDICT" "$(basename "$RAWFILE")" >> "$CSV"
sync "$CSV" 2>/dev/null || true

kubectl -n "$NS" delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true
echo "$CFG -> ${WFPS:-?} wf/s  gen_cpu=${GEN_CPU_CORES:-unavailable} cores on ${GEN_NODE:-?} (${GEN_ROLE:-?})  kind=$KIND  verdict=$VERDICT"
case "$VERDICT" in INVALID*|UNPARSEABLE) exit 1 ;; esac
