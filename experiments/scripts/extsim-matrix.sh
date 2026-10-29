#!/usr/bin/env bash
# extsim-matrix.sh — external-effect failure matrix against the live lab.
#
# Each scenario drives ONE ExternalEffectProbe workflow and then reads two
# independent sources:
#
#   * the engine's durable state in PostgreSQL (journal, status, task rows)
#   * the simulator's effect ledger, in a SEPARATE process that the engine
#     cannot touch and that survives SIGKILLing the engine
#
# The whole point is counting INVOCATIONS (ledger "Requests") separately from
# EFFECTS (ledger rows per key). at-least-once invocation with effectively-once
# effect looks like requests > performed; a genuine duplicate effect looks like
# two ledger rows sharing one dedup key, which is reported as
# duplicate_effect_count.
#
# A scenario is only PASS if the fault is CONFIRMED to have fired. "Nothing
# went wrong" is not evidence when the fault may never have armed.
#
# RESUME: scenarios append to the output file with a header; re-running appends
# a fresh block rather than overwriting, so a killed run keeps its results.
set -uo pipefail

NS=durexec-lab
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
OUT="${OUT:-$ROOT/docs/results/phase7-extsim-matrix.txt}"
IP="$(minikube ip 2>/dev/null)"
EXT="http://$IP:30091"

q() { kubectl exec -n $NS postgres-0 -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d ' '; }
counters() { curl -sS --max-time 10 "$EXT/counters"; }
effects()  { curl -sS --max-time 10 "$EXT/effects"; }
arm_ext()  { curl -sS --max-time 10 -X POST "$EXT/arm" -H 'content-type: application/json' -d "$1" >/dev/null; }

# Arming the SIMULATOR is a POST; arming an engine CRASH BOUNDARY is a config
# change plus a restart, because the engine deliberately has no endpoint for it.
arm_crash() {
  kubectl -n $NS patch configmap durexec-config --type merge \
    -p "{\"data\":{\"FAULT_INJECTION\":\"$2\",\"FAULT_CRASH_AT\":\"$1\"}}" >/dev/null
  kubectl -n $NS rollout restart statefulset/durexec >/dev/null
  kubectl -n $NS rollout status statefulset/durexec --timeout=400s >/dev/null 2>&1
}
disarm_crash() { arm_crash "" "false"; }

start_probe() {
  local wf="$1" body="$2" code
  for port in 30081 30082; do
    code="$(curl -s -o /tmp/p7b.$$ -w '%{http_code}' --max-time 40 \
      -X POST "http://$IP:$port/v1/workflows" -H 'content-type: application/json' \
      -d "{\"workflow_id\":\"$wf\",\"workflow_type\":\"ExternalEffectProbe\",\"input\":$body}")"
    [ "$code" != "409" ] && { echo "$code"; rm -f /tmp/p7b.$$; return; }
  done
  rm -f /tmp/p7b.$$; echo "409"
}

report() {
  local name="$1" wf="$2" fired="$3"
  local ex ev sched comp fail tasks status dupseq
  ex="$(q "select count(*) from executions where workflow_id='$wf'")"
  ev="$(q "select count(*) from events where workflow_id='$wf'")"
  sched="$(q "select count(*) from events where workflow_id='$wf' and event_type='ActivityTaskScheduled'")"
  comp="$(q "select count(*) from events where workflow_id='$wf' and event_type='ActivityTaskCompleted'")"
  fail="$(q "select count(*) from events where workflow_id='$wf' and event_type='ActivityTaskFailed'")"
  tasks="$(q "select count(*) from tasks where workflow_id='$wf'")"
  status="$(q "select coalesce(status,'-') from executions where workflow_id='$wf'")"
  dupseq="$(q "select count(*) from (select run_id,seq from events where workflow_id='$wf' group by run_id,seq having count(*)>1) x")"
  {
    echo "  fault confirmed fired : $fired"
    echo "  LEDGER (separate process, survives engine death):"
    echo "    counters            : $(counters)"
    # Written to a temp file and read with a script file rather than -c, so
    # no quoting can close early. The earlier inline version used double
    # quotes inside a single-quoted shell string nested in a function; the
    # quoting terminated early and EVERY scenario recorded
    # "(ledger unreadable)" while still printing PASS-shaped output around
    # it. The counters line carried the essential numbers so the evidence was
    # degraded rather than wrong, but it is the same silent-failure pattern
    # this project keeps hitting, so it is recorded in the report.
    effects > /tmp/p7eff.$$ 2>/dev/null
    python3 "$HERE/read_effects.py" /tmp/p7eff.$$ >> "$OUT" 2>/dev/null || echo "    (ledger unreadable)" >> "$OUT"
    rm -f /tmp/p7eff.$$
    echo "  ENGINE durable state:"
    echo "    executions=$ex events=$ev tasks_pending=$tasks status=${status:--}"
    echo "    ActivityTaskScheduled=$sched Completed=$comp Failed=$fail dup(run_id,seq)=$dupseq"
  } >> "$OUT"
  sync
}

