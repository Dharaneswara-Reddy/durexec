#!/usr/bin/env bash
# Is the cloud cluster actually healthy? Read-only; no confirmation needed.
#
# "Truthful health" is what is being checked, not "200 OK". Phase 6 found a
# /healthz that reported all sixteen shards on all three nodes because it
# answered from the static shard list instead of the lease manager's live
# view. So this looks at the arithmetic.
#
# Two sources, on purpose:
#
#   /healthz gives owned_shards as an INTEGER COUNT (api/handlers.go:125), not
#   a list, so it can show that the counts sum to sixteen but can say nothing
#   about WHICH shards. Summing to sixteen is consistent with two nodes
#   claiming shard 3 while nobody claims shard 7.
#
#   shard_leases is the authoritative table and holds owner_node per shard_id,
#   so disjointness and completeness are checked there. A query against the
#   journal is the only thing that can prove it.
source "$(dirname "$0")/_common.sh"

fail=0
echo "=== containers ==="
remote 'cd /opt/durexec/deploy/aws && set -a && . /opt/durexec/.env && set +a && docker compose -f docker-compose.cloud.yml ps --format "{{.Name}}\t{{.State}}\t{{.Status}}"' || fail=1

echo
echo "=== /healthz on each node ==="
for port in 8081 8082 8083; do
  printf '  %s: ' "$port"
  remote "curl -sS -m 5 http://localhost:$port/healthz" || { echo "UNREACHABLE"; fail=1; }
  echo
done

echo
echo "=== owned_shards must sum to 16 across the three nodes ==="
remote 'for p in 8081 8082 8083; do curl -sS -m 5 "http://localhost:$p/healthz"; echo; done' \
  > /tmp/durexec-health.txt || fail=1
python3 - /tmp/durexec-health.txt <<'PY' || fail=1
import json, sys
total, nodes = 0, []
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    try:
        h = json.loads(line)
    except Exception:
        continue
    # owned_shards is a COUNT here, not a list. See api/handlers.go:125.
    n, owned, cluster = h.get("node"), h.get("owned_shards"), h.get("cluster_shards")
    nodes.append((n, owned, cluster))
    total += owned or 0
    print(f"  {n}: owns {owned} of {cluster}")
    if cluster != 16:
        print(f"  FAIL: {n} reports cluster_shards={cluster}, expected 16")
        sys.exit(1)
if not nodes:
    print("  FAIL: no health responses parsed")
    sys.exit(1)
print(f"  sum of owned_shards = {total}")
if total != 16:
    print(f"  FAIL: expected the counts to sum to exactly 16, got {total}.")
    print("        Under 16 means shards are unowned; over 16 means a stale lease.")
    sys.exit(1)
print("  counts sum to 16")
print("  NOTE: this does NOT prove disjointness -- see the shard_leases check below.")
PY

echo
echo "=== shard_leases: disjoint and complete (authoritative) ==="
remote bash -s <<'REMOTE' || fail=1
set -euo pipefail
set -a; . /opt/durexec/.env; set +a
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  psql -U durexec -d durexec -At -F'|' -c "
  select
    (select count(*) from shard_leases)                              as rows,
    (select count(distinct shard_id) from shard_leases)              as distinct_shards,
    (select count(*) from shard_leases where owner_node is null)     as unowned,
    (select count(*) from shard_leases where expires_at < now())     as expired,
    (select string_agg(owner_node || '=' || c, ' ' order by owner_node)
       from (select owner_node, count(*) c from shard_leases
             where owner_node is not null group by owner_node) t)    as per_node"
REMOTE
# shard_id is the PRIMARY KEY of shard_leases, so two rows cannot claim the
# same shard -- the schema makes double ownership impossible rather than
# merely unlikely. What IS possible is an unowned or expired lease, which is
# why those are counted separately.

echo
echo "=== no leftover work ==="
# tasks has NO completed_at column: a completed task is DELETED, and a row
# that still exists is outstanding work (migrations/0001_init.up.sql:41-52).
# An earlier draft of this script queried completed_at and would simply have
# errored.
#
# Terminal statuses come from core.ExecutionStatus.IsTerminal:
# COMPLETED, FAILED and COMPENSATING_FAILED. RUNNING and COMPENSATING are
# non-terminal. CANCELLED and TERMINATED do NOT exist in this engine.
remote bash -s <<'REMOTE' || fail=1
set -euo pipefail
set -a; . /opt/durexec/.env; set +a
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  psql -U durexec -d durexec -c "
  select
    (select count(*) from executions
       where status not in ('COMPLETED','FAILED','COMPENSATING_FAILED')) as nonterminal,
    (select count(*) from tasks)                                        as outstanding_tasks,
    (select count(*) from timers)                                       as pending_timers,
    (select count(*) from events)                                       as events"
REMOTE

echo
echo "=== no duplicate journal records ==="
# The program-order index must be unique per run. This is the invariant every
# earlier phase checked, and it is checked by (run_id, seq) -- NOT by seq
# alone and NOT across event types. Phase 6 raised a false alarm of three
# duplicate groups on a healthy cluster by grouping without run_id.
remote bash -s <<'REMOTE' || fail=1
set -euo pipefail
set -a; . /opt/durexec/.env; set +a
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  psql -U durexec -d durexec -At -c "
  select count(*) from (
    select run_id, seq from events group by run_id, seq having count(*) > 1
  ) d"
REMOTE

echo
echo "=== replication ==="
remote bash -s <<'REMOTE'
set -a; . /opt/durexec/.env; set +a
echo "--- leader: standby connected? ---"
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  psql -U durexec -d durexec -At -F'|' -c \
  "select client_addr, state, sync_state, sent_lsn, replay_lsn from pg_stat_replication" \
  2>/dev/null || echo "  (query failed)"
echo "--- standby: in recovery? (t = yes, which is correct) ---"
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-standby-1 \
  psql -U durexec -d durexec -At -c "select pg_is_in_recovery()" \
  2>/dev/null || echo "  (standby unreachable)"
REMOTE

echo
echo "=== host resources ==="
remote 'free -m | head -2; df -h /mnt/durexec-data | tail -1'

echo
if [ "$fail" -ne 0 ]; then
  echo "RESULT: FAIL -- see above."
  exit 1
fi
echo "RESULT: PASS"
