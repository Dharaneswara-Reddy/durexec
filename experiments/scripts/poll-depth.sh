#!/usr/bin/env bash
# Test B, part 2: poll cost versus queue DEPTH, on the REAL schema.
#
# Why synthetic rows are correct here, and why the audit's version was not:
# the audit measured this on a TEMP table that omitted the primary key, so its
# 52x figure was meaningless. These rows go into the real `tasks` table with
# the real tasks_pkey and the real tasks_poll_ready index, so the planner sees
# production statistics and production access paths.
#
# Safety: every probe row has visible_at = now() + 1 day, so GREATEST(...) is
# always in the future and NO worker can ever claim one. They are inert
# backlog. All rows carry the __depthprobe__ prefix and are deleted at the end.
set -uo pipefail
NS=durexec-lab
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/experiments/results/phase9-poll-depth.csv"
PLANS="$ROOT/experiments/results/phase9-poll-depth-plans.txt"
q() { kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d '\r' || true; }
qv() { kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -c "$1" 2>/dev/null || true; }

cleanup() {
  echo "  cleaning up probe rows..."
  q "delete from tasks where workflow_id like '__depthprobe__%';" >/dev/null
  echo "  remaining probe rows: $(q "select count(*) from tasks where workflow_id like '__depthprobe__%';")"
}
trap cleanup EXIT

[ -f "$OUT" ] || { echo "# durexec Phase 9 Test B2: poll cost vs queue depth, real schema + real indexes"
  echo "# resume: ./experiments/scripts/poll-depth.sh"
  echo "# probe rows are inert (visible_at = now()+1day) and deleted on exit"
  echo "depth_rows,claimable,scan_type,actual_ms,buffers_hit,buffers_read,rows_out,index_used" > "$OUT"; }

probe() {  # probe <depth> <claimable_desc>
  local depth="$1" cdesc="$2" plan line scan ms hit rd rows idx
  plan=$(kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -t -c \
    "EXPLAIN (ANALYZE, BUFFERS) SELECT shard_id, task_id FROM tasks
      WHERE shard_id=0 AND task_type='ACTIVITY'
        AND GREATEST(visible_at, COALESCE(locked_until, '-infinity'::timestamptz)) <= now()
      ORDER BY shard_id, task_type, GREATEST(visible_at, COALESCE(locked_until, '-infinity'::timestamptz))
      LIMIT 1;" 2>/dev/null)
  { echo "===== depth=$depth claimable=$cdesc ====="; echo "$plan"; } >> "$PLANS"
  scan=$(echo "$plan" | grep -oE "(Seq Scan|Index Scan|Index Only Scan|Bitmap Heap Scan|Bitmap Index Scan)" | head -1)
  idx=$(echo "$plan" | grep -oE "tasks_poll_ready|tasks_pkey" | head -1)
  ms=$(echo "$plan" | grep -oE "actual time=[0-9.]+\.\.[0-9.]+" | head -1 | grep -oE "[0-9.]+$")
  hit=$(echo "$plan" | grep -oE "shared hit=[0-9]+" | head -1 | grep -oE "[0-9]+")
  rd=$(echo "$plan" | grep -oE "read=[0-9]+" | head -1 | grep -oE "[0-9]+")
  rows=$(echo "$plan" | grep -oE "rows=[0-9]+ loops" | head -1 | grep -oE "[0-9]+")
  echo "$depth,$cdesc,${scan:-?},${ms:-?},${hit:-0},${rd:-0},${rows:-?},${idx:-NONE}" >> "$OUT"
  sync
  printf "  depth=%-7s claimable=%-4s %-20s %8sms hit=%-6s read=%-5s index=%s\n" \
    "$depth" "$cdesc" "${scan:-?}" "${ms:-?}" "${hit:-0}" "${rd:-0}" "${idx:-NONE}"
}

echo "=== baseline (no probe rows) ==="
probe "$(q "select count(*) from tasks where shard_id=0;")" "mixed"

for depth in 1000 10000 100000; do
  echo "=== building shard 0 backlog to $depth inert rows ==="
  q "insert into tasks (shard_id, task_type, workflow_id, run_id, visible_at, attempt)
     select 0, 'ACTIVITY', '__depthprobe__'||g, gen_random_uuid(), now() + interval '1 day', 1
     from generate_series(1, $depth - (select count(*) from tasks where workflow_id like '__depthprobe__%')) g
     on conflict do nothing;" >/dev/null
  q "ANALYZE tasks;" >/dev/null
  n=$(q "select count(*) from tasks where shard_id=0;")
  probe "$n" "0"
done

echo "=== the important case: ONE claimable needle in a 100k haystack ==="
q "insert into tasks (shard_id, task_type, workflow_id, run_id, visible_at, attempt)
   values (0,'ACTIVITY','__depthprobe__needle', gen_random_uuid(), now() - interval '1 minute', 1);" >/dev/null
q "ANALYZE tasks;" >/dev/null
probe "$(q "select count(*) from tasks where shard_id=0;")" "1"

echo "=== table/index size at depth ==="
qv "select pg_size_pretty(pg_relation_size('tasks')) as heap,
           pg_size_pretty(pg_relation_size('tasks_poll_ready')) as poll_idx,
           pg_size_pretty(pg_relation_size('tasks_pkey')) as pkey;"