# Each scenario gets a FRESH simulator so its ledger contains only its own
# effects. Restarting the deployment is how the in-memory ledger is cleared;
# it is done BEFORE a scenario, never during one.
fresh_sim() {
  kubectl -n $NS rollout restart deploy/extsim >/dev/null
  kubectl -n $NS rollout status deploy/extsim --timeout=180s >/dev/null 2>&1
  for i in $(seq 1 20); do
    curl -sS --max-time 5 "$EXT/healthz" >/dev/null 2>&1 && return 0
    sleep 3
  done
  return 1
}

# The probe is opt-in: base/10-config.yaml ships EXTSIM_URL empty so a plain
# apply registers neither the activity nor the workflow. This script turns it
# on for its own run, so it is self-sufficient and cannot silently measure a
# cluster where the probe was never registered.
ensure_probe() {
  local want="http://extsim.durexec-lab.svc.cluster.local:8090"
  local have
  have="$(kubectl get cm durexec-config -n $NS -o jsonpath='{.data.EXTSIM_URL}' 2>/dev/null)"
  if [ "$have" != "$want" ]; then
    kubectl -n $NS patch configmap durexec-config --type merge \
      -p "{\"data\":{\"EXTSIM_URL\":\"$want\"}}" >/dev/null
    kubectl -n $NS rollout restart statefulset/durexec >/dev/null
    kubectl -n $NS rollout status statefulset/durexec --timeout=400s >/dev/null 2>&1
  fi
  # Refuse to measure if the workflow is not actually registered. Without this
  # a misconfigured run would start nothing, record zero effects, and look
  # exactly like a clean pass.
  for p in 0 1; do
    if ! kubectl logs -n $NS "durexec-$p" -c engined 2>/dev/null | grep -q ExternalEffectProbe; then
      echo "PREFLIGHT: durexec-$p does not have ExternalEffectProbe registered" >&2
      return 1
    fi
  done
  return 0
}

mkdir -p "$(dirname "$OUT")"
{
  echo "========================================================================"
  echo "durexec Phase 7 — external-effect failure matrix   $(date -Is)"
  echo "commit=$(cd "$ROOT" && git rev-parse HEAD)"
  echo "engine image=$(kubectl get pod durexec-0 -n $NS -o jsonpath='{.spec.containers[0].image}' 2>/dev/null)"
  echo "RESUME: OUT=$OUT $HERE/extsim-matrix.sh <scenario|all>"
  echo "========================================================================"
} >> "$OUT"; sync

# ---------------------------------------------------------------------------
scenario_ambiguous() {
  # §4.3 — the effect SUCCEEDS but the caller never sees the confirmation.
  # This is the scenario that distinguishes effectively-once from
  # exactly-once-invocation: the engine MUST retry (it has no way to know the
  # effect landed) and the retry MUST NOT produce a second effect.
  local wf="p7-ambiguous-$(date +%s)"
  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  arm_ext '{"LoseResponseAfterEffect":1}'
  echo "" >> "$OUT"
  echo "---- A. ambiguous outcome: effect lands, response discarded (wf=$wf) ----" >> "$OUT"
  local code; code="$(start_probe "$wf" '{"amount":700,"payee":"ambig","payments":1}')"
  echo "  start http=$code" >> "$OUT"
  sleep 40
  local injected; injected="$(counters | python3 -c 'import json,sys; print(json.load(sys.stdin)["Injected"])' 2>/dev/null)"
  report "ambiguous" "$wf" "simulator Injected=$injected (expect >=1)"
}

