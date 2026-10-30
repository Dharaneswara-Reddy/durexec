#!/usr/bin/env bash
# Run ONE load point in the cloud and persist its evidence before returning.
#
# Replaces the Compose-only path in experiments/scripts/shard_sweep.sh, which
# calls `docker exec deploy-postgres-leader-1` and cannot work against EKS.
#
# What is measured, and how, during the SAME window as the load:
#   generator  CPU cores from the kubelet Summary API (cumulative counters, so
#              exact), its node and role, restarts, exit code
#   engine     CPU cores and memory per pod; node CPU against allocatable
#   database   pg_stat_database / pg_stat_wal / pg_stat_bgwriter /
#              pg_stat_statements deltas, connection and lock-wait peaks, a
#              wait-event profile, and the DB HOST's CPU, iowait, steal and
#              disk from /proc read through pg_read_file
#   network    harness->database round trip (psql \timing, select 1)
#
# Each point appends one CSV row and keeps every raw sample under raw/<run>/,
# so an interrupted campaign leaves every completed point usable.
#
# The verdict never upgrades a run: a generator at or above its saturation
# threshold, a missing measurement or too few samples is recorded as such and
# must stay out of any capacity claim.
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/aws/_common.sh
enable_evidence_tee
require_confirmation "run a load point against the EKS cluster"
NS="${NS:-durexec-lab}"
. scripts/aws/_k8s.sh

REGION="${AWS_REGION:-us-east-1}"
TAG="${TAG:-$(git rev-parse --short HEAD)}"
CONC="${CONC:-128}"; DUR="${DUR:-25s}"; WARMUP="${WARMUP:-5s}"
# MODE=scaling is the CLOSED loop (CONC workflows always in flight; throughput
# = CONC / latency, so it can only find a knee if CONC is high enough).
# MODE=arrival is the OPEN loop (RATE starts/s regardless of completions), the
# right tool for locating a knee; it needs FLOOR, the workload's latency floor
# on THIS deployment, measured first by a low-rate calibration point.
MODE="${MODE:-scaling}"; RATE="${RATE:-}"; FLOOR="${FLOOR:-}"
case "$MODE" in
  scaling) : ;;
  arrival) [ -n "$RATE" ] && [ -n "$FLOOR" ] || { echo "FATAL: MODE=arrival needs RATE and FLOOR" >&2; exit 2; } ;;
  *) echo "FATAL: MODE must be scaling or arrival" >&2; exit 2 ;;
esac
# Pipeline SMOKE runs prove the chain; PERF runs are measurements. The kind is
# recorded per row so the two can never merge into one curve.
KIND="${KIND:-smoke}"
# Free-text experiment tag (e.g. E1-shards24), carried into the row.
EXP="${EXP:-adhoc}"
# Trial number, so repeats of one configuration are distinct rows.
TRIAL="${TRIAL:-1}"
GEN_SATURATED_CORES="${GEN_SATURATED_CORES:-1.6}"
MIN_SAMPLES="${MIN_SAMPLES:-150}"
# A resource at or above this fraction of its capacity is named as a candidate
# bottleneck in the row (the saturation rule in KUBERNETES-TEST-PLAN.md).
SAT_PCT="${SAT_PCT:-85}"
SAMPLE_EVERY="${SAMPLE_EVERY:-3}"
# Relative to engine/ (the cd above), so ../docs is the REPOSITORY docs/ --
# the tree collect_eks_evidence.sh bundles and the runbook points at. A plain
# docs/ here wrote to engine/docs/, where no collector ever looked.
OUT="${OUT:-../docs/results/cloud-campaign}"; mkdir -p "$OUT/raw"
CSV="$OUT/results.csv"

