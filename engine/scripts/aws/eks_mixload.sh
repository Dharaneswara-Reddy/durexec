#!/usr/bin/env bash
# One MIXED-WORKLOAD point on the EKS cluster: cmd/mixload as a Job on the
# harness node, with the same evidence as eks_loadgen.sh (pg_stat before /
# during / after, kubelet CPU samples, engine metrics, journal invariants).
#
#   EXP=M-mix CONC=64 DUR=120s WARMUP=15s MIX='DeliveryOrder=30,...' TRIAL=1 scripts/aws/eks_mixload.sh
#   RATE=40 ... runs open loop instead of closed loop.
#
# Results: $OUT/mixload/<run_id>-<cfg>/ (types.csv, agent.csv, logs, raw).
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/aws/_common.sh
enable_evidence_tee
require_confirmation "run a mixed-workload point against the EKS cluster"
NS="${NS:-durexec-lab}"
. scripts/aws/_k8s.sh

REGION="${AWS_REGION:-us-east-1}"
TAG="${TAG:-$(git rev-parse --short HEAD)}"
CONC="${CONC:-64}"; RATE="${RATE:-0}"; DUR="${DUR:-120s}"; WARMUP="${WARMUP:-15s}"
MIX="${MIX:-DeliveryOrder=30,MixPipeline=15,MixFanOut=15,MixApproval=15,MixRetry=10,DomainAgent=15}"
EXP="${EXP:-M-adhoc}"; TRIAL="${TRIAL:-1}"; SEED="${SEED:-$TRIAL}"
CORPORA="${CORPORA:-flight,multidomain,heldout,heldout2}"
SAMPLE_EVERY="${SAMPLE_EVERY:-3}"
OUT="${OUT:-../docs/results/cloud-campaign}"
PODS=$(engine_pods); [ -n "$PODS" ] || { echo "FATAL: no engine pods" >&2; exit 1; }
REPLICAS=$(wc -w <<<"$PODS" | tr -d ' ')
SEMF=$(kubectl -n "$NS" get configmap durexec-config -o jsonpath='{.data.SEMANTIC_FIELDS}')
LOAD=$([ "$RATE" != 0 ] && echo "rate${RATE}" || echo "c${CONC}")
MIXTAG=$(tr -cd 'A-Za-z0-9=,' <<<"$MIX" | sed 's/DeliveryOrder/D/;s/MixPipeline/P/;s/MixFanOut/F/;s/MixApproval/A/;s/MixRetry/R/;s/DomainAgent/G/' | tr -d '=,')
CFG="r${REPLICAS}-${SEMF:-semantic}-${LOAD}-${MIXTAG}-d${DUR}-t${TRIAL}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
JOB="mixload-${RUN_ID,,}"; JOB="${JOB//[^a-z0-9-]/}"
RAW="$OUT/mixload/${RUN_ID}-${CFG}"; mkdir -p "$RAW"
IDP="mx-${RUN_ID,,}-"

ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
IMAGE="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com/durexec/loadgen:${TAG}"
BASES=""; for p in $PODS; do BASES="${BASES},http://${p}.durexec-hl.${NS}.svc.cluster.local:8080"; done
BASES="${BASES#,}"
ARGS=$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' \
  -nodes "$BASES" -mix "$MIX" -concurrency "$CONC" -rate "$RATE" -duration "$DUR" -warmup "$WARMUP" \
  -agent-corpora "$CORPORA" -id-prefix "$IDP" -seed "$SEED" -out /tmp/mix -emit-csv)

{ echo "run_id=$RUN_ID"; echo "exp=$EXP"; echo "config_id=$CFG"; echo "git_sha=$(git rev-parse HEAD)"
  echo "image_tag=$TAG"; echo "mix=$MIX"; echo "semantic_fields=${SEMF:-semantic}"; echo "args=$ARGS"
  kubectl -n "$NS" get pods -o wide 2>&1; } > "$RAW/00-provenance.txt"
