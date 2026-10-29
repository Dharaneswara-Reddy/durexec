#!/usr/bin/env bash
# phase8-boundaries.sh — the two boundaries Phase 7 left open.
#
#   H. external effect confirmed, then REAL process death before the result is
#      journaled (uses the BeforeResultJournal seam, which SIGKILLs itself)
#   I. shard ownership moves while the external operation is still in flight,
#      and the stale worker then tries to journal its result
#
# Both read the simulator's ledger from a SEPARATE pod, so the evidence
# survives the engine dying, and both read PostgreSQL independently.
#
# set -e is NOT used. A failed kubectl in the middle of a scenario must record
# an INVALID result and continue, not abort the file and leave the reader
# unable to tell a crash from a clean run.
set -uo pipefail

NS=durexec-lab
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
OUT="${OUT:-$ROOT/docs/results/phase8-boundaries.txt}"
IP="$(minikube ip 2>/dev/null)"
EXT="http://$IP:30091"
SIM_URL="http://extsim.durexec-lab.svc.cluster.local:8090"

q() { kubectl exec -n $NS postgres-0 -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d ' '; }
counters() { curl -sS --max-time 10 "$EXT/counters" 2>/dev/null; }
arm_ext()  { curl -sS --max-time 10 -X POST "$EXT/arm" -H 'content-type: application/json' -d "$1" >/dev/null 2>&1; }
perf()     { counters | python3 -c 'import json,sys; print(json.load(sys.stdin)["Performed"])' 2>/dev/null || echo "-1"; }
restarts() { kubectl get pod "$1" -n $NS -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null; }

set_cfg() {
  kubectl -n $NS patch configmap durexec-config --type merge \
    -p "{\"data\":{\"FAULT_INJECTION\":\"$2\",\"FAULT_CRASH_AT\":\"$1\",\"EXTSIM_URL\":\"$SIM_URL\"}}" >/dev/null
}
restart_engine() {
  kubectl -n $NS rollout restart statefulset/durexec >/dev/null
  kubectl -n $NS rollout status statefulset/durexec --timeout=400s >/dev/null 2>&1
}

fresh_sim() {
  kubectl -n $NS rollout restart deploy/extsim >/dev/null
  kubectl -n $NS rollout status deploy/extsim --timeout=180s >/dev/null 2>&1
  for i in $(seq 1 20); do curl -sS --max-time 5 "$EXT/healthz" >/dev/null 2>&1 && return 0; sleep 3; done
  return 1
}

start_probe() {
  local wf="$1" body="$2" code
  for port in 30081 30082; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 40 \
      -X POST "http://$IP:$port/v1/workflows" -H 'content-type: application/json' \
      -d "{\"workflow_id\":\"$wf\",\"workflow_type\":\"ExternalEffectProbe\",\"input\":$body}")"
    [ "$code" != "409" ] && { echo "$code"; return; }
  done
  echo "409"
}

durable() {
  local wf="$1"
  {
    echo "  ENGINE durable state:"
    echo "    executions=$(q "select count(*) from executions where workflow_id='$wf'") events=$(q "select count(*) from events where workflow_id='$wf'") tasks_pending=$(q "select count(*) from tasks where workflow_id='$wf'") status=$(q "select coalesce(status,'-') from executions where workflow_id='$wf'")"
    echo "    ActivityTaskScheduled=$(q "select count(*) from events where workflow_id='$wf' and event_type='ActivityTaskScheduled'") Completed=$(q "select count(*) from events where workflow_id='$wf' and event_type='ActivityTaskCompleted'") Failed=$(q "select count(*) from events where workflow_id='$wf' and event_type='ActivityTaskFailed'")"
    echo "    duplicate (run_id,seq) groups=$(q "select count(*) from (select run_id,seq from events where workflow_id='$wf' group by run_id,seq having count(*)>1) x")"
    echo "    LEDGER (separate pod): $(counters)"
  } >> "$OUT"
  curl -sS --max-time 10 "$EXT/effects" > /tmp/p8eff.$$ 2>/dev/null
  python3 "$HERE/read_effects.py" /tmp/p8eff.$$ >> "$OUT" 2>/dev/null || echo "    (ledger detail unreadable)" >> "$OUT"
  rm -f /tmp/p8eff.$$
  sync
}