scenario_transient() {
  # §4.9 — a transient failure BEFORE the effect. The retry must then produce
  # the effect exactly once; invocations exceed effects by the failure count.
  local wf="p7-transient-$(date +%s)"
  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  arm_ext '{"FailBeforeEffect":2}'
  echo "" >> "$OUT"
  echo "---- B. transient failure before the effect, twice (wf=$wf) ----" >> "$OUT"
  local code; code="$(start_probe "$wf" '{"amount":800,"payee":"transient","payments":1}')"
  echo "  start http=$code" >> "$OUT"
  sleep 60
  local injected; injected="$(counters | python3 -c 'import json,sys; print(json.load(sys.stdin)["Injected"])' 2>/dev/null)"
  report "transient" "$wf" "simulator Injected=$injected (expect 2)"
}

scenario_journaled_crash() {
  # §4.5 — the result IS journaled and the worker then dies before
  # acknowledging the task. Replay must find the journaled result and must NOT
  # invoke the external operation again.
  local wf="p7-journaled-$(date +%s)"
  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  arm_ext '{}'
  arm_crash "AfterActivityJournaled" "true"
  echo "" >> "$OUT"
  echo "---- C. journaled, then crash before task ack (wf=$wf) ----" >> "$OUT"
  local code; code="$(start_probe "$wf" '{"amount":900,"payee":"journaled","payments":1}')"
  echo "  start http=$code" >> "$OUT"
  sleep 45
  local fired=0
  for p in 0 1; do
    n="$(kubectl logs -n $NS durexec-$p -c engined --tail=500 2>/dev/null | grep -c 'injected crash after the activity result was journaled')"
    fired=$((fired+n))
  done
  report "journaled_crash" "$wf" "engine crash lines=$fired (expect >=1)"
  disarm_crash
}

scenario_no_invocation() {
  # §4.1 — the process dies BEFORE the external operation is invoked. The
  # decision that would have scheduled the payment is abandoned, so the
  # ledger must stay completely empty.
  local wf="p7-noinvoke-$(date +%s)"
  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  arm_ext '{}'
  arm_crash "BeforeStartCommit" "true"
  echo "" >> "$OUT"
  echo "---- D. crash before the operation is ever invoked (wf=$wf) ----" >> "$OUT"
  local code; code="$(start_probe "$wf" '{"amount":1000,"payee":"noinvoke","payments":1}')"
  echo "  start http=$code (expect 500: the creation was abandoned)" >> "$OUT"
  sleep 12
  report "no_invocation" "$wf" "start returned $code"
  disarm_crash
}

scenario_saga() {
  # §4.10 — an external effect has landed and a LATER step fails. The
  # ExternalPayment activity declares NO compensation, deliberately, so the
  # honest expectation is a failed workflow with the effect still standing.
  # This is a limitation to report, not a bug to hide.
  local wf="p7-saga-$(date +%s)"
  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  arm_ext '{}'
  echo "" >> "$OUT"
  echo "---- E. later step fails after the effect landed (wf=$wf) ----" >> "$OUT"
  local code; code="$(start_probe "$wf" '{"amount":1100,"payee":"saga","payments":1,"fail_after":true}')"
  echo "  start http=$code" >> "$OUT"
  sleep 60
  report "saga" "$wf" "n/a (no fault injected; the workflow fails itself)"
  local comp; comp="$(q "select count(*) from events where workflow_id='$wf' and event_type like '%Compensat%'")"
  echo "  compensation events for this run: $comp" >> "$OUT"
  echo "  NOTE: ExternalPayment registers no compensation (see PayDefinition)." >> "$OUT"
  echo "        A refund is itself an external effect needing its own dedup key," >> "$OUT"
  echo "        and the relationship between an effect key and its compensation" >> "$OUT"
  echo "        key is a design question this engine has not answered." >> "$OUT"
  sync
}

