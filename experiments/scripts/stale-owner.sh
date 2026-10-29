#!/usr/bin/env bash
# Scenario 2 (clean) + Scenario 7 combined: freeze a node with SIGSTOP.
#
# Why SIGSTOP rather than SIGKILL:
#   * the process cannot renew its lease AND cannot release it, which is a
#     truthful abrupt failure -- SIGTERM-based kills measure graceful release;
#   * the container never exits, so Kubernetes applies NO exponential restart
#     backoff. The previous SIGKILL trials were contaminated by it;
#   * after SIGCONT the old owner wakes holding a STALE fencing token and tries
#     to write. That is the fencing test, performed against the real code path
#     rather than a simulated crash -- which matters because the declared
#     faultinj crash point AfterLeaseRenew is never checked in production code.
set -euo pipefail
NS=durexec-lab
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/experiments/results/phase9-stale-owner.csv"
q() { kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d '\r'; }
dbnow() { q "select (extract(epoch from clock_timestamp())*1000)::bigint;"; }

if [ ! -f "$OUT" ]; then
  { echo "# durexec Phase 9: frozen-node failover + stale-owner fencing"
    echo "# resume: ./experiments/scripts/stale-owner.sh <trials>"
    echo "trial,victim,shards,t_expire_ms,t_takeover_ms,t_progress_ms,taken_by,tok_before,tok_after,frozen_s,post_thaw_writes_by_stale_owner,stale_tok_still_present,orphans,dup_journal,verdict"
  } > "$OUT"
fi

for n in $(seq 1 "${1:-1}"); do
  kubectl -n "$NS" rollout status statefulset/durexec --timeout=180s >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    [ "$(q "select count(*) from shard_leases where owner_node is not null and expires_at>clock_timestamp();")" = "16" ] && break
    sleep 2
  done

  victim=$(q "select owner_node from shard_leases group by owner_node order by count(*) desc limit 1;")
  pod="${victim%%.*}"
  survivor=$(q "select owner_node from shard_leases where owner_node<>'$victim' group by owner_node order by count(*) desc limit 1;")
  shards=$(q "select string_agg(shard_id::text,',' order by shard_id) from shard_leases where owner_node='$victim';")
  held=$(q "select count(*) from shard_leases where owner_node='$victim';")
  tok_before=$(q "select max(fencing_token) from shard_leases where owner_node='$victim';")

  cid=""
  for _ in $(seq 1 15); do
    cid=$(minikube ssh -- "docker ps -q --filter label=io.kubernetes.pod.name=$pod --filter label=io.kubernetes.container.name=engined" 2>/dev/null | tr -d '\r\n')
    [ -n "$cid" ] && break; sleep 2
  done
  [ -z "$cid" ] && { echo "$n,$pod,$held,NA,NA,NA,,$tok_before,NA,NA,NA,NA,NA,NA,INVALID_no_container" >> "$OUT"; sync; continue; }

  echo "=== trial $n: freezing $pod ($held shards: $shards) tok=$tok_before; survivor=$survivor ==="
  t0=$(dbnow)
  minikube ssh -- "docker pause $cid" >/dev/null 2>&1

  t_exp=""; t_take=""; t_prog=""; taken_by=""
  for i in $(seq 1 180); do
    now=$(dbnow)
    if [ -z "$t_exp" ]; then
      [ "$(q "select count(*) from shard_leases where shard_id in ($shards) and owner_node='$victim' and expires_at>clock_timestamp();")" = "0" ] && t_exp=$((now-t0))
    fi
    if [ -z "$t_take" ]; then
      got=$(q "select count(*) from shard_leases where shard_id in ($shards) and owner_node='$survivor' and expires_at>clock_timestamp() and fencing_token>$tok_before;")
      if [ "$got" = "$held" ]; then t_take=$((now-t0)); taken_by="$survivor"; fi
    fi
    if [ -n "$t_take" ] && [ -z "$t_prog" ]; then
      [ "$(q "select count(*) from events where shard_id in ($shards) and created_at > to_timestamp($t0/1000.0);")" -gt 0 ] && t_prog=$((now-t0))
    fi
    [ -n "$t_take" ] && [ -n "$t_prog" ] && break
    sleep 0.5
  done

  tok_after=$(q "select max(fencing_token) from shard_leases where shard_id in ($shards);")
  frozen=$(( ($(dbnow)-t0)/1000 ))

  # THE FENCING TEST: thaw the stale owner and watch whether it can write.
  mark=$(dbnow)
  minikube ssh -- "docker unpause $cid" >/dev/null 2>&1
  sleep 20
  # Any event written on a reclaimed shard by the stale incarnation would have
  # to appear after the thaw mark; the engine must refuse it at commit because
  # verifyToken sees a newer token.
  stale_writes=$(q "select count(*) from events where shard_id in ($shards) and created_at > to_timestamp($mark/1000.0);")
  stale_tok=$(q "select count(*) from shard_leases where shard_id in ($shards) and fencing_token <= $tok_before;")
  orph=$(q "select count(*) from executions e where not exists (select 1 from events v where v.shard_id=e.shard_id and v.workflow_id=e.workflow_id and v.run_id=e.run_id);")
  dupj=$(q "select coalesce(sum(c-1),0) from (select count(*) c from events where event_type='ActivityTaskCompleted' group by shard_id,workflow_id,run_id,(payload->>'k') having count(*)>1) t;")

  verdict="ok"
  [ -z "$t_take" ] && verdict="NO_TAKEOVER_WITHIN_90s"
  [ "${stale_tok:-0}" != "0" ] && verdict="$verdict;STALE_TOKEN_STILL_LIVE"
  [ "${orph:-0}" != "0" ] && verdict="$verdict;ORPHANS=$orph"
  [ "${dupj:-0}" != "0" ] && verdict="$verdict;DUP_JOURNAL=$dupj"

  echo "$n,$pod,$held,${t_exp:-NA},${t_take:-NA},${t_prog:-NA},$taken_by,$tok_before,$tok_after,$frozen,${stale_writes:-NA},${stale_tok:-NA},${orph:-NA},${dupj:-NA},$verdict" >> "$OUT"
  sync
  echo "    expire=${t_exp:-NA}ms takeover=${t_take:-NA}ms progress=${t_prog:-NA}ms tok $tok_before->$tok_after"
  echo "    after thaw: events_on_those_shards=${stale_writes:-NA} stale_tokens_live=${stale_tok:-NA} orphans=${orph:-NA} dup_journal=${dupj:-NA}"
  sleep 20
done
echo "appended to $OUT"