mkdir -p "$(dirname "$OUT")"
{
  echo "========================================================================"
  echo "durexec Phase 8 — boundaries Phase 7 left open   $(date -Is)"
  echo "commit=$(cd "$ROOT" && git rev-parse HEAD)"
  echo "engine image=$(kubectl get pod durexec-0 -n $NS -o jsonpath='{.spec.containers[0].image}' 2>/dev/null)"
  echo "RESUME: OUT=$OUT $HERE/phase8-boundaries.sh <H|I|all>"
  echo "========================================================================"
} >> "$OUT"; sync

# ---------------------------------------------------------------------------
scenario_H() {
  # §5 — effect confirmed, then REAL process death before the result journal.
  #
  # The seam SIGKILLs the process. Because FAULT_CRASH_AT stays in the
  # ConfigMap, the restarted pod re-arms and would die again on the
  # redelivered task, so this scenario DISARMS as soon as it has confirmed the
  # first death. That is a real constraint of the mechanism, not tidiness:
  # without it the node crash-loops and the workflow never completes.
  local wf="p8-H-$(date +%s)"
  echo "" >> "$OUT"
  echo "---- H. effect confirmed, then SIGKILL before the result journal (wf=$wf) ----" >> "$OUT"

  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  arm_ext '{}'

  # QUIESCENCE PRECONDITION, and it is not optional.
  #
  # BeforeResultJournal is armed per PROCESS, not per workflow, so it kills on
  # the first activity result ANYWHERE in the cluster. The first attempt at
  # this scenario failed exactly that way: a Phase 7 workflow left running on
  # a 12-minute timer fired while the seam was armed, killed both nodes, and
  # the probe's own start request then got HTTP 000 because nothing was
  # serving. Refusing to arm until the cluster is idle is what makes the kill
  # attributable to the workflow under test.
  local pend nonterm
  for i in $(seq 1 40); do
    pend="$(q "select count(*) from tasks")"
    # A POSITIVE list of non-terminal states, not a NOT IN over terminal ones.
    # The negated form needed single quotes nested inside a double-quoted
    # command substitution inside this function, the escaping collapsed, and
    # the predicate silently matched every row: the check reported
    # nonterminal=46791 (the whole table) and BLOCKED a cluster that was in
    # fact idle. A positive list needs no escaping at all.
    nonterm="$(q "select count(*) from executions where status in ('RUNNING','COMPENSATING','PARKED')")"
    [ "${pend:-1}" = "0" ] && [ "${nonterm:-1}" = "0" ] && break
    sleep 10
  done
  echo "  quiescence before arming: pending_tasks=${pend:--} nonterminal=${nonterm:--} (both must be 0)" >> "$OUT"
  if [ "${pend:-1}" != "0" ] || [ "${nonterm:-1}" != "0" ]; then
    echo "  BLOCKED_cluster_not_quiescent: refusing to arm a process-killing seam while other work is in flight" >> "$OUT"
    sync; return
  fi

  set_cfg "BeforeResultJournal" "true"
  restart_engine

  local armed=0
  for p in 0 1; do
    armed=$((armed + $(kubectl logs -n $NS durexec-$p -c engined 2>/dev/null | grep -c "ARMED a deliberate crash boundary")))
  done
  echo "  armed on $armed/2 nodes (expect 2)" >> "$OUT"
  local r0_before r1_before
  r0_before="$(restarts durexec-0)"; r1_before="$(restarts durexec-1)"
  echo "  restart counts before: durexec-0=$r0_before durexec-1=$r1_before" >> "$OUT"

  local code; code="$(start_probe "$wf" '{"amount":2100,"payee":"h-killbeforejournal","payments":1}')"
  echo "  start http=$code" >> "$OUT"

  # Wait for the effect to be CONFIRMED by the recipient, which is the
  # precondition the whole scenario rests on: the kill is only meaningful once
  # the effect exists.
  local p_after=0
  for i in $(seq 1 40); do
    p_after="$(perf)"
    [ "$p_after" = "1" ] && break
    sleep 2
  done
  echo "  simulator Performed after start = $p_after (precondition: must be 1)" >> "$OUT"
  if [ "$p_after" != "1" ]; then
    echo "  INVALID_effect_never_confirmed: the kill boundary was never reached" >> "$OUT"
    set_cfg "" "false"; restart_engine; sync; return
  fi

  # Confirm the process actually DIED — and capture that evidence BEFORE
  # disarming.
  #
  # The previous attempt looked for the seam's log line with `--previous`
  # AFTER the disarm, and the disarm does a rollout restart which REPLACES the
  # pod. `--previous` then refers to the new pod's predecessor, not the
  # container that was killed, so the evidence had already been destroyed by
  # the time it was read. The ledger showed Requests=2 / Performed=1 /
  # Replayed=1 with Injected=0, i.e. the engine had plainly lost a result and
  # re-invoked, but "plainly" is not evidence. Both signals are now collected
  # in one loop that runs while the killed container is still the pod's
  # previous one.
  local killed="" seam=0 r0 r1
  for i in $(seq 1 60); do
    r0="$(restarts durexec-0)"; r1="$(restarts durexec-1)"
    seam=0
    for p in 0 1; do
      seam=$((seam + $(kubectl logs -n $NS durexec-$p -c engined --previous 2>/dev/null | grep -c "injected process kill after the external effect completed")))
      seam=$((seam + $(kubectl logs -n $NS durexec-$p -c engined 2>/dev/null | grep -c "injected process kill after the external effect completed")))
    done
    if [ "${r0:-0}" -gt "${r0_before:-0}" ] || [ "${r1:-0}" -gt "${r1_before:-0}" ]; then
      killed="yes (durexec-0: $r0_before->$r0, durexec-1: $r1_before->$r1)"
      break
    fi
    [ "$seam" -gt 0 ] && { killed="yes (seam log line seen, restart counts $r0/$r1)"; break; }
    sleep 2
  done
  echo "  process death confirmed: ${killed:-NO}" >> "$OUT"
  echo "  seam log lines found while the dead container was still addressable: $seam" >> "$OUT"
  sync

  # Disarm IMMEDIATELY now that the evidence is captured, before the restarted
  # pod can die again on the redelivered task. This is why the point declares
  # KillsProcess().
  set_cfg "" "false"
  restart_engine
  echo "  disarmed and restarted (prevents a crash loop on the redelivered task)" >> "$OUT"

  sleep 60
  echo "  AFTER RECOVERY:" >> "$OUT"
  durable "$wf"
}

