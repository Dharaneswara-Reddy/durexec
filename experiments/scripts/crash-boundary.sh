#!/usr/bin/env bash
# crash-boundary.sh — exercise one armed crash boundary and check the durable
# state it left behind.
#
# Usage: ./crash-boundary.sh <BeforeStartCommit|BeforeDecisionCommit|AfterActivityJournaled>
#
# The boundary is armed from CONFIGURATION, not over the network: the script
# patches FAULT_CRASH_AT in the engine ConfigMap and restarts the StatefulSet.
# That is deliberate — see internal/config FaultCrashAt. It means one armed
# crash per process start, which is also what makes the result deterministic.
#
# What is asserted is PostgreSQL's durable state, never a metric. A counter on
# a path that then fails is easy to misread, and the whole question here is
# what survived.
#
# RESUME: re-run with the same argument; results append to the output file.
set -uo pipefail

POINT="${1:?usage: crash-boundary.sh <crash point>}"
NS=durexec-lab
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
OUT="${OUT:-$ROOT/docs/results/phase6-crash-boundaries.txt}"
IP="$(minikube ip)"
WF="crash-$(echo "$POINT" | tr 'A-Z' 'a-z')-$(date +%s)"

q() { kubectl exec -n $NS postgres-0 -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d ' '; }

mkdir -p "$(dirname "$OUT")"
if [ ! -s "$OUT" ]; then
  {
    echo "durexec Phase 6 — crash-boundary validation (F9-2)"
    echo "commit=$(cd "$ROOT" && git rev-parse HEAD)"
    echo "RESUME: $HERE/crash-boundary.sh <point>"
    echo "Each block records the durable state PostgreSQL held after the armed"
    echo "boundary fired. These seams abandon a transaction at the boundary;"
    echo "they do NOT kill the process, so they test the durable-state boundary"
    echo "and not loss of in-memory state. Real process death is covered by the"
    echo "Phase 9 SIGKILL work in 04-PHASE9-FAULT-INJECTION.md."
    echo "========================================================================"
  } >> "$OUT"; sync
fi

arm() {
  kubectl -n $NS patch configmap durexec-config --type merge \
    -p "{\"data\":{\"FAULT_INJECTION\":\"true\",\"FAULT_CRASH_AT\":\"$1\"}}" >/dev/null
  kubectl -n $NS rollout restart statefulset/durexec >/dev/null
  kubectl -n $NS rollout status statefulset/durexec --timeout=300s >/dev/null 2>&1
}

disarm() {
  kubectl -n $NS patch configmap durexec-config --type merge \
    -p '{"data":{"FAULT_INJECTION":"false","FAULT_CRASH_AT":""}}' >/dev/null
  kubectl -n $NS rollout restart statefulset/durexec >/dev/null
  kubectl -n $NS rollout status statefulset/durexec --timeout=300s >/dev/null 2>&1
}

start_wf() {
  # Both per-pod ports are tried, because the shard for this id may belong to
  # either node and only the owner can arm-and-fire on its own commit path.
  for port in 30081 30082; do
    code="$(curl -s -o /tmp/cb-body.$$ -w '%{http_code}' --max-time 30 \
      -X POST "http://$IP:$port/v1/workflows" -H 'content-type: application/json' \
      -d "{\"workflow_id\":\"$1\",\"workflow_type\":\"DeliveryOrder\",\"input\":{\"order_id\":\"$1\",\"amount_cents\":1500,\"address\":\"9 Crash Lane\",\"prep_seconds\":1}}")"
    body="$(cat /tmp/cb-body.$$ 2>/dev/null)"; rm -f /tmp/cb-body.$$
    # A 409 means this node is not the owner; try the other one.
    case "$code" in 409) continue;; *) echo "$code|$body"; return;; esac
  done
  echo "409|no owner reachable"
}

{
  echo
  echo "---- $POINT  wf=$WF  at $(date -Is) ----"
  echo "arming via FAULT_CRASH_AT and restarting the StatefulSet"
} >> "$OUT"; sync

arm "$POINT"