# --- deployed configuration, READ from the cluster, never assumed ----------
DEPLOYED_SHARDS=$(kubectl -n "$NS" get configmap durexec-config -o jsonpath='{.data.SHARD_COUNT}')
RING=$(kubectl -n "$NS" get configmap durexec-config -o jsonpath='{.data.CLUSTER_NODES}')
WF_WORKERS=$(kubectl -n "$NS" get configmap durexec-config -o jsonpath='{.data.WORKFLOW_WORKERS}')
ACT_WORKERS=$(kubectl -n "$NS" get configmap durexec-config -o jsonpath='{.data.ACTIVITY_WORKERS}')
SYNC_COMMIT=$(kubectl -n "$NS" get configmap durexec-config -o jsonpath='{.data.SYNC_COMMIT}')
PODS=$(engine_pods)
[ -n "$PODS" ] || { echo "FATAL: no engine pods in $NS" >&2; exit 1; }
REPLICAS=$(wc -w <<<"$PODS" | tr -d ' ')
# SHARDS is how many of the DEPLOYED shards loadgen drives. Default: all of
# them. A value below DEPLOYED_SHARDS reproduces the local sweep's subset
# semantics and is labelled as such in config_id.
SHARDS="${SHARDS:-$DEPLOYED_SHARDS}"
if [ "$MODE" = arrival ]; then LOAD="rate${RATE}"; else LOAD="c${CONC}"; fi
CFG="r${REPLICAS}-S${DEPLOYED_SHARDS}-drive${SHARDS}-w${WF_WORKERS}x${ACT_WORKERS}-sc${SYNC_COMMIT}-${MODE}-${LOAD}-d${DUR}-t${TRIAL}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
JOB="loadgen-${RUN_ID,,}"; JOB="${JOB//[^a-z0-9-]/}"
RAW="$OUT/raw/${RUN_ID}-${CFG}"; mkdir -p "$RAW"

HEADER="run_id,exp,config_id,kind,mode,offered_rate,service_floor,lg_verdict,dropped,gen_lag_p99_s,inflight_slope,trial,ts_start,ts_end,git_sha,git_tree,image_tag,replicas,deployed_shards,driven_shards,wf_workers,act_workers,sync_commit,concurrency,duration,warmup,started,completed,wf_per_s,p50_s,p95_s,p99_s,failed,non_completed,notowner_follows,owner_nodes,shards_per_owner,shard_skew,gen_node,gen_role,gen_cpu_cores,gen_cpu_peak,gen_mem_peak_mb,gen_restarts,gen_exit,job_status,eng_cpu_cores_total,eng_cpu_cores_max_pod,eng_node_cpu_pct_max,eng_mem_peak_mb_max,db_rtt_ms_p50,db_rtt_ms_max,pg_commit_per_s,pg_rollback,pg_wal_MBps,pg_hit_ratio,pg_stmt_mean_ms,pg_deadlocks,pg_peak_conns,pg_peak_active,pg_peak_lock_waits,pg_ckpt_req,pg_wait_profile,eng_pool_acquires,eng_pool_empty_frac,eng_pool_wait_s,eng_task_failures,eng_fencing_rejections,eng_lease_changes,eng_replays,eng_gc_pause_mean_ms,eng_goroutines_max,eng_queue_oldest_ready_s,e2e_db_p50_ms,e2e_db_p99_ms,first_decision_p50_ms,act_p50_ms,act_p99_ms,decision_p50_ms,decision_p99_ms,timer_lag_p50_ms,timer_lag_p99_ms,inv_runs,inv_dup_run_seq,inv_dup_activity_completion,inv_orphan_completion,inv_seq_gaps,inv_multi_terminal,inv_pending_tasks,inv_exec_status,dbhost_cpu_pct,dbhost_iowait_pct,dbhost_steal_pct,dbhost_disk_write_iops,dbhost_disk_util_pct,saturated,missing,verdict,raw_dir"
if [ -f "$CSV" ]; then
  # Refuse to append rows of a different shape; a mixed-schema file is not
  # analysable and would be silently misread.
  grep -v '^#' "$CSV" | head -1 | grep -qxF "$HEADER" \
    || { echo "FATAL: $CSV has a different header; use a new OUT" >&2; exit 1; }
else
  { echo "# durexec cloud campaign. One row per load point; raw samples under raw/<run_id>-<config_id>/."
    echo "# Missing measurements are empty and listed in 'missing'; they are never estimated."
    echo "$HEADER"; } > "$CSV"
fi
grep -q ",${CFG},${KIND},${MODE}," "$CSV" && { echo "$CFG ($KIND) already recorded, skipping"; exit 0; }

ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
IMAGE="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com/durexec/loadgen:${TAG}"

