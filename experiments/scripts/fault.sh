#!/usr/bin/env bash
# Phase 9 fault-injection harness.
#
# RECOVERY TIME IS DEFINED PRECISELY (spec section 2). Four instants, all taken
# from the database's own clock so host clock skew cannot distort them:
#
#   t_inject   the fault is applied
#   t_expire   every lease the victim held has passed expires_at
#   t_claim    a surviving node holds ALL those shards with a HIGHER token
#   t_progress the first event is committed on an affected shard after t_inject
#
# "Running again" is not recovered. t_claim means ownership moved; t_progress
# means useful work resumed. Both are reported, because they answer different
# questions and conflating them is how a failover gets described as faster than
# it is.
#
# Results are appended one row at a time and flushed, so a run killed at trial
# 3 leaves 3 usable rows (CLAUDE.md section 3).
set -euo pipefail
NS=durexec-lab
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/experiments/results/phase9-recovery.csv"

q() { kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d '\r'; }
# Database clock, as epoch milliseconds, so every instant shares one clock.
dbnow() { q "select (extract(epoch from clock_timestamp())*1000)::bigint;"; }

if [ ! -f "$OUT" ]; then
  {
    echo "# durexec Phase 9 recovery measurements"
    echo "# resume: ./experiments/scripts/fault.sh <scenario> <trials>"
    echo "# t_* are milliseconds from t_inject, measured on the PostgreSQL clock"
    echo "scenario,trial,victim,shards_held,t_expire_ms,t_claim_ms,t_progress_ms,new_owner,token_before,token_after,restarts,verdict"
  } > "$OUT"
fi

scenario="${1:?usage: fault.sh <graceful|abrupt|stale-owner|db-outage> <trials>}"
trials="${2:-1}"

for n in $(seq 1 "$trials"); do
  # PRECONDITION (spec section 3: "documented initial state"). Both pods ready
  # and all 16 shards held on a live lease, or this trial measures recovery
  # from the previous trial rather than from its own fault.
  kubectl -n "$NS" rollout status statefulset/durexec --timeout=180s >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    liveall=$(q "select count(*) from shard_leases where owner_node is not null and expires_at>clock_timestamp();")
    ready=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready} {end}' | tr -d ' \n' | grep -c false || true)
    [ "${liveall:-0}" = "16" ] && [ "${ready:-1}" = "0" ] && break
    sleep 2
  done
  echo "  precondition: 16/16 shards live=${liveall:-?}, pods not-ready=${ready:-?}"

  # Pick the victim as the pod holding the most shards, so the fault matters.
  victim=$(q "select owner_node from shard_leases group by owner_node order by count(*) desc limit 1;")
  pod="${victim%%.*}"
  held=$(q "select count(*) from shard_leases where owner_node='$victim';")
  shards=$(q "select string_agg(shard_id::text,',' order by shard_id) from shard_leases where owner_node='$victim';")
  tok_before=$(q "select max(fencing_token) from shard_leases where owner_node='$victim';")
  r_before=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}')

  echo "=== $scenario trial $n: victim=$pod holds $held shards ($shards) token=$tok_before ==="

  t_inject=$(dbnow)
  case "$scenario" in
    graceful) kubectl -n "$NS" delete pod "$pod" --grace-period=45 --wait=false >/dev/null ;;
    abrupt)
              # SIGKILL the container from inside the minikube node. The engine
              # never sees a signal it can act on, so no lease is released and
              # the lease must genuinely time out. Anything routed through
              # kubelet (delete --force included) delivers SIGTERM first and
              # measures graceful shutdown instead.
              cid=""
              for _ in $(seq 1 15); do
                cid=$(minikube ssh -- "docker ps -q --filter label=io.kubernetes.pod.name=$pod --filter label=io.kubernetes.container.name=engined" 2>/dev/null | tr -d '\r\n')
                [ -n "$cid" ] && break
                sleep 2
              done
              if [ -z "$cid" ]; then
                echo "    SKIP: container for $pod never appeared"
                echo "$scenario,$n,$pod,$held,NA,NA,NA,,$tok_before,NA,NA,INVALID_container_not_found" >> "$OUT"
                sync; continue
              fi
              minikube ssh -- "docker kill --signal=KILL $cid" >/dev/null 2>&1 ;;
    *) echo "scenario $scenario not implemented here"; exit 2 ;;
  esac

  t_expire=""; t_claim=""; t_progress=""; new_owner=""
  # Sample every 500ms for up to 120s.
  for i in $(seq 1 240); do
    now=$(dbnow)
    if [ -z "$t_expire" ]; then
      live=$(q "select count(*) from shard_leases where shard_id in ($shards) and owner_node='$victim' and expires_at>clock_timestamp();")
      [ "$live" = "0" ] && t_expire=$((now - t_inject))
    fi
    if [ -z "$t_claim" ]; then
      claimed=$(q "select count(*) from shard_leases where shard_id in ($shards) and owner_node is not null and expires_at>clock_timestamp() and fencing_token>$tok_before;")
      if [ "$claimed" = "$held" ]; then
        t_claim=$((now - t_inject))
        new_owner=$(q "select string_agg(distinct owner_node, '|') from shard_leases where shard_id in ($shards);")
      fi
    fi
    if [ -n "$t_claim" ] && [ -z "$t_progress" ]; then
      prog=$(q "select count(*) from events where shard_id in ($shards) and created_at > to_timestamp($t_inject/1000.0);")
      [ "${prog:-0}" -gt 0 ] && t_progress=$((now - t_inject))
    fi
    [ -n "$t_claim" ] && [ -n "$t_progress" ] && break
    sleep 0.5
  done

  tok_after=$(q "select max(fencing_token) from shard_leases where shard_id in ($shards);")
  kubectl -n "$NS" rollout status statefulset/durexec --timeout=180s >/dev/null 2>&1 || true
  r_after=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo NA)

  verdict="ok"
  [ -z "$t_claim" ] && verdict="NO_CLAIM_WITHIN_120s"
  [ -z "$t_progress" ] && verdict="${verdict};no_progress_observed"

  echo "$scenario,$n,$pod,$held,${t_expire:-NA},${t_claim:-NA},${t_progress:-NA},$new_owner,$tok_before,$tok_after,${r_before}->${r_after},$verdict" >> "$OUT"
  sync
  echo "    t_expire=${t_expire:-NA}ms t_claim=${t_claim:-NA}ms t_progress=${t_progress:-NA}ms token $tok_before->$tok_after"

  # Let the cluster settle so the next trial starts from a known state.
  sleep 15
done
echo; echo "appended to $OUT"