armed_log="$(kubectl -n $NS logs durexec-0 -c engined --tail=200 2>/dev/null | grep -c "ARMED a deliberate crash boundary")"
armed_log1="$(kubectl -n $NS logs durexec-1 -c engined --tail=200 2>/dev/null | grep -c "ARMED a deliberate crash boundary")"
{
  echo "arm confirmation in logs: durexec-0=$armed_log durexec-1=$armed_log1 (expect 1 each)"
} >> "$OUT"; sync

r1="$(start_wf "$WF")"
code1="${r1%%|*}"; body1="${r1#*|}"
sleep 6

execs="$(q "select count(*) from executions where workflow_id='$WF'")"
events="$(q "select count(*) from events where workflow_id='$WF'")"
tasks="$(q "select count(*) from tasks where workflow_id='$WF'")"
status="$(q "select coalesce(status,'-') from executions where workflow_id='$WF'")"

# Did the boundary actually fire? This is the assertion whose absence let a
# real defect hide: the seams were armed and reported armed, while
# eventlog.Store held a nil injector so ShouldCrash always returned false.
# "Armed" is not "fired", and only the second is evidence.
fired0="$(kubectl -n $NS logs durexec-0 -c engined --tail=400 2>/dev/null | grep -c "injected crash")"
fired1="$(kubectl -n $NS logs durexec-1 -c engined --tail=400 2>/dev/null | grep -c "injected crash")"
case "$POINT" in
  BeforeStartCommit)
    # A fired start boundary must refuse the request, so a 201 is proof it
    # did NOT fire.
    if [ "$code1" = "201" ]; then verdict="FAILED_boundary_did_not_fire (got 201 Created)"; else verdict="fired"; fi ;;
  *)
    verdict="see durable state"; ;;
esac

{
  echo "crash-error log lines: durexec-0=$fired0 durexec-1=$fired1"
  echo "FIRE VERDICT: $verdict"
  echo "attempt 1: http=$code1"
  echo "  body: $(echo "$body1" | head -c 220)"
  echo "  durable state: executions=$execs events=$events tasks=$tasks status=${status:--}"
} >> "$OUT"; sync

# Second attempt: the crash is consumed, so the same request must now behave
# normally. For BeforeStartCommit that is the whole correctness claim — the
# first attempt left NO trace, so a retry is a fresh start and not a
# "workflow already open".
r2="$(start_wf "$WF")"
code2="${r2%%|*}"
sleep 25
execs2="$(q "select count(*) from executions where workflow_id='$WF'")"
events2="$(q "select count(*) from events where workflow_id='$WF'")"
status2="$(q "select coalesce(status,'-') from executions where workflow_id='$WF'")"
# Duplicate checks that matter regardless of which boundary fired.
dupseq="$(q "select count(*) from (select run_id,seq from events where workflow_id='$WF' group by run_id,seq having count(*)>1) x")"
# Program-order duplicates are only meaningful among SCHEDULED events: a
# COMPLETED and its SCHEDULED legitimately share a k, so grouping across all
# types counts correct pairs as duplicates. An earlier version of this query
# did exactly that and reported 3 duplicates on a healthy run.
dupk="$(q "select count(*) from (select run_id,(payload->>'k') k from events where workflow_id='$WF' and event_type='ActivityTaskScheduled' and payload ? 'k' group by run_id,(payload->>'k') having count(*)>1) x")"
invokes="$(q "select count(*) from events where workflow_id='$WF' and event_type='ActivityTaskScheduled'")"
completed="$(q "select count(*) from events where workflow_id='$WF' and event_type='ActivityTaskCompleted'")"

{
  echo "attempt 2 (crash consumed): http=$code2"
  echo "  durable state: executions=$execs2 events=$events2 status=${status2:--}"
  echo "  duplicate (run_id,seq) groups: $dupseq   duplicate program-order k groups: $dupk"
  echo "  ActivityTaskScheduled=$invokes  ActivityTaskCompleted=$completed"
} >> "$OUT"; sync

disarm
{
  echo "disarmed; FAULT_INJECTION=false FAULT_CRASH_AT=\"\""
  echo "cluster: $(kubectl -n $NS get pods -l app=durexec -o jsonpath='{range .items[*]}{.metadata.name}={.status.containerStatuses[0].ready}/{.status.containerStatuses[0].restartCount} {end}')"
} >> "$OUT"; sync
echo "recorded $POINT -> $OUT"
