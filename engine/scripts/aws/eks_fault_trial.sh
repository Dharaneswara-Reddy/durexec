#!/usr/bin/env bash
# One fault-injection trial UNDER LOAD, with the Phase 9 recovery oracle and
# the journal invariants, appended as one CSV row.
#
#   SCENARIO=<name> TRIAL=<n> [VICTIM=durexec-N] FLOOR=<s> LOAD_RATE=<wf/s> \
#     DUREXEC_CONFIRM=... ./scripts/aws/eks_fault_trial.sh
#
# Scenarios: delete-graceful delete-force kill freeze netem partition sever-db
#            drain terminate-node reboot-db
#
# Recovery instants, all on the PostgreSQL clock and in ms from t_inject (the
# same definitions as experiments/scripts/fault.sh, so cloud and lab compare):
#   t_expire   every lease the victim held has passed expires_at, or moved
#   t_claim    every one of those shards is held, unexpired, with a HIGHER token
#   t_progress the first event committed on one of those shards after t_inject
#   t_ring     every engine pod Ready again and no shard unowned
#
# Phase 9 lesson, enforced here: killing the SAME pod repeatedly measured
# Kubernetes' exponential restart back-off, not lease recovery. The default
# victim rotates with TRIAL, the trial refuses to start while any pod is in
# CrashLoopBackOff or restarted recently, and a back-off seen during the trial
# marks it CONTAMINATED rather than slow.
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/aws/_common.sh
enable_evidence_tee
require_confirmation "run a fault trial against the EKS pilot"
NS="${NS:-durexec-lab}"
. scripts/aws/_k8s.sh
FAULT=scripts/aws/eks_fault.sh

SCENARIO="${SCENARIO:?set SCENARIO}"; TRIAL="${TRIAL:-1}"
FLOOR="${FLOOR:?set FLOOR (the calibrated service floor, e.g. 1.6s)}"
LOAD_RATE="${LOAD_RATE:?set LOAD_RATE (about 0.6 x the measured knee)}"
LOAD_DUR="${LOAD_DUR:-180s}"; PRE_S="${PRE_S:-30}"; MAX_WAIT_S="${MAX_WAIT_S:-150}"
FREEZE_S="${FREEZE_S:-35}"; NETEM="${NETEM:-delay 200ms 50ms loss 20%}"; HOLD_S="${HOLD_S:-30}"
COOL_S="${COOL_S:-120}"
TAG="${TAG:-$(git rev-parse --short HEAD)}"; REGION="${AWS_REGION:-us-east-1}"
# Relative to engine/ (the cd above), so ../docs is the REPOSITORY docs/ --
# the tree collect_eks_evidence.sh bundles and the runbook points at. A plain
# docs/ here wrote to engine/docs/, where no collector ever looked.
OUT="${OUT:-../docs/results/cloud-campaign}"; mkdir -p "$OUT/raw"
CSV="$OUT/faults.csv"
PODS=$(engine_pods); REPLICAS=$(wc -w <<<"$PODS" | tr -d ' ')
VICTIM="${VICTIM:-durexec-$(( TRIAL % REPLICAS ))}"
[ "$SCENARIO" = reboot-db ] && VICTIM="database"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"; RAW="$OUT/raw/${RUN_ID}-fault-${SCENARIO}-t${TRIAL}"; mkdir -p "$RAW"
IDP="lg-fault-${RUN_ID,,}"; JOB="loadgen-fault-${RUN_ID,,}"; JOB="${JOB//[^a-z0-9-]/}"

HEADER="run_id,scenario,trial,victim,victim_node,replicas,shards_held,tok_before,tok_after,t_expire_ms,t_claim_ms,t_progress_ms,t_ring_ms,new_owners,restarts_before,restarts_after,crashloop_seen,policy_enforced,stale_fencing_rejections,load_rate,lg_completed,lg_errored,lg_timedout,lg_p99_s,inv_runs,inv_dup_run_seq,inv_dup_activity_completion,inv_seq_gaps,inv_multi_terminal,inv_exec_status,drained,verdict,raw_dir"
[ -f "$CSV" ] || { echo "# durexec cloud fault trials; t_* in ms from t_inject on the PostgreSQL clock; NA = not reached within MAX_WAIT_S"; echo "$HEADER"; } > "$CSV"