# loadgen hashes workflow ids onto -shard-count shards; it defaults to 16, so
# every non-16 deployment either failed ("-shards 32 exceeds -shard-count 16",
# campaign 2 Stage G) or, below 16, would silently aim at shards that do not
# exist. It is always the DEPLOYED count, read from the cluster.
# Per-pod headless DNS: loadgen follows 409 NotOwner redirects to the named
# owner, and a round-robin Service would send the retry to an arbitrary pod.
BASES=""; for p in $PODS; do BASES="${BASES},http://${p}.durexec-hl.${NS}.svc.cluster.local:8080"; done
BASES="${BASES#,}"
# A per-run id prefix, so the journal rows of THIS point can be selected for the
# latency decomposition and the invariants (sql/run_latency_and_invariants.sql).
IDP="lg-${RUN_ID,,}"
if [ "$MODE" = arrival ]; then
  ARGS=$(printf '["-mode","arrival","-rates","%s","-service-floor","%s","-shard-count","%s","-shards","%s","-duration","%s","-warmup","%s","-id-prefix","%s","-base","%s"]' \
          "$RATE" "$FLOOR" "$DEPLOYED_SHARDS" "$SHARDS" "$DUR" "$WARMUP" "$IDP" "$BASES")
else
  ARGS=$(printf '["-mode","scaling","-shard-count","%s","-sweep","%s","-concurrency","%s","-duration","%s","-warmup","%s","-id-prefix","%s","-base","%s"]' \
          "$DEPLOYED_SHARDS" "$SHARDS" "$CONC" "$DUR" "$WARMUP" "$IDP" "$BASES")
fi

# --- provenance --------------------------------------------------------------
{
  echo "run_id=$RUN_ID"; echo "exp=$EXP"; echo "config_id=$CFG"; echo "kind=$KIND"
  echo "git_sha=$(git rev-parse HEAD)"; echo "git_tree=$(git diff --quiet && echo clean || echo dirty)"
  echo "image_tag=$TAG"; echo "loadgen_args=$ARGS"; echo "ring=$RING"
  kubectl -n "$NS" get pods -o wide 2>&1
  kubectl get nodes -L durexec.io/role,node.kubernetes.io/instance-type,topology.kubernetes.io/zone 2>&1
  echo "node_inventory: name role EC2_instance_type availability_zone"
  kubectl get nodes -o json | python3 scripts/aws/node_inventory.py
} > "$RAW/00-provenance.txt"
kubectl -n "$NS" get configmap durexec-config -o yaml > "$RAW/01-configmap.yaml" 2>&1 || true
engine_get "${PODS%% *}" /v1/shards > "$RAW/02-shards-before.json" 2>&1 || true
for p in $PODS; do engine_get "$p" /metrics > "$RAW/39-metrics-before-$p.txt" 2>&1 || true; done

# --- database baseline and network RTT ----------------------------------------
probe_ensure
probe_rtt 20 > "$RAW/10-db-rtt-ms.txt" 2>"$RAW/10-db-rtt.err" || true
probe_sql < scripts/aws/sql/pgstat_snapshot.sql > "$RAW/11-pgstat-before.txt" 2>&1 || true

# --- launch ------------------------------------------------------------------
TS_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
sed -e "s|LOADGEN_JOB_NAME|$JOB|" -e "s|LOADGEN_IMAGE|$IMAGE|" -e "s|LOADGEN_ARGS|$ARGS|" \
    deploy/kubernetes/loadgen/job.yaml | kubectl apply -f - >/dev/null

# --- sample DURING the window --------------------------------------------------
# Every node carrying an engine pod, the harness node, and therefore the
# generator. Summaries are cumulative, so a missed sample costs resolution,
# not correctness.
NODES=$(kubectl get nodes -l 'durexec.io/role in (engine,harness)' -o jsonpath='{.items[*].metadata.name}')
sampler() {
  while :; do
    for n in $NODES; do
      s=$(node_summary "$n" 2>/dev/null) && printf '{"node":"%s","summary":%s}\n' "$n" "$(tr -d '\n' <<<"$s")"
    done >> "$RAW/20-kubelet-samples.jsonl"
    sleep "$SAMPLE_EVERY"
  done
}
pgsampler() {
  while :; do
    probe_sql < scripts/aws/sql/pgstat_snapshot.sql >> "$RAW/12-pgstat-during.txt" 2>/dev/null || true
    sleep 5
  done
}
sampler & SP=$!
pgsampler & PP=$!
cleanup() { kill "$SP" "$PP" 2>/dev/null || true; wait "$SP" "$PP" 2>/dev/null || true; }
trap cleanup EXIT

