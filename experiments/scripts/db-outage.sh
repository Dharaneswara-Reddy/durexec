#!/usr/bin/env bash
# Test A: genuine PostgreSQL unavailability, then restoration.
#
# TWO injection methods, because they are different failures:
#
#   pause    docker pause on the postgres container. TCP stays open but nothing
#            answers, so the engine must hit client/query timeouts. This is the
#            network-partition shape -- the hard one, no RST to react to.
#   restart  scale statefulset/postgres to 0 then back to 1. The process is
#            gone, so connections are REFUSED immediately, then the database
#            restarts and replays WAL. The PVC is retained, so committed data
#            must survive -- that is the durability half of this test.
#
# Correctness comes from the DURABLE-STATE ORACLE, never from engine metrics.
# A missing observation is recorded as unknown, never as a pass.
set -euo pipefail
NS=durexec-lab
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/experiments/results/phase9-db-outage.csv"
LOGDIR="$ROOT/experiments/results/phase9-db-outage-logs"
mkdir -p "$LOGDIR"

q() { kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d '\r'; }
# Host clock in ms: the database clock is unusable while the database is down.
hostnow() { date +%s%3N; }
# Can the engine reach the database? Asked THROUGH the engine's own API, so this
# measures what the engine can do, not what kubectl can do.
engine_ok() { curl -s --max-time 4 "http://$IP:30080/v1/shards" >/dev/null 2>&1; }
db_ok() { kubectl -n "$NS" exec statefulset/postgres -- pg_isready -U durexec -d durexec >/dev/null 2>&1; }

IP=$(minikube ip)
method="${1:?usage: db-outage.sh <pause|restart> <outage_seconds> [trials]}"
secs="${2:-30}"
trials="${3:-1}"

if [ ! -f "$OUT" ]; then
  { echo "# durexec Phase 9 Test A: PostgreSQL outage and recovery"
    echo "# resume: ./experiments/scripts/db-outage.sh <pause|restart> <outage_seconds> [trials]"
    echo "# t_* are milliseconds from t_inject, on the HOST clock (the db clock is unavailable mid-outage)"
    echo "method,trial,outage_s,t_db_down_ms,t_db_up_ms,t_engine_recovered_ms,t_progress_ms,wf_before,wf_after,completed_after,failed_after,inflight_after,orphans,dup_journal,dup_invoke,stuck_gt_5min,engine_restarts,verdict"
  } > "$OUT"
fi