q()     { echo "$1" | probe_sql; }
dbnow() { q "select (extract(epoch from clock_timestamp())*1000)::bigint;"; }
restarts() { kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{range .items[*]}{.metadata.name}={.status.containerStatuses[0].restartCount}{" "}{end}'; }
crashloop() { kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{range .items[*]}{.status.containerStatuses[0].state.waiting.reason}{" "}{end}' | grep -c CrashLoopBackOff || true; }
ring_ok() {
  local ready unowned
  ready=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True || true)
  unowned=$(q "select count(*) from shard_leases where owner_node is null or expires_at < clock_timestamp();")
  [ "$ready" = "$REPLICAS" ] && [ "$unowned" = 0 ]
}

# --- preconditions -----------------------------------------------------------
probe_ensure
log() { echo "$*" | tee -a "$RAW/00-timeline.txt"; }
log "run_id=$RUN_ID scenario=$SCENARIO trial=$TRIAL victim=$VICTIM git=$(git rev-parse HEAD) image=$TAG"
[ "$(crashloop)" = 0 ] || { log "precondition: a pod is in CrashLoopBackOff"; VERDICT_PRE="INVALID_crashloop_before_start"; }
# The most recent container start must be at least COOL_S ago, so a trial
# never begins inside the previous trial's restart back-off window.
last_start=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{range .items[*]}{.status.containerStatuses[0].state.running.startedAt}{"\n"}{end}' \
  | xargs -r -n1 date -u +%s -d 2>/dev/null | sort -n | tail -1)
if [ -n "$last_start" ] && [ $(( $(date +%s) - last_start )) -lt "$COOL_S" ]; then
  w=$(( COOL_S - ($(date +%s) - last_start) )); log "cooling ${w}s after the last container start"; sleep "$w"
fi
ring_ok || VERDICT_PRE="${VERDICT_PRE:-INVALID_ring_not_healthy_before_start}"
RESTARTS_BEFORE=$(restarts)
POLICY_ENFORCED=""
if [ "$SCENARIO" = partition ]; then
  POLICY_ENFORCED=$("$FAULT" verify-policy 2>&1 | sed -n 's/^networkpolicy_enforced=\([a-z]*\).*/\1/p')
  [ "$POLICY_ENFORCED" = yes ] || VERDICT_PRE="${VERDICT_PRE:-NOT_TESTABLE_networkpolicy_not_enforced}"
fi
if [ -n "${VERDICT_PRE:-}" ]; then
  log "verdict=$VERDICT_PRE (not started)"
  ( IFS=,; echo "$RUN_ID,$SCENARIO,$TRIAL,$VICTIM,,$REPLICAS,,,,,,,,,${RESTARTS_BEFORE// /;},,,${POLICY_ENFORCED},,$LOAD_RATE,,,,,,,,,,,,${VERDICT_PRE},${RAW#"$OUT"/}" ) >> "$CSV"
  exit 1
fi

# --- background load ------------------------------------------------------------
ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
IMAGE="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com/durexec/loadgen:${TAG}"
DEPLOYED_SHARDS=$(kubectl -n "$NS" get configmap durexec-config -o jsonpath='{.data.SHARD_COUNT}')
BASES=""; for p in $PODS; do BASES="${BASES},http://${p}.durexec-hl.${NS}.svc.cluster.local:8080"; done
ARGS=$(printf '["-mode","arrival","-rates","%s","-service-floor","%s","-shards","%s","-duration","%s","-warmup","5s","-grace","120s","-id-prefix","%s","-base","%s"]' \
        "$LOAD_RATE" "$FLOOR" "$DEPLOYED_SHARDS" "$LOAD_DUR" "$IDP" "${BASES#,}")
sed -e "s|LOADGEN_JOB_NAME|$JOB|" -e "s|LOADGEN_IMAGE|$IMAGE|" -e "s|LOADGEN_ARGS|$ARGS|" \
    deploy/kubernetes/loadgen/job.yaml | kubectl apply -f - >/dev/null
log "load: $LOAD_RATE wf/s for $LOAD_DUR (job $JOB)"
sleep "$PRE_S"

# --- snapshot what the victim holds, then inject ----------------------------------
VNODE=""; [ "$VICTIM" != database ] && VNODE=$(kubectl -n "$NS" get pod "$VICTIM" -o jsonpath='{.spec.nodeName}')
if [ "$VICTIM" = database ]; then
  SHARDS=$(q "select string_agg(shard_id::text, ',' order by shard_id) from shard_leases;")
else
  SHARDS=$(q "select string_agg(shard_id::text, ',' order by shard_id) from shard_leases where owner_node like '$VICTIM.%';")
fi
[ -n "$SHARDS" ] || { log "victim $VICTIM holds no shards"; SHARDS="-1"; }
HELD=$(tr ',' '\n' <<<"$SHARDS" | grep -c . || true)
TOK_BEFORE=$(q "select string_agg(fencing_token::text, ';' order by shard_id) from shard_leases where shard_id in ($SHARDS);")
q "select shard_id||'|'||coalesce(owner_node,'-')||'|'||fencing_token||'|'||expires_at from shard_leases order by shard_id;" > "$RAW/10-leases-before.txt"
[ "$VICTIM" != database ] && engine_get "$VICTIM" /metrics > "$RAW/11-victim-metrics-before.txt" 2>/dev/null || true
T0=$(dbnow); log "t_inject=$T0 shards=$SHARDS tokens=$TOK_BEFORE"

case "$SCENARIO" in
  delete-graceful) "$FAULT" delete "$VICTIM" 30 ;;
  delete-force)    "$FAULT" delete "$VICTIM" 0 ;;
  kill)            "$FAULT" kill "$VICTIM" ;;
  freeze)          "$FAULT" freeze "$VICTIM" ;;
  netem)           # shellcheck disable=SC2086
                   "$FAULT" netem "$VICTIM" $NETEM ;;
  partition)       "$FAULT" partition "$VICTIM" ;;
  sever-db)        "$FAULT" sever-db "$VICTIM" ;;
  drain)           "$FAULT" drain "$VICTIM" ;;
  terminate-node)  "$FAULT" terminate-node "$VICTIM" ;;
  reboot-db)       "$FAULT" reboot-db ;;
  *) echo "unknown SCENARIO $SCENARIO" >&2; exit 2 ;;