kubectl -n "$NS" wait --for=condition=complete --timeout=900s "job/$JOB" >/dev/null 2>&1 \
  || kubectl -n "$NS" wait --for=condition=failed --timeout=10s "job/$JOB" >/dev/null 2>&1 || true
cleanup; trap - EXIT
TS_END="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

probe_sql < scripts/aws/sql/pgstat_snapshot.sql > "$RAW/13-pgstat-after.txt" 2>&1 || true
# Journal-derived latency phases and invariants for this run's workflows only.
{ printf "\\set prefix '%s'\n" "$IDP"; cat scripts/aws/sql/run_latency_and_invariants.sql; } \
  | probe_sql > "$RAW/14-journal-latency-invariants.txt" 2>&1 || true
kubectl -n "$NS" logs "job/$JOB" > "$RAW/30-loadgen.txt" 2>&1 || true
engine_get "${PODS%% *}" /v1/shards > "$RAW/03-shards-after.json" 2>&1 || true
for p in $PODS; do engine_get "$p" /metrics > "$RAW/40-metrics-$p.txt" 2>&1 || true; done
kubectl -n "$NS" get events --sort-by=.lastTimestamp > "$RAW/50-events.txt" 2>&1 || true
kubectl -n "$NS" get pods -l job-name="$JOB" -o json > "$RAW/31-loadgen-pod.json" 2>&1 || true

# --- generator identity and placement ------------------------------------------
jp() { kubectl -n "$NS" get pods -l job-name="$JOB" -o jsonpath="$1" 2>/dev/null || true; }
GEN_POD=$(jp '{.items[0].metadata.name}')
GEN_NODE=$(jp '{.items[0].spec.nodeName}')
GEN_ROLE=$(kubectl get node "$GEN_NODE" -o jsonpath='{.metadata.labels.durexec\.io/role}' 2>/dev/null || true)
GEN_RESTARTS=$(jp '{.items[0].status.containerStatuses[0].restartCount}')
GEN_EXIT=$(jp '{.items[0].status.containerStatuses[0].state.terminated.exitCode}')
GEN_T0=$(jp '{.items[0].status.containerStatuses[0].state.terminated.startedAt}')
GEN_T1=$(jp '{.items[0].status.containerStatuses[0].state.terminated.finishedAt}')
JOB_STATUS=$(kubectl -n "$NS" get job "$JOB" -o jsonpath='{.status.conditions[0].type}' 2>/dev/null || true)

# --- reduce ------------------------------------------------------------------
python3 scripts/aws/pgstat_delta.py "$RAW/11-pgstat-before.txt" "$RAW/13-pgstat-after.txt" \
  "$RAW/12-pgstat-during.txt" > "$RAW/60-pg-reduced.txt" 2>&1 || true
# Engine and node CPU over the generator's own lifetime when known, so the
# figure covers the load and not the probes around it.
WIN=""
if [ -n "$GEN_T0" ] && [ -n "$GEN_T1" ]; then
  WIN="$(date -u -d "$GEN_T0" +%s) $(date -u -d "$GEN_T1" +%s)"
fi
MPAIRS=(); for p in $PODS; do MPAIRS+=("$RAW/39-metrics-before-$p.txt" "$RAW/40-metrics-$p.txt"); done
python3 scripts/aws/metrics_delta.py "${MPAIRS[@]}" > "$RAW/63-metrics-reduced.txt" 2>&1 || true
# shellcheck disable=SC2086
python3 scripts/aws/k8s_usage.py "$RAW/20-kubelet-samples.jsonl" $WIN > "$RAW/61-k8s-reduced.txt" 2>&1 || true
# The generator ran only inside that window; reduce it over everything seen.
python3 scripts/aws/k8s_usage.py "$RAW/20-kubelet-samples.jsonl" > "$RAW/62-k8s-reduced-all.txt" 2>&1 || true