for n in $(seq 1 "$trials"); do
  kubectl -n "$NS" rollout status statefulset/durexec --timeout=180s >/dev/null 2>&1 || true
  echo "=== $method trial $n: ${secs}s outage ==="

  wf_before=$(q "select count(*) from executions;")
  ev_before=$(q "select count(*) from events;")
  r_before=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}+{end}')
  pgcid=$(minikube ssh -- "docker ps -q --filter label=io.kubernetes.pod.name=postgres-0 --filter label=io.kubernetes.container.name=postgres" 2>/dev/null | tr -d '\r\n')

  t0=$(hostnow); t_down=""; t_up=""; t_eng=""; t_prog=""

  case "$method" in
    pause)
      [ -z "$pgcid" ] && { echo "$method,$n,$secs,NA,NA,NA,NA,$wf_before,NA,NA,NA,NA,NA,NA,NA,NA,NA,INVALID_no_pg_container" >> "$OUT"; sync; continue; }
      minikube ssh -- "docker pause $pgcid" >/dev/null 2>&1 ;;
    restart)
      kubectl -n "$NS" scale statefulset/postgres --replicas=0 >/dev/null ;;
    *) echo "unknown method"; exit 2 ;;
  esac

  # Confirm the database is GENUINELY unavailable. If it never went down this
  # is not an outage test and must not be reported as one.
  for _ in $(seq 1 60); do
    if ! db_ok; then t_down=$(( $(hostnow)-t0 )); break; fi
    sleep 1
  done
  if [ -z "$t_down" ]; then
    echo "    DATABASE NEVER WENT DOWN -- recording as invalid"
    case "$method" in
      pause) minikube ssh -- "docker unpause $pgcid" >/dev/null 2>&1 ;;
      restart) kubectl -n "$NS" scale statefulset/postgres --replicas=1 >/dev/null ;;
    esac
    echo "$method,$n,$secs,NA,NA,NA,NA,$wf_before,NA,NA,NA,NA,NA,NA,NA,NA,NA,INVALID_db_never_unavailable" >> "$OUT"; sync; continue
  fi
  echo "    database unavailable after ${t_down}ms; holding ${secs}s"

  # Capture engine behaviour DURING the outage.
  sleep "$secs"
  kubectl -n "$NS" logs durexec-0 --tail=120 > "$LOGDIR/${method}-t${n}-durexec0-during.log" 2>/dev/null || true

  # Restore.
  t_restore=$(hostnow)
  case "$method" in
    pause) minikube ssh -- "docker unpause $pgcid" >/dev/null 2>&1 ;;
    restart) kubectl -n "$NS" scale statefulset/postgres --replicas=1 >/dev/null
             kubectl -n "$NS" rollout status statefulset/postgres --timeout=300s >/dev/null 2>&1 || true ;;
  esac
  for _ in $(seq 1 120); do db_ok && { t_up=$(( $(hostnow)-t0 )); break; }; sleep 1; done

  # Engine recovery = the engine can serve a database-backed endpoint again.
  for _ in $(seq 1 180); do engine_ok && { t_eng=$(( $(hostnow)-t0 )); break; }; sleep 1; done
  # Useful progress = a NEW event committed after the injection instant.
  if [ -n "$t_eng" ]; then
    for _ in $(seq 1 180); do
      nev=$(q "select count(*) from events;")
      [ -n "$nev" ] && [ "${nev:-0}" -gt "${ev_before:-0}" ] && { t_prog=$(( $(hostnow)-t0 )); break; }
      sleep 1
    done
  fi

  sleep 25   # let in-flight work drain before reconciling
  wf_after=$(q "select count(*) from executions;")
  comp=$(q "select count(*) from executions where status='COMPLETED';")
  fail=$(q "select count(*) from executions where status='FAILED';")
  infl=$(q "select count(*) from executions where status not in ('COMPLETED','FAILED','CANCELLED','COMPENSATING_FAILED');")
  orph=$(q "select count(*) from executions e where not exists (select 1 from events v where v.shard_id=e.shard_id and v.workflow_id=e.workflow_id and v.run_id=e.run_id);")
  dupj=$(q "select coalesce(sum(c-1),0) from (select count(*) c from events where event_type='ActivityTaskCompleted' group by shard_id,workflow_id,run_id,(payload->>'k') having count(*)>1) t;")
  dupi=$(q "select coalesce(sum(c-1),0) from (select count(*) c from events where event_type='ActivityTaskScheduled' group by shard_id,workflow_id,run_id,(payload->>'k') having count(*)>1) t;")
  stuck=$(q "select count(*) from executions where status not in ('COMPLETED','FAILED','CANCELLED','COMPENSATING_FAILED') and created_at < clock_timestamp() - interval '5 minutes';")
  r_after=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}+{end}')
  kubectl -n "$NS" logs durexec-0 --tail=120 > "$LOGDIR/${method}-t${n}-durexec0-after.log" 2>/dev/null || true

  verdict="ok"
  [ -z "$t_up" ] && verdict="DB_DID_NOT_RETURN"
  [ -z "$t_eng" ] && verdict="$verdict;ENGINE_DID_NOT_RECOVER"
  [ -z "$t_prog" ] && verdict="$verdict;NO_PROGRESS_OBSERVED"
  [ "${orph:-0}" != "0" ] && verdict="$verdict;ORPHANS=$orph"
  [ "${dupj:-0}" != "0" ] && verdict="$verdict;DUP_JOURNAL=$dupj"
  [ "${stuck:-0}" != "0" ] && verdict="$verdict;STUCK=$stuck"

  echo "$method,$n,$secs,${t_down},${t_up:-NA},${t_eng:-NA},${t_prog:-NA},$wf_before,$wf_after,$comp,$fail,$infl,${orph:-NA},${dupj:-NA},${dupi:-NA},${stuck:-NA},${r_before}->${r_after},$verdict" >> "$OUT"
  sync
  echo "    db_up=${t_up:-NA}ms engine_recovered=${t_eng:-NA}ms progress=${t_prog:-NA}ms"
  echo "    orphans=${orph:-NA} dup_journal=${dupj:-NA} dup_invoke=${dupi:-NA} stuck=${stuck:-NA} verdict=$verdict"
  sleep 20
done
echo "appended to $OUT"