esac 2>&1 | tee -a "$RAW/00-timeline.txt"

# --- oracle -------------------------------------------------------------------------
T_EXPIRE=NA; T_CLAIM=NA; T_PROGRESS=NA; HEALED=no; CRASHLOOP=0
heal() {
  [ "$HEALED" = yes ] && return 0; HEALED=yes
  case "$SCENARIO" in
    freeze)    "$FAULT" thaw "$VICTIM" ;;
    netem)     "$FAULT" netem-clear "$VICTIM" ;;
    partition) "$FAULT" heal "$VICTIM" ;;
    drain)     "$FAULT" uncordon "$VICTIM" ;;
  esac 2>&1 | tee -a "$RAW/00-timeline.txt"
}
trap 'heal' EXIT
hold=$FREEZE_S; [ "$SCENARIO" = freeze ] || hold=$HOLD_S
start=$(date +%s)
while [ $(( $(date +%s) - start )) -lt "$MAX_WAIT_S" ]; do
  now=$(dbnow 2>/dev/null || true)
  if [ -n "$now" ]; then
    el=$(( now - T0 ))
    if [ "$T_EXPIRE" = NA ]; then
      n=$(q "select count(*) from shard_leases where shard_id in ($SHARDS) and expires_at > clock_timestamp() and fencing_token = (string_to_array('$TOK_BEFORE', ';'))[array_position(string_to_array('$SHARDS', ',')::int[], shard_id)]::bigint;" 2>/dev/null || echo x)
      [ "$n" = 0 ] && T_EXPIRE=$el
    fi
    if [ "$T_CLAIM" = NA ]; then
      n=$(q "select count(*) from shard_leases l where shard_id in ($SHARDS) and owner_node is not null and expires_at > clock_timestamp() and fencing_token > (string_to_array('$TOK_BEFORE', ';'))[array_position(string_to_array('$SHARDS', ',')::int[], shard_id)]::bigint;" 2>/dev/null || echo x)
      [ "$n" = "$HELD" ] && T_CLAIM=$el
    fi
    if [ "$T_PROGRESS" = NA ]; then
      p=$(q "select (extract(epoch from min(created_at))*1000)::bigint from events where shard_id in ($SHARDS) and created_at > to_timestamp($T0/1000.0);" 2>/dev/null || true)
      [ -n "$p" ] && T_PROGRESS=$(( p - T0 ))
    fi
  fi
  [ "$(crashloop)" = 0 ] || CRASHLOOP=1
  [ "$HEALED" = no ] && [ $(( $(date +%s) - start )) -ge "$hold" ] && heal
  [ "$T_CLAIM" != NA ] && [ "$T_PROGRESS" != NA ] && [ "$HEALED" = yes ] && break
  sleep 0.5