scenario_claim_expiry() {
  # §4.6 — the task claim expires while the operation is still running. The
  # simulator holds the response past TASK_LOCK, so a second worker may claim
  # and retry. The dedup key must absorb the second invocation.
  local wf="p7-lockexpiry-$(date +%s)"
  local lock; lock="$(kubectl get cm durexec-config -n $NS -o jsonpath='{.data.TASK_LOCK}' 2>/dev/null)"
  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  # Hang longer than TASK_LOCK so the claim lapses mid-flight. The effect is
  # already recorded before the hang begins.
  arm_ext '{"HangAfterEffect":1,"Hang":45000000000}'
  echo "" >> "$OUT"
  echo "---- F. task claim expires while the operation is in flight (wf=$wf, TASK_LOCK=$lock) ----" >> "$OUT"
  local code; code="$(start_probe "$wf" '{"amount":1200,"payee":"lockexpiry","payments":1}')"
  echo "  start http=$code" >> "$OUT"
  sleep 120
  local injected; injected="$(counters | python3 -c 'import json,sys; print(json.load(sys.stdin)["Injected"])' 2>/dev/null)"
  report "claim_expiry" "$wf" "simulator Injected=$injected (hang armed)"
}

scenario_kill_midflight() {
  # §4.2 and §4.4 — the process dies while the operation is IN PROGRESS, after
  # the effect has been recorded but before any result could be journaled.
  # This needs REAL process death: returning an error would not lose the
  # in-memory state. The kill is a node-level SIGKILL, not a pod delete, which
  # would send SIGTERM and allow a graceful drain.
  local wf="p7-killmid-$(date +%s)"
  fresh_sim || { echo "  BLOCKED: simulator unavailable" >> "$OUT"; return; }
  arm_ext '{"HangAfterEffect":1,"Hang":60000000000}'
  echo "" >> "$OUT"
  echo "---- G. SIGKILL the owner while the effect is in flight (wf=$wf) ----" >> "$OUT"
  local code; code="$(start_probe "$wf" '{"amount":1300,"payee":"killmid","payments":1}')"
  echo "  start http=$code" >> "$OUT"
  # Wait for the effect to be recorded, then kill the node that owns the shard.
  sleep 12
  local before; before="$(counters)"
  echo "  ledger before the kill: $before" >> "$OUT"
  local owner; owner="$(q "select owner_node from shard_leases where shard_id=(select shard_id from executions where workflow_id='$wf')")"
  local pod="${owner%%.*}"
  echo "  shard owner=$owner -> killing pod $pod with a node-level SIGKILL" >> "$OUT"
  local cid
  cid="$(minikube ssh -- "docker ps --filter name=k8s_engined_${pod}_ --format '{{.ID}}'" 2>/dev/null | tr -d '\r' | head -1)"
  if [ -z "$cid" ]; then
    echo "  INVALID_container_not_found: cannot SIGKILL; scenario not run" >> "$OUT"; sync; return
  fi
  minikube ssh -- "docker kill --signal=KILL $cid" >/dev/null 2>&1
  echo "  SIGKILLed container $cid" >> "$OUT"
  kubectl -n $NS rollout status statefulset/durexec --timeout=400s >/dev/null 2>&1
  sleep 90
  report "kill_midflight" "$wf" "SIGKILL delivered to $pod (container $cid)"
}

if ! ensure_probe; then
  echo "refusing to run: the external-effect probe is not registered" >&2
  exit 3
fi

case "${1:-all}" in
  ambiguous)   scenario_ambiguous ;;
  transient)   scenario_transient ;;
  journaled)   scenario_journaled_crash ;;
  noinvoke)    scenario_no_invocation ;;
  saga)        scenario_saga ;;
  lockexpiry)  scenario_claim_expiry ;;
  killmid)     scenario_kill_midflight ;;
  all)
    scenario_ambiguous
    scenario_transient
    scenario_journaled_crash
    scenario_no_invocation
    scenario_saga
    scenario_claim_expiry
    scenario_kill_midflight
    ;;
  *) echo "unknown scenario: $1" >&2; exit 2 ;;
esac
echo "matrix run complete -> $OUT"
