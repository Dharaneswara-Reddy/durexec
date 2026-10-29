#!/usr/bin/env bash
# storage-profile.sh — how much disk one workflow and one event actually cost.
#
# Reports table and index sizes separately, because they grow for different
# reasons and a single "bytes per event" that silently folds in index overhead
# cannot be used to project anything. Everything is read from
# pg_total_relation_size on the live database; nothing is estimated from row
# widths.
#
# RESUME: re-run; output is appended with a timestamp, so successive runs form
# a growth series rather than overwriting each other.
set -uo pipefail
NS=durexec-lab
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
OUT="${OUT:-$ROOT/docs/results/phase6-storage.txt}"
mkdir -p "$(dirname "$OUT")"

psqlq() { kubectl exec -n $NS postgres-0 -- psql -U durexec -d durexec "$@" 2>/dev/null; }

{
  echo "==== $(date -Is)  commit=$(cd "$ROOT" && git rev-parse --short HEAD) ===="
  echo "-- row counts --"
  psqlq -c "select
      (select count(*) from executions) as executions,
      (select count(*) from events)     as events,
      (select count(*) from tasks)      as tasks_pending,
      round((select count(*) from events)::numeric
            / nullif((select count(*) from executions),0), 2) as events_per_execution"
  echo "-- per-relation size, table vs index --"
  psqlq -c "select relname,
      pg_size_pretty(pg_table_size(c.oid))   as heap,
      pg_size_pretty(pg_indexes_size(c.oid)) as indexes,
      pg_size_pretty(pg_total_relation_size(c.oid)) as total
    from pg_class c join pg_namespace n on n.oid=c.relnamespace
    where n.nspname='public' and c.relkind='r'
    order by pg_total_relation_size(c.oid) desc"
  echo "-- cost per unit, measured not estimated --"
  psqlq -c "with t as (
      select (select count(*) from events) ev,
             (select count(*) from executions) ex,
             pg_table_size('events')   ev_heap,
             pg_indexes_size('events') ev_idx,
             pg_database_size(current_database()) dbsize)
    select ev as events, ex as executions,
      round(ev_heap::numeric/nullif(ev,0))              as event_heap_bytes,
      round(ev_idx::numeric/nullif(ev,0))               as event_index_bytes,
      round((ev_heap+ev_idx)::numeric/nullif(ev,0))     as event_total_bytes,
      round(dbsize::numeric/nullif(ex,0))               as db_bytes_per_workflow,
      pg_size_pretty(dbsize)                            as database_size
    from t"
  echo "-- replay cost distribution, from the engine's own histogram --"
  echo "   (queried separately via Prometheus; see phase6-sweep.csv replay_* columns)"
  echo
} >> "$OUT"
sync
echo "appended -> $OUT"