pg()  { sed -n "s/^$1=//p" "$RAW/60-pg-reduced.txt" | head -1 | sed 's/^unavailable$//'; }
md()  { sed -n "s/^$1=//p" "$RAW/63-metrics-reduced.txt" | head -1 | sed -E 's/^(unavailable)$//'; }
# Ownership CHANGES during the window (acquired/lost/expired), not renewals.
LEASE_CHANGES=$({ grep '^d_lease_transitions_total{' "$RAW/63-metrics-reduced.txt" 2>/dev/null || true; } \
  | { grep -v 'direction="renewed"' || true; } | awk -F= '{s+=$NF} END{print s+0}')
k8s() { { grep -F "$1" "$RAW/$2" || true; } | sed -n "s/.*|$3=//p" | head -1 | sed 's/^unavailable$//'; }

GEN_CPU=$(k8s "pod|${NS}/${GEN_POD}|" 62-k8s-reduced-all.txt cpu_cores)
GEN_CPU_PEAK=$(k8s "pod|${NS}/${GEN_POD}|" 62-k8s-reduced-all.txt cpu_cores_peak)
GEN_MEM=$(k8s "pod|${NS}/${GEN_POD}|" 62-k8s-reduced-all.txt mem_peak_bytes)
ENG_TOTAL=0; ENG_MAXPOD=""; NODE_MAX=""; ENG_MEM=""
for p in $PODS; do
  c=$(k8s "pod|${NS}/${p}|" 61-k8s-reduced.txt cpu_cores)
  m=$(k8s "pod|${NS}/${p}|" 61-k8s-reduced.txt mem_peak_bytes)
  n=$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
  nc=$(k8s "node|${n}|" 61-k8s-reduced.txt cpu_cores)
  alloc=$(kubectl get node "$n" -o jsonpath='{.status.allocatable.cpu}' 2>/dev/null | awk '/m$/{sub(/m$/,"");print $1/1000;next}{print}')
  [ -n "$c" ] && ENG_TOTAL=$(awk -v a="$ENG_TOTAL" -v b="$c" 'BEGIN{printf "%.3f",a+b}') \
              && ENG_MAXPOD=$(awk -v a="${ENG_MAXPOD:-0}" -v b="$c" 'BEGIN{printf "%.3f",(b>a)?b:a}')
  [ -n "$m" ] && ENG_MEM=$(awk -v a="${ENG_MEM:-0}" -v b="$m" 'BEGIN{printf "%.0f",(b/1048576>a)?b/1048576:a}')
  if [ -n "$nc" ] && [ -n "$alloc" ]; then
    NODE_MAX=$(awk -v a="${NODE_MAX:-0}" -v b="$nc" -v c="$alloc" 'BEGIN{v=100*b/c; printf "%.1f",(v>a)?v:a}')
  fi
done
[ -n "$ENG_MAXPOD" ] || ENG_TOTAL=""
GEN_MEM_MB=$([ -n "$GEN_MEM" ] && awk -v b="$GEN_MEM" 'BEGIN{printf "%.0f",b/1048576}' || true)
RTT_P50=$(sort -n "$RAW/10-db-rtt-ms.txt" 2>/dev/null | awk '{a[NR]=$1} END{if(NR) printf "%.3f", a[int((NR+1)/2)]}')
RTT_MAX=$(sort -n "$RAW/10-db-rtt-ms.txt" 2>/dev/null | tail -1)

J="$RAW/14-journal-latency-invariants.txt"
jl() { { grep "^lat|$1|" "$J" 2>/dev/null || true; } | tr '|' '\n' | sed -n "s/^$2=//p" | head -1; }
ji() { sed -n "s/^inv|$1=//p" "$J" 2>/dev/null | head -1; }
INV_BAD=""
for k in dup_run_seq dup_activity_completion orphan_completion seq_gaps multi_terminal; do
  v=$(ji "$k"); [ -n "$v" ] && [ "$v" != 0 ] && INV_BAD="${INV_BAD}+${k}=${v}"