done
heal
log "t_expire=$T_EXPIRE t_claim=$T_CLAIM t_progress=$T_PROGRESS"

# Fencing: after a freeze, the thawed stale owner must be refused, not obeyed.
STALE_REJ=""
if [ "$SCENARIO" = freeze ]; then
  sleep 5
  engine_get "$VICTIM" /metrics > "$RAW/12-victim-metrics-after.txt" 2>/dev/null || true
  STALE_REJ=$(python3 scripts/aws/metrics_delta.py "$RAW/11-victim-metrics-before.txt" "$RAW/12-victim-metrics-after.txt" 2>/dev/null \
               | sed -n 's/^d_fencing_rejections_total=//p')
fi

# --- ring recovery --------------------------------------------------------------------
T_RING=NA; start=$(date +%s)
while [ $(( $(date +%s) - start )) -lt 600 ]; do
  if ring_ok 2>/dev/null; then T_RING=$(( $(dbnow) - T0 )); break; fi
  [ "$(crashloop)" = 0 ] || CRASHLOOP=1
  sleep 2
done
TOK_AFTER=$(q "select string_agg(fencing_token::text, ';' order by shard_id) from shard_leases where shard_id in ($SHARDS);" || true)
NEW_OWNERS=$(q "select string_agg(distinct split_part(owner_node, '.', 1), ';') from shard_leases where shard_id in ($SHARDS);" || true)
q "select shard_id||'|'||coalesce(owner_node,'-')||'|'||fencing_token||'|'||expires_at from shard_leases order by shard_id;" > "$RAW/13-leases-after.txt" || true
RESTARTS_AFTER=$(restarts)

