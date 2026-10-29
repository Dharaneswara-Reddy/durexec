#!/usr/bin/env bash
# Pull logs, metrics and journal state off the instance. Read-only.
#
# This runs BEFORE any disarm, restart or teardown, never after. Phase 7 and
# Phase 8 both destroyed their own evidence by restarting pods to disarm a
# fault before reading the logs that proved the fault had fired -- the same
# mistake twice, and in Phase 8 it cost a whole scenario attempt. A container
# that is replaced takes its logs with it, and `--previous` then points at the
# wrong container.
#
# Everything is written per-file as it is produced, so an interrupted
# collection still leaves usable evidence on disk.
source "$(dirname "$0")/_common.sh"

LABEL="${1:-}"
if [ -z "$LABEL" ]; then
  echo "usage: $0 <label>    e.g. $0 tier1-baseline" >&2
  exit 1
fi

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT="${ENGINE_DIR}/../docs/results/cloud/${STAMP}-${LABEL}"
mkdir -p "$OUT"
echo "collecting into $OUT"

cat > "$OUT/00-provenance.txt" <<PROV
durexec AWS evidence bundle
label:       $LABEL
collected:   $(date -Is)
instance:    $(tf_output instance_id)
public ip:   $(tf_output public_ip)
ami:         $(tf_output ami_id)
az:          $(tf_output availability_zone)
data volume: $(tf_output data_volume_id)
local HEAD:  $(git -C "$ENGINE_DIR" rev-parse HEAD)
local dirty: $([ -n "$(git -C "$ENGINE_DIR" status --porcelain)" ] && echo yes || echo no)
PROV
echo "  00-provenance.txt"

# Container logs first, because they are the thing a restart destroys.
for c in node-a node-b node-c durexec-postgres-leader-1 durexec-postgres-standby-1 extsim; do
  remote "docker logs --timestamps $c 2>&1 | tail -4000" > "$OUT/10-logs-$c.txt" 2>/dev/null \
    && echo "  10-logs-$c.txt" \
    || echo "  (no container $c)"
done

remote 'cd /opt/durexec/deploy/aws && set -a && . /opt/durexec/.env && set +a && docker compose -f docker-compose.cloud.yml ps --format json' \
  > "$OUT/20-compose-ps.json" 2>/dev/null && echo "  20-compose-ps.json"

remote 'docker stats --no-stream --format "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}"' \
  > "$OUT/21-docker-stats.txt" 2>/dev/null && echo "  21-docker-stats.txt"

remote 'echo "--- free ---"; free -m; echo "--- df ---"; df -h; echo "--- uptime ---"; uptime; echo "--- dmesg oom ---"; sudo dmesg 2>/dev/null | grep -i -E "oom|killed process" | tail -20 || echo "(none)"' \
  > "$OUT/22-host.txt" 2>/dev/null && echo "  22-host.txt"

# The journal invariants, as numbers rather than prose. These are the same
# four checks every phase since 6 has ended with.
remote bash -s > "$OUT/30-invariants.txt" 2>&1 <<'REMOTE' && echo "  30-invariants.txt"
set -a; . /opt/durexec/.env; set +a
q() { docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 psql -U durexec -d durexec "$@"; }
echo "=== status breakdown ==="
q -c "select status, count(*) from executions group by status order by status"
echo "=== non-terminal (RUNNING or COMPENSATING) ==="
q -At -c "select count(*) from executions where status not in ('COMPLETED','FAILED','COMPENSATING_FAILED')"
echo "=== outstanding tasks (a completed task is DELETED, so any row is work owed) ==="
q -At -c "select count(*) from tasks"
echo "=== pending timers ==="
q -At -c "select count(*) from timers"
echo "=== duplicate (run_id, seq) journal records -- must be 0 ==="
q -At -c "select count(*) from (select run_id, seq from events group by run_id, seq having count(*)>1) d"
echo "=== shard leases ==="
q -c "select shard_id, owner_node, fencing_token, expires_at < now() as expired from shard_leases order by shard_id"
echo "=== event counts by type ==="
q -c "select event_type, count(*) from events group by event_type order by count(*) desc limit 20"
echo "=== replication ==="
q -c "select client_addr, state, sync_state, sent_lsn, replay_lsn from pg_stat_replication"
echo "=== durability settings (Phase 8 recorded these as gaps; re-check in cloud) ==="
q -c "select name, setting from pg_settings where name in
  ('fsync','synchronous_commit','full_page_writes','archive_mode','wal_level',
   'data_checksums','wal_log_hints','synchronous_standby_names','wal_keep_size')
  order by name"
REMOTE

# The external-effect ledger, if the simulator is running. It is the only
# record of how many times an effect REALLY happened; a unique journal row is
# not evidence of that.
remote 'docker exec extsim /extsimd -h 2>/dev/null; curl -sS -m 5 http://localhost:8090/counters 2>/dev/null || docker run --rm --network container:extsim curlimages/curl:latest -sS -m 5 http://localhost:8090/counters 2>/dev/null' \
  > "$OUT/40-extsim-counters.txt" 2>/dev/null || echo "  (extsim not running; no ledger)"
if [ -s "$OUT/40-extsim-counters.txt" ]; then echo "  40-extsim-counters.txt"; else rm -f "$OUT/40-extsim-counters.txt"; fi

# Prometheus metrics, raw. Kept as the exposition format rather than queried,
# so the numbers can be re-derived later from the file on disk.
for port in 8081 8082 8083; do
  remote "curl -sS -m 10 http://localhost:$port/metrics" > "$OUT/50-metrics-$port.prom" 2>/dev/null \
    && echo "  50-metrics-$port.prom"
done

cat >> "$OUT/00-provenance.txt" <<PROV

files collected: $(find "$OUT" -type f | wc -l)
PROV

echo
echo "bundle: $OUT"
du -sh "$OUT"