done
INV_BAD="${INV_BAD#+}"
# Zero matching runs makes every invariant trivially 0 -- green and wrong.
# The journal must hold at least as many runs as loadgen counted completed.
[ -n "$(ji runs)" ] || missing_journal=1

RAWFILE="$RAW/30-loadgen.txt"
lat() { awk -v k="latency_p$1 @" 'index($0,k) && !s { for(i=1;i<=NF;i++) if($i ~ /^[0-9]+\.?[0-9]*$/){print $i;s=1;exit} }' "$RAWFILE"; }
LG_VERDICT=""; DROPPED=""; LAG=""; SLOPE=""
if [ "$MODE" = arrival ]; then
  acc=$(grep -E '^ +scheduled [0-9]+, issued' "$RAWFILE" | tail -1 || true)
  cmp=$(grep -E '^ +completed [0-9.]+ wf/s \(offered' "$RAWFILE" | tail -1 || true)
  STARTED=$(sed -n 's/.*issued \([0-9]*\),.*/\1/p' <<<"$acc")
  DROPPED=$(sed -n 's/.*dropped \([0-9]*\),.*/\1/p' <<<"$acc")
  COMPLETED=$(sed -n 's/.*accepted [0-9]*, completed \([0-9]*\),.*/\1/p' <<<"$acc")
  FAILED=$(sed -n 's/.*errored \([0-9]*\).*/\1/p' <<<"$acc")
  NONCOMP=$(sed -n 's/.*timed out \([0-9]*\),.*/\1/p' <<<"$acc")
  FOLLOWS=""
  WFPS=$(sed -n 's/^ *completed \([0-9.]*\) wf\/s.*/\1/p' <<<"$cmp")
  SLOPE=$(sed -n 's/.*slope \([-+0-9.]*\)\/s.*/\1/p' <<<"$cmp")
  LAG=$(awk 'index($0,"generator_lag_p99 @") && !s {for(i=1;i<=NF;i++) if($i ~ /^[0-9]+\.?[0-9]*$/){print $i;s=1;exit}}' "$RAWFILE")
  LG_VERDICT=$(sed -n 's/^ *VERDICT \([A-Z_]*\):.*/\1/p' "$RAWFILE" | tail -1)
else
  line=$(grep -E 'completed [0-9]+ in .* wf/s' "$RAWFILE" | tail -1 || true)
  COMPLETED=$(sed -n 's/.*completed \([0-9]*\) in.*/\1/p' <<<"$line")
  WFPS=$(sed -n 's/.*= \([0-9.]*\) wf\/s.*/\1/p' <<<"$line")
  STARTED=$(sed -n 's/.*(started \([0-9]*\),.*/\1/p' <<<"$line")
  FAILED=$(sed -n 's/.*failed \([0-9]*\),.*/\1/p' <<<"$line")
  NONCOMP=$(sed -n 's/.*non-COMPLETED \([0-9]*\),.*/\1/p' <<<"$line")
  FOLLOWS=$(sed -n 's/.*409 follows \([0-9]*\)).*/\1/p' <<<"$line")
fi
OWNERS=$(sed -n 's/.*owned by \[\([^]]*\)\].*/\1/p' "$RAWFILE" | tail -1 | wc -w | tr -d ' ')
# Shards PER owner, not just how many owners. The ring is a consistent hash,
# so ownership is uneven even when the shard count divides by the replica
# count (r3 with S=24 gives 8/11/5, S=16 gives 5/9/2). The busiest engine
# bounds a closed-loop run, so the skew is a covariate of every scaling point.
read -r PER_OWNER SKEW < <(python3 - "$RAW/02-shards-before.json" "$REPLICAS" <<'PY' 2>/dev/null || echo " "
import json, sys
s = json.load(open(sys.argv[1]))["shards"]; reps = int(sys.argv[2])
own = {}
for x in s:
    if x.get("owner"):
        own[x["owner"].split(".")[0]] = own.get(x["owner"].split(".")[0], 0) + 1
print(";".join(f"{k}:{own[k]}" for k in sorted(own)) or "-", f"{max(own.values()) / (len(s) / reps):.2f}" if own else "-")
PY
)

