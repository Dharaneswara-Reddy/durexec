#!/usr/bin/env bash
# Measure the deployed engine and database with no workload generator active.
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/aws/_common.sh
enable_evidence_tee

if [ "${1:-}" = "--self-test" ]; then
  python3 scripts/aws/idle_baseline.py --self-test
  exit 0
fi
require_confirmation "measure an idle baseline on the running EKS pilot"

NS="${NS:-durexec-lab}"
SECONDS_TO_MEASURE="${DUR_SECONDS:-120}"
SAMPLE_EVERY="${SAMPLE_EVERY:-3}"
SETTLE_SECONDS="${SETTLE_SECONDS:-15}"
OUT="${OUT:-../docs/results/cloud-campaign/idle-baselines}"
[[ "$SECONDS_TO_MEASURE" =~ ^[0-9]+$ ]] && [ "$SECONDS_TO_MEASURE" -ge 30 ] \
  || { echo "FATAL: DUR_SECONDS must be an integer >= 30" >&2; exit 2; }
[[ "$SAMPLE_EVERY" =~ ^[0-9]+$ ]] && [ "$SAMPLE_EVERY" -ge 1 ] \
  || { echo "FATAL: SAMPLE_EVERY must be a positive integer" >&2; exit 2; }
[[ "$SETTLE_SECONDS" =~ ^[0-9]+$ ]] \
  || { echo "FATAL: SETTLE_SECONDS must be a non-negative integer" >&2; exit 2; }
. scripts/aws/_k8s.sh

PODS=$(engine_pods)
[ -n "$PODS" ] || { echo "FATAL: no engine pods in $NS" >&2; exit 1; }
CONFIG=$(kubectl -n "$NS" get configmap durexec-config -o jsonpath='{.data.SHARD_COUNT}|{.data.WORKFLOW_WORKERS}|{.data.ACTIVITY_WORKERS}|{.data.SYNC_COMMIT}')
IFS='|' read -r SHARDS WF_WORKERS ACT_WORKERS SYNC_COMMIT <<<"$CONFIG"
REPLICAS=$(wc -w <<<"$PODS" | tr -d ' ')
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
CFG="r${REPLICAS}-S${SHARDS}-w${WF_WORKERS}x${ACT_WORKERS}-sc${SYNC_COMMIT}"
RAW="$OUT/${RUN_ID}-${CFG}"
mkdir -p "$RAW"

ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
echo "AWS account=$ACCOUNT region=$AWS_REGION"

# Refuse to call a busy cluster idle. No new workload is started by this tool.
ACTIVE_JOBS=$(kubectl -n "$NS" get jobs -o json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(j.get("status",{}).get("active",0) for j in d.get("items",[])))')
[ "$ACTIVE_JOBS" = 0 ] || { echo "FATAL: $ACTIVE_JOBS active Kubernetes job(s) in $NS" >&2; exit 1; }
probe_ensure
echo "settling for ${SETTLE_SECONDS}s before taking idle-baseline snapshots"
sleep "$SETTLE_SECONDS"
ACTIVE_JOBS=$(kubectl -n "$NS" get jobs -o json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(j.get("status",{}).get("active",0) for j in d.get("items",[])))')
[ "$ACTIVE_JOBS" = 0 ] || { echo "FATAL: cluster became busy during settling" >&2; exit 1; }
IDLE_COUNTS=$(printf '%s\n' "select (select count(*) from executions where status not in ('COMPLETED','FAILED','COMPENSATING_FAILED')) || '|' || (select count(*) from tasks);" | probe_sql)
read -r NONTERMINAL PENDING <<<"${IDLE_COUNTS//|/ }"
[ "${NONTERMINAL:-x}" = 0 ] && [ "${PENDING:-x}" = 0 ] \
  || { echo "FATAL: cluster is not quiescent (nonterminal=$NONTERMINAL pending_tasks=$PENDING)" >&2; exit 1; }
