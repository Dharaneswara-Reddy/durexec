#!/usr/bin/env bash
# Independent recovery oracle for the durexec Kubernetes lab.
#
# It reads the engine's DURABLE STATE, not the engine's metrics, and it keeps
# three counts deliberately separate (spec section 2 / section 10):
#
#   journal records      rows in events
#   activity invocations ActivityTaskScheduled per (workflow, k)
#   external effects     NOT MEASURABLE in this deployment -- internal/extsim
#                        is test-only and is not wired into engined, so nothing
#                        here may be read as proof of exactly-once side effects.
#
# Usage: oracle.sh <id-prefix>
set -euo pipefail
NS=durexec-lab
PREFIX="${1:?usage: oracle.sh <id-prefix>}"

q() { kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -tAc "$1" 2>/dev/null | tr -d '\r'; }

STARTED=$(q "select count(*) from executions where workflow_id like '${PREFIX}%';")
COMPLETED=$(q "select count(*) from executions where workflow_id like '${PREFIX}%' and status='COMPLETED';")
FAILED=$(q "select count(*) from executions where workflow_id like '${PREFIX}%' and status='FAILED';")
NONTERM=$(q "select count(*) from executions where workflow_id like '${PREFIX}%' and status not in ('COMPLETED','FAILED','CANCELLED','COMPENSATING_FAILED');")

# An orphan is an accepted execution with NO history behind it. This is the
# F-01 class of defect: accepted, acknowledged, permanently abandoned.
ORPHANS=$(q "select count(*) from executions e where e.workflow_id like '${PREFIX}%'
             and not exists (select 1 from events v
                             where v.shard_id=e.shard_id and v.workflow_id=e.workflow_id and v.run_id=e.run_id);")

# Duplicate journal records: the primary key makes an exact duplicate seq
# impossible, so the real question is whether one logical step was journalled
# twice -- two ActivityTaskCompleted rows for the same k in the same run.
DUP_JOURNAL=$(q "select coalesce(sum(c-1),0) from (
                   select count(*) c from events
                   where workflow_id like '${PREFIX}%' and event_type='ActivityTaskCompleted'
                   group by shard_id, workflow_id, run_id, (payload->>'k')
                   having count(*) > 1) t;")

# Duplicate activity INVOCATIONS: the same k scheduled more than once. A retry
# after a crash is legitimate at-least-once behaviour, so this is reported, not
# asserted to be zero.
DUP_INVOKE=$(q "select coalesce(sum(c-1),0) from (
                  select count(*) c from events
                  where workflow_id like '${PREFIX}%' and event_type='ActivityTaskScheduled'
                  group by shard_id, workflow_id, run_id, (payload->>'k')
                  having count(*) > 1) t;")

STUCK_TASKS=$(q "select count(*) from tasks t join executions e using (shard_id, workflow_id)
                 where e.workflow_id like '${PREFIX}%'
                 and e.status not in ('COMPLETED','FAILED','CANCELLED','COMPENSATING_FAILED');")

echo "started=$STARTED completed=$COMPLETED failed=$FAILED nonterminal=$NONTERM orphans=$ORPHANS dup_journal=$DUP_JOURNAL dup_invoke=$DUP_INVOKE pending_tasks=$STUCK_TASKS"