# --- attribution: which resources were at or above SAT_PCT ---------------------
DBCPU=$(pg dbhost_cpu_busy_pct); DBUTIL=$(pg dbhost_disk_util_pct)
sat=""
ge() { [ -n "$1" ] && awk -v v="$1" -v t="$2" 'BEGIN{exit !(v>=t)}'; }
ge "$NODE_MAX" "$SAT_PCT" && sat="${sat}+engine_node_cpu"
ge "$DBCPU" "$SAT_PCT" && sat="${sat}+db_cpu"
ge "$DBUTIL" "$SAT_PCT" && sat="${sat}+db_disk"
ge "$GEN_CPU" "$GEN_SATURATED_CORES" && sat="${sat}+generator"
sat="${sat#+}"
missing=""
for v in GEN_CPU ENG_TOTAL NODE_MAX DBCPU RTT_P50; do [ -n "${!v}" ] || missing="${missing}+${v,,}"; done
[ -n "$(pg pg_xact_commit_per_s)" ] || missing="${missing}+pg_stat"
[ -n "$(md d_db_pool_acquires_total)" ] || missing="${missing}+engine_metrics"
[ -z "${missing_journal:-}" ] || missing="${missing}+journal_invariants"
if [ -n "$(ji runs)" ] && [ -n "${COMPLETED:-}" ] && [ "$(ji runs)" -lt "$COMPLETED" ]; then
  INV_BAD="${INV_BAD:+$INV_BAD+}journal_runs_$(ji runs)_lt_completed_${COMPLETED}"
fi
missing="${missing#+}"

# Ordered so the most damning problem wins.
if [ "$JOB_STATUS" = "Failed" ]; then VERDICT="INVALID_job_failed"
elif [ -n "${GEN_EXIT:-}" ] && [ "${GEN_EXIT:-0}" != "0" ]; then VERDICT="INVALID_generator_exit_${GEN_EXIT}"
elif [ "${GEN_RESTARTS:-0}" != "0" ]; then VERDICT="INVALID_generator_restarted_${GEN_RESTARTS}x"
elif [ "$GEN_ROLE" != "harness" ]; then VERDICT="INVALID_generator_on_${GEN_ROLE:-unknown}_node"
elif [ ! -s "$RAWFILE" ]; then VERDICT="INVALID_no_output"
elif [ -z "$WFPS" ]; then VERDICT="UNPARSEABLE"
elif [ -n "$INV_BAD" ]; then VERDICT="CORRECTNESS_VIOLATION_${INV_BAD//+/_}"
elif [ "${FAILED:-0}" != "0" ] || [ "${NONCOMP:-0}" != "0" ]; then VERDICT="CORRECTNESS_REVIEW_failed${FAILED:-?}_noncompleted${NONCOMP:-?}"
elif [ -z "$GEN_CPU" ]; then VERDICT="INCONCLUSIVE_generator_cpu_unmeasured"
elif [ "$LG_VERDICT" = GENERATOR_BOUND ]; then VERDICT="GENERATOR_SATURATED_schedule_lag"
elif ge "$GEN_CPU" "$GEN_SATURATED_CORES"; then VERDICT="GENERATOR_SATURATED_${GEN_CPU}_cores"
elif [ -z "${COMPLETED:-}" ] || [ "${COMPLETED:-0}" -lt "$MIN_SAMPLES" ]; then VERDICT="INCONCLUSIVE_only_${COMPLETED:-0}_samples"
elif [ -n "${missing_journal:-}" ]; then VERDICT="INCONCLUSIVE_no_correctness_evidence"
elif [ "$KIND" = smoke ]; then VERDICT="SMOKE_ok"
elif [ -n "$missing" ]; then VERDICT="RECORDED_PARTIAL"
# loadgen's gate says SATURATED but no measured resource is near capacity:
# either the declared FLOOR is too low for this deployment (it then calls an
# idle engine saturated), or the limit is serialization (the per-shard lease
# row lock) rather than a resource. Both need a follow-up before this point
# may be called a knee.
elif [ "$LG_VERDICT" = SATURATED ] && [ -z "$sat" ]; then VERDICT="SATURATED_UNATTRIBUTED"
else VERDICT="RECORDED"
fi