kubectl -n "$NS" get configmap durexec-config -o yaml > "$RAW/01-configmap.yaml" 2>&1 || true
for p in $PODS; do engine_get "$p" /metrics > "$RAW/39-metrics-before-$p.txt" 2>&1 || true; done
probe_ensure
probe_sql < scripts/aws/sql/pgstat_snapshot.sql > "$RAW/11-pgstat-before.txt" 2>&1 || true

sed -e "s|LOADGEN_JOB_NAME|$JOB|" -e "s|LOADGEN_IMAGE|$IMAGE|" -e "s|LOADGEN_ARGS|$ARGS|" \
    -e 's|          args: |          command: ["/mixload"]\n          args: |' \
    deploy/kubernetes/loadgen/job.yaml | kubectl apply -f - >/dev/null

NODES=$(kubectl get nodes -l 'durexec.io/role in (engine,harness)' -o jsonpath='{.items[*].metadata.name}')
sampler() { while :; do for n in $NODES; do
  s=$(node_summary "$n" 2>/dev/null) && printf '{"node":"%s","summary":%s}\n' "$n" "$(tr -d '\n' <<<"$s")"
  done >> "$RAW/20-kubelet-samples.jsonl"; sleep "$SAMPLE_EVERY"; done; }
pgsampler() { while :; do probe_sql < scripts/aws/sql/pgstat_snapshot.sql >> "$RAW/12-pgstat-during.txt" 2>/dev/null || true; sleep 5; done; }
sampler & SP=$!; pgsampler & PP=$!
cleanup() { kill "$SP" "$PP" 2>/dev/null || true; wait "$SP" "$PP" 2>/dev/null || true; }
trap cleanup EXIT
kubectl -n "$NS" wait --for=condition=complete --timeout=1500s "job/$JOB" >/dev/null 2>&1 \
  || kubectl -n "$NS" wait --for=condition=failed --timeout=10s "job/$JOB" >/dev/null 2>&1 || true
cleanup; trap - EXIT

probe_sql < scripts/aws/sql/pgstat_snapshot.sql > "$RAW/13-pgstat-after.txt" 2>&1 || true
{ printf "\\set prefix '%s'\n" "$IDP"; cat scripts/aws/sql/run_latency_and_invariants.sql; } \
  | probe_sql > "$RAW/14-journal-latency-invariants.txt" 2>&1 || true
kubectl -n "$NS" logs "job/$JOB" > "$RAW/30-mixload.txt" 2>&1 || true
for p in $PODS; do engine_get "$p" /metrics > "$RAW/40-metrics-$p.txt" 2>&1 || true; done
kubectl -n "$NS" get events --sort-by=.lastTimestamp > "$RAW/50-events.txt" 2>&1 || true
python3 scripts/aws/pgstat_delta.py "$RAW/11-pgstat-before.txt" "$RAW/13-pgstat-after.txt" \
  "$RAW/12-pgstat-during.txt" > "$RAW/60-pg-reduced.txt" 2>&1 || true
python3 scripts/aws/k8s_usage.py "$RAW/20-kubelet-samples.jsonl" > "$RAW/61-k8s-reduced.txt" 2>&1 || true
MPAIRS=(); for p in $PODS; do MPAIRS+=("$RAW/39-metrics-before-$p.txt" "$RAW/40-metrics-$p.txt"); done
python3 scripts/aws/metrics_delta.py "${MPAIRS[@]}" > "$RAW/63-metrics-reduced.txt" 2>&1 || true
# Split the CSVs mixload printed between its markers.
awk '/^=== BEGIN (.*) ===$/{f=$3; next} /^=== END /{f=""; next} f{print > ("'"$RAW"'/" f)}' "$RAW/30-mixload.txt"
JOBST=$(kubectl -n "$NS" get job "$JOB" -o jsonpath='{.status.conditions[0].type}' 2>/dev/null || true)
echo "$RUN_ID $CFG job=$JOBST raw=$RAW"
sed -n '/^type /,/^ALL /p' "$RAW/30-mixload.txt"
grep -A6 "^DomainAgent, the engine" "$RAW/30-mixload.txt" || true
grep "^inv|" "$RAW/14-journal-latency-invariants.txt" | head -10 || true