BASE_EXECUTIONS=$(printf '%s\n' "select count(*) from executions;" | probe_sql)

{
  echo "run_id=$RUN_ID"
  echo "config_id=$CFG"
  echo "git_sha=$(git rev-parse HEAD)"
  echo "git_tree=$(git diff --quiet && echo clean || echo dirty)"
  echo "window_seconds=$SECONDS_TO_MEASURE"
  echo "settle_seconds=$SETTLE_SECONDS"
  echo "pre_window_nonterminal=$NONTERMINAL"
  echo "pre_window_pending_tasks=$PENDING"
  echo "nodes (name, role, EC2 instance type, zone):"
  kubectl get nodes -o json | python3 scripts/aws/node_inventory.py
  echo "pods:"
  kubectl -n "$NS" get pods -l app=durexec -o wide
} > "$RAW/00-provenance.txt"

probe_sql < scripts/aws/sql/pgstat_snapshot.sql > "$RAW/10-pg-before.txt"
BEFORE_FILES=()
AFTER_FILES=()
for p in $PODS; do
  engine_get "$p" /metrics > "$RAW/20-metrics-before-$p.txt"
  BEFORE_FILES+=("$RAW/20-metrics-before-$p.txt")
done

NODES=$(kubectl get nodes -l durexec.io/role=engine -o jsonpath='{.items[*].metadata.name}')
sampler() {
  while :; do
    for node in $NODES; do
      summary=$(node_summary "$node" 2>/dev/null) \
        && printf '{"node":"%s","summary":%s}\n' "$node" "$(tr -d '\n' <<<"$summary")"
    done >> "$RAW/30-kubelet-samples.jsonl"
    sleep "$SAMPLE_EVERY"
  done
}
sampler & SAMPLE_PID=$!
stop_sampler() { kill "$SAMPLE_PID" 2>/dev/null || true; wait "$SAMPLE_PID" 2>/dev/null || true; }
trap stop_sampler EXIT

START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "idle measurement started=$START duration=${SECONDS_TO_MEASURE}s config=$CFG"
sleep "$SECONDS_TO_MEASURE"
END=$(date -u +%Y-%m-%dT%H:%M:%SZ)
stop_sampler
trap - EXIT

probe_sql < scripts/aws/sql/pgstat_snapshot.sql > "$RAW/11-pg-after.txt"
for p in $PODS; do
  engine_get "$p" /metrics > "$RAW/21-metrics-after-$p.txt"
  AFTER_FILES+=("$RAW/21-metrics-after-$p.txt")
done
MPAIRS=()
for i in "${!BEFORE_FILES[@]}"; do
  MPAIRS+=("${BEFORE_FILES[$i]}" "${AFTER_FILES[$i]}")
done
END_EXECUTIONS=$(printf '%s\n' "select count(*) from executions;" | probe_sql)
if [ "$END_EXECUTIONS" != "$BASE_EXECUTIONS" ]; then
  echo "FATAL: idle baseline was contaminated by new workflow executions ($BASE_EXECUTIONS -> $END_EXECUTIONS)" >&2
  exit 1
fi
printf 'window_start=%s\nwindow_end=%s\n' "$START" "$END" > "$RAW/01-window.txt"

python3 scripts/aws/pgstat_delta.py "$RAW/10-pg-before.txt" "$RAW/11-pg-after.txt" \
  > "$RAW/40-pg-reduced.txt"
python3 scripts/aws/metrics_delta.py "${MPAIRS[@]}" > "$RAW/41-engine-reduced.txt"
python3 scripts/aws/k8s_usage.py "$RAW/30-kubelet-samples.jsonl" > "$RAW/42-node-reduced.txt"
python3 scripts/aws/idle_baseline.py "$RAW/40-pg-reduced.txt" "$RAW/41-engine-reduced.txt" \
  > "$RAW/50-idle-summary.txt"
cat "$RAW/50-idle-summary.txt"
echo "idle baseline evidence: $RAW"