# --- load outcome and correctness --------------------------------------------------------
kubectl -n "$NS" wait --for=condition=complete --timeout=600s "job/$JOB" >/dev/null 2>&1 || true
kubectl -n "$NS" logs "job/$JOB" > "$RAW/30-loadgen.txt" 2>&1 || true
acc=$(grep -E '^ +scheduled [0-9]+, issued' "$RAW/30-loadgen.txt" | tail -1 || true)
LG_COMPLETED=$(sed -n 's/.*accepted [0-9]*, completed \([0-9]*\),.*/\1/p' <<<"$acc")
LG_ERR=$(sed -n 's/.*errored \([0-9]*\).*/\1/p' <<<"$acc"); LG_TO=$(sed -n 's/.*timed out \([0-9]*\),.*/\1/p' <<<"$acc")
LG_P99=$(awk 'index($0,"latency_p99 @") && !s {for(i=1;i<=NF;i++) if($i ~ /^[0-9]+\.?[0-9]*$/){print $i;s=1;exit}}' "$RAW/30-loadgen.txt")
# Drain this trial's workflows before judging loss.
DRAINED=no; start=$(date +%s)
while [ $(( $(date +%s) - start )) -lt 300 ]; do
  left=$(q "select count(*) from executions where workflow_id like '${IDP}%' and status not in ('COMPLETED','FAILED','COMPENSATING_FAILED','CANCELLED');" 2>/dev/null || echo x)
  [ "$left" = 0 ] && { DRAINED=yes; break; }; sleep 5
done
{ printf "\\set prefix '%s'\n" "$IDP"; cat scripts/aws/sql/run_latency_and_invariants.sql; } | probe_sql > "$RAW/40-journal-latency-invariants.txt" 2>&1 || true
ji() { sed -n "s/^inv|$1=//p" "$RAW/40-journal-latency-invariants.txt" | head -1; }
kubectl -n "$NS" get events --sort-by=.lastTimestamp > "$RAW/50-events.txt" 2>&1 || true
[ "$VICTIM" != database ] && kubectl -n "$NS" logs "$VICTIM" --previous --tail=500 > "$RAW/51-victim-previous.log" 2>/dev/null || true
kubectl -n "$NS" delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true

bad=""
for k in dup_run_seq dup_activity_completion seq_gaps multi_terminal; do
  v=$(ji "$k"); [ -n "$v" ] && [ "$v" != 0 ] && bad="${bad}_${k}${v}"
done
if [ -n "$bad" ]; then VERDICT="CORRECTNESS_VIOLATION${bad}"
elif [ -z "$(ji runs)" ]; then VERDICT="INCONCLUSIVE_no_correctness_evidence"
elif [ "$DRAINED" != yes ]; then VERDICT="LOST_OR_STUCK_not_drained_after_300s"
elif [ "$CRASHLOOP" = 1 ]; then VERDICT="CONTAMINATED_crashloop_backoff"
elif [ "$T_RING" = NA ]; then VERDICT="NOT_RECOVERED_ring_incomplete"
elif [ "$T_CLAIM" = NA ] && [ "$HELD" -gt 0 ] && [[ "$SCENARIO" != netem && "$SCENARIO" != sever-db ]]; then VERDICT="NOT_RECOVERED_claim_not_observed"
else VERDICT="ok"
fi
log "verdict=$VERDICT"
( IFS=,; echo "$RUN_ID,$SCENARIO,$TRIAL,$VICTIM,$VNODE,$REPLICAS,$HELD,${TOK_BEFORE},${TOK_AFTER},$T_EXPIRE,$T_CLAIM,$T_PROGRESS,$T_RING,${NEW_OWNERS},${RESTARTS_BEFORE// /;},${RESTARTS_AFTER// /;},$CRASHLOOP,$POLICY_ENFORCED,${STALE_REJ},$LOAD_RATE,${LG_COMPLETED},${LG_ERR},${LG_TO},${LG_P99},$(ji runs),$(ji dup_run_seq),$(ji dup_activity_completion),$(ji seq_gaps),$(ji multi_terminal),$(ji executions_by_status),$DRAINED,$VERDICT,${RAW#"$OUT"/}" ) >> "$CSV"
echo "$SCENARIO t$TRIAL victim=$VICTIM expire=$T_EXPIRE claim=$T_CLAIM progress=$T_PROGRESS ring=$T_RING verdict=$VERDICT"
case "$VERDICT" in ok) exit 0 ;; *) exit 1 ;; esac