# ---------------------------------------------------------------------------
scenario_I() {
  # §4 — ownership moves while the external operation is IN FLIGHT, and the
  # stale worker then tries to journal.
  #
  # Ordering tested: the REPLACEMENT invocation begins before the original
  # finishes. That is made deterministic by the simulator, not by sleeps: the
  # response is held for 75s, which is longer than LEASE_TTL (10s) plus the
  # 2xTTL adoption guard plus TASK_LOCK (30s), so ownership and the task claim
  # both move while the original call is still blocked in its HTTP request.
  #
  # Ownership is moved by PAUSING the owner rather than killing it. A paused
  # process cannot renew its lease, so the lease expires and another node
  # adopts the shard — and crucially the paused process is still ALIVE, so
  # when it is thawed it wakes up believing it still owns the shard and tries
  # to journal with a stale fencing token. A kill would destroy the very
  # worker whose stale write is the thing under test.
  local wf="p8-I-$(date +%s)"
  echo "" >> "$OUT"
  echo "---- I. ownership moves while the effect is in flight; stale worker then journals (wf=$wf) ----" >> "$OUT"

  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  set_cfg "" "false"
  arm_ext '{"HangAfterEffect":1,"Hang":75000000000}'
  echo "  simulator armed: effect recorded, then the response held for 75s" >> "$OUT"

  local code; code="$(start_probe "$wf" '{"amount":2200,"payee":"i-leaseloss","payments":1}')"
  echo "  start http=$code" >> "$OUT"

  # Wait until the effect is confirmed, so we know the call is genuinely
  # in flight and not merely scheduled.
  local p_after=0
  for i in $(seq 1 30); do
    p_after="$(perf)"; [ "$p_after" = "1" ] && break; sleep 2
  done
  echo "  simulator Performed = $p_after (the call is in flight, effect already recorded)" >> "$OUT"
  if [ "$p_after" != "1" ]; then
    echo "  INVALID_effect_never_confirmed: ownership was not moved" >> "$OUT"; sync; return
  fi

  local shard owner pod tok_before
  shard="$(q "select shard_id from executions where workflow_id='$wf'")"
  owner="$(q "select owner_node from shard_leases where shard_id=$shard")"
  tok_before="$(q "select fencing_token from shard_leases where shard_id=$shard")"
  pod="${owner%%.*}"
  echo "  shard=$shard owner=$owner fencing_token=$tok_before -> pausing $pod" >> "$OUT"

  local cid
  cid="$(minikube ssh -- "docker ps --filter name=k8s_engined_${pod}_ --format '{{.ID}}'" 2>/dev/null | tr -d '\r' | head -1)"
  if [ -z "$cid" ]; then
    echo "  INVALID_container_not_found: cannot pause; scenario not run" >> "$OUT"; sync; return
  fi
  minikube ssh -- "docker pause $cid" >/dev/null 2>&1
  echo "  PAUSED container $cid (it cannot renew its lease; it is alive, not dead)" >> "$OUT"

  # Wait for the shard to be adopted by the other node: the fencing token must
  # increase, which is the only unambiguous evidence ownership moved.
  local tok_after owner_after
  for i in $(seq 1 40); do
    tok_after="$(q "select fencing_token from shard_leases where shard_id=$shard")"
    owner_after="$(q "select owner_node from shard_leases where shard_id=$shard")"
    if [ -n "$tok_after" ] && [ "${tok_after:-0}" -gt "${tok_before:-0}" ]; then break; fi
    sleep 3
  done
  echo "  after pause: owner=$owner_after fencing_token=$tok_after (was $tok_before)" >> "$OUT"
  if [ "${tok_after:-0}" -le "${tok_before:-0}" ]; then
    echo "  INVALID_ownership_did_not_move: the token never advanced" >> "$OUT"
    minikube ssh -- "docker unpause $cid" >/dev/null 2>&1
    sync; return
  fi

  echo "  ledger while paused: $(counters)" >> "$OUT"

  # Thaw the stale owner. Its HTTP call returns, and it then attempts to
  # journal a result for a shard it no longer owns, with a stale token.
  minikube ssh -- "docker unpause $cid" >/dev/null 2>&1
  echo "  UNPAUSED $cid: the stale worker now wakes and tries to journal" >> "$OUT"
  sleep 120

  local fenced=0
  for p in 0 1; do
    fenced=$((fenced + $(kubectl logs -n $NS durexec-$p -c engined --tail=600 2>/dev/null | grep -ci "fenced out\|lost shard ownership")))
  done
  echo "  stale-write refusals observed in logs: $fenced" >> "$OUT"
  echo "  AFTER RECOVERY:" >> "$OUT"
  durable "$wf"
  echo "  fencing_token now = $(q "select fencing_token from shard_leases where shard_id=$shard")" >> "$OUT"
  sync
}

case "${1:-all}" in
  H) scenario_H ;;
  I) scenario_I ;;
  all) scenario_H; scenario_I ;;
  *) echo "unknown scenario: $1" >&2; exit 2 ;;
esac
echo "phase 8 boundaries complete -> $OUT"
