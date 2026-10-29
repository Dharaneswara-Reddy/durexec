#!/usr/bin/env bash
# Test B: dedicated retry-storm workload.
#
# The storm is REAL, not synthetic: retryable activity failures are armed
# through POST /v1/admin/fail-step, so every failure goes round the engine's
# own retry path and the backoff that results is the production backoff. No row
# is hand-inserted into tasks, and no temporary table is used -- poll cost is
# measured against the real `tasks` table and the real `tasks_poll_ready` index.
#
# Poll latency comes from pg_stat_statements, i.e. the server's own timing of
# the actual pollSQL, plus EXPLAIN (ANALYZE, BUFFERS) of the selection CTE so
# rows read AND buffers touched are both recorded -- a lower row count alone
# does not prove lower cost.
set -uo pipefail
NS=durexec-lab
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/experiments/results/phase9-retry-storm.csv"
PLANS="$ROOT/experiments/results/phase9-retry-storm-plans.txt"
IP=$(minikube ip)

q() { kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d '\r' || true; }
qq() { kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -c "$1" 2>/dev/null; }

if [ ! -f "$OUT" ]; then
  { echo "# durexec Phase 9 Test B: retry storm on the real schema"
    echo "# resume: ./experiments/scripts/retry-storm.sh <phase-label> <armed_failures>"
    echo "phase,t_s,tasks_total,claimable,backing_off,locked,completed_total,poll_calls,poll_mean_ms,poll_max_ms,poll_rows,shared_blks_hit,shared_blks_read,retries_total"
  } > "$OUT"
fi

label="${1:-storm}"
arm="${2:-0}"

sample() {  # sample <phase> <t_s>
  local ph="$1" t="$2" row stats
  # ONE query for the queue shape: taken in separate statements, now() advances
  # between them and claimable+backing_off could exceed the total.
  row=$(q "select count(*)||'|'||
             count(*) filter (where GREATEST(visible_at, COALESCE(locked_until,'-infinity'::timestamptz)) <= now())||'|'||
             count(*) filter (where GREATEST(visible_at, COALESCE(locked_until,'-infinity'::timestamptz)) >  now())||'|'||
             count(*) filter (where locked_until is not null and locked_until > now())||'|'||
             coalesce(sum(attempt-1),0)
           from tasks;")
  # round() has no (double precision, int) form in PostgreSQL, so the division
  # must be cast to numeric or this silently returns nothing.
  stats=$(q "select coalesce(sum(calls),0)||'|'||
                    coalesce(round((sum(total_exec_time)/greatest(sum(calls),1))::numeric,3),0)||'|'||
                    coalesce(round(max(max_exec_time)::numeric,3),0)||'|'||
                    coalesce(sum(rows),0)||'|'||
                    coalesce(sum(shared_blks_hit),0)||'|'||
                    coalesce(sum(shared_blks_read),0)
             from pg_stat_statements where query like '%SKIP LOCKED%';")
  local comp; comp=$(q "select count(*) from executions where status='COMPLETED';")
  IFS='|' read -r tot cl bo lk retr <<< "${row:-NA|NA|NA|NA|NA}"
  IFS='|' read -r calls mean mx rows hit rd <<< "${stats:-NA|NA|NA|NA|NA|NA}"
  echo "$ph,$t,${tot:-NA},${cl:-NA},${bo:-NA},${lk:-NA},${comp:-NA},${calls:-NA},${mean:-NA},${mx:-NA},${rows:-NA},${hit:-NA},${rd:-NA},${retr:-NA}" >> "$OUT"
  sync
  echo "  t=${t}s tasks=${tot} claimable=${cl} backing_off=${bo} poll_calls=${calls} poll_mean=${mean}ms poll_max=${mx}ms blks_read=${rd}"
}

plan() {  # plan <label>
  {
    echo "=============================================================="
    echo "EXPLAIN (ANALYZE, BUFFERS) of the real poll selection -- $1"
    echo "queue shape: total=$(q "select count(*) from tasks;") claimable=$(q "select count(*) from tasks where GREATEST(visible_at, COALESCE(locked_until,'-infinity'::timestamptz)) <= now();") backing_off=$(q "select count(*) from tasks where GREATEST(visible_at, COALESCE(locked_until,'-infinity'::timestamptz)) > now();")"
    echo "$(date -Is)"
  } >> "$PLANS"
  # Read-only: the selection CTE only. FOR UPDATE is omitted deliberately so
  # this probe cannot steal a task from a live worker.
  for sh in 0 13; do
    echo "--- shard $sh, ACTIVITY ---" >> "$PLANS"
    kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -c \
      "EXPLAIN (ANALYZE, BUFFERS) SELECT shard_id, task_id FROM tasks
        WHERE shard_id=$sh AND task_type='ACTIVITY'
          AND GREATEST(visible_at, COALESCE(locked_until, '-infinity'::timestamptz)) <= now()
        ORDER BY shard_id, task_type, GREATEST(visible_at, COALESCE(locked_until, '-infinity'::timestamptz))
        LIMIT 1;" 2>/dev/null >> "$PLANS"
  done
}

echo "=== Test B phase '$label', arming $arm retryable failures per node ==="
q "select pg_stat_statements_reset();" >/dev/null
plan "BEFORE ($label)"
sample "${label}-before" 0

if [ "$arm" -gt 0 ]; then
  # Arm on BOTH nodes: route through a workflow each node owns, so each node's
  # own injector is armed. fail-step arms by activity type on the owning node.
  for act in ChargePayment ReserveInventory; do
    for wf in $(q "select workflow_id from executions where status='RUNNING' limit 4;"); do
      curl -s --max-time 6 -X POST "http://$IP:30080/v1/admin/fail-step" \
        -H 'Content-Type: application/json' \
        -d "{\"workflow_id\":\"$wf\",\"activity_type\":\"$act\",\"attempts\":$arm,\"non_retryable\":false}" >/dev/null 2>&1 || true
    done
  done
  echo "  armed $arm retryable failures for ChargePayment and ReserveInventory"
fi

for t in 15 30 45 60 90 120; do
  sleep 15
  sample "$label" "$t"
done
plan "AFTER ($label)"
echo "appended to $OUT and $PLANS"