row=("$RUN_ID" "$EXP" "$CFG" "$KIND" "$MODE" "$RATE" "$FLOOR" "$LG_VERDICT" "$DROPPED" "$LAG" "$SLOPE" "$TRIAL" "$TS_START" "$TS_END" "$(git rev-parse --short HEAD)"
  "$(git diff --quiet && echo clean || echo dirty)" "$TAG" "$REPLICAS" "$DEPLOYED_SHARDS" "$SHARDS"
  "$WF_WORKERS" "$ACT_WORKERS" "$SYNC_COMMIT" "$CONC" "$DUR" "$WARMUP"
  "${STARTED:-}" "${COMPLETED:-}" "${WFPS:-}" "$(lat 50)" "$(lat 95)" "$(lat 99)"
  "${FAILED:-}" "${NONCOMP:-}" "${FOLLOWS:-}" "${OWNERS:-}" "${PER_OWNER:-}" "${SKEW:-}"
  "${GEN_NODE:-}" "${GEN_ROLE:-}" "${GEN_CPU:-}" "${GEN_CPU_PEAK:-}" "${GEN_MEM_MB:-}" "${GEN_RESTARTS:-}" "${GEN_EXIT:-}" "${JOB_STATUS:-}"
  "${ENG_TOTAL:-}" "${ENG_MAXPOD:-}" "${NODE_MAX:-}" "${ENG_MEM:-}" "${RTT_P50:-}" "${RTT_MAX:-}"
  "$(pg pg_xact_commit_per_s)" "$(pg pg_d_xact_rollback)"
  "$(awk -v b="$(pg pg_wal_bytes_per_s)" 'BEGIN{if(b!="")printf "%.3f",b/1e6}')"
  "$(pg pg_buffer_hit_ratio)" "$(pg pg_stmt_mean_ms)" "$(pg pg_d_deadlocks)"
  "$(pg pg_peak_conns_total)" "$(pg pg_peak_conns_active)" "$(pg pg_peak_conns_waiting_lock)" "$(pg pg_d_ckpt_req)"
  "$(pg pg_wait_profile)"
  "$(md d_db_pool_acquires_total)" "$(md pool_empty_fraction)" "$(md d_db_pool_acquire_wait_seconds_total)"
  "$(md d_worker_task_failures_total)" "$(md d_fencing_rejections_total)" "${LEASE_CHANGES:-}"
  "$(md d_replay_duration_seconds_count)" "$(md gc_pause_mean_ms)" "$(md max_go_goroutines)"
  "$(md max_task_queue_oldest_ready_seconds)"
  "$(jl end_to_end p50)" "$(jl end_to_end p99)" "$(jl start_to_first_decision p50)"
  "$(jl activity_sched_to_done p50)" "$(jl activity_sched_to_done p99)"
  "$(jl decision_turnaround p50)" "$(jl decision_turnaround p99)"
  "$(jl timer_lag p50)" "$(jl timer_lag p99)"
  "$(ji runs)" "$(ji dup_run_seq)" "$(ji dup_activity_completion)" "$(ji orphan_completion)"
  "$(ji seq_gaps)" "$(ji multi_terminal)" "$(ji pending_tasks)" "$(ji executions_by_status)"
  "$DBCPU" "$(pg dbhost_cpu_iowait_pct)" "$(pg dbhost_cpu_steal_pct)"
  "$(pg dbhost_disk_write_iops)" "$DBUTIL" "$sat" "$missing" "$VERDICT"
  "${RAW#"$OUT"/}")
( IFS=,; echo "${row[*]}" ) >> "$CSV"
sync "$CSV" 2>/dev/null || true

kubectl -n "$NS" delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true
echo "$CFG -> ${WFPS:-?} wf/s lg=${LG_VERDICT:--} p99=$(lat 99)s gen=${GEN_CPU:-?}c eng=${ENG_TOTAL:-?}c node_max=${NODE_MAX:-?}% db_cpu=${DBCPU:-?}% sat=[${sat}] missing=[${missing}] verdict=$VERDICT"
case "$VERDICT" in INVALID*|UNPARSEABLE) exit 1 ;; esac
