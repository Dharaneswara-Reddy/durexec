#!/usr/bin/env bash
# The standby-promotion drill: kill the leader while the cluster is under
# load, promote the standby, repoint the engine nodes at it, and measure what
# that cost. Replication being healthy (see check_replication.sh) says nothing
# about whether the standby can actually take over; this is the test of that.
#
# What it records, honestly, whatever the outcome:
#   - how long promotion itself took, and how long the whole outage was
#   - how much committed data the standby did not have (measured against the
#     dead leader's own data directory, not estimated)
#   - whether any workflow the leader had committed is missing afterwards
#   - every step that needed a human or a script outside the engine
#
# Promotion method: pg_promote() over a normal SQL connection, not
# `pg_ctl promote`. It needs no shell inside the container and no knowledge of
# PGDATA or the postgres OS user, it waits for promotion to finish and says
# whether it did, and it is what an operator or a failover manager would call
# from outside. pg_ctl promote does the same thing underneath.
#
# Afterwards (unless RESTORE=0) the cluster is put back the way `make up`
# leaves it: the old leader is primary again, the standby is rebuilt from it
# and replicating, and the nodes point at the leader. The workflow data
# written during the drill on the promoted standby is discarded by that
# rebuild; it is test data. See `make promote-drill`.
set -euo pipefail

ENGINE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ENGINE_DIR"

COMPOSE_FILE="${COMPOSE_FILE:-deploy/docker-compose.yml}"
PROMOTED_OVERLAY="${PROMOTED_OVERLAY:-deploy/docker-compose.promoted.yml}"
COMPOSE=(docker compose -f "$COMPOSE_FILE")
COMPOSE_PROMOTED=(docker compose -f "$COMPOSE_FILE" -f "$PROMOTED_OVERLAY")

LEADER_URL="${LEADER_URL:-postgres://durexec:durexec@localhost:5433/durexec?sslmode=disable}"
STANDBY_URL="${STANDBY_URL:-postgres://durexec:durexec@localhost:5434/durexec?sslmode=disable}"
LEADER_CTR="${LEADER_CTR:-deploy-postgres-leader-1}"
STANDBY_CTR="${STANDBY_CTR:-deploy-postgres-standby-1}"
NODE_PORTS=(8081 8082 8083)
NODES=(node-a node-b node-c)

# Load shape. Duration is long enough that the kill lands in the middle of it.
LOAD_DURATION="${LOAD_DURATION:-60s}"
LOAD_CONCURRENCY="${LOAD_CONCURRENCY:-32}"
LOAD_SHARDS="${LOAD_SHARDS:-4}"
KILL_AFTER_SECONDS="${KILL_AFTER_SECONDS:-15}"
SHARD_WAIT_SECONDS="${SHARD_WAIT_SECONDS:-120}"
RESTORE="${RESTORE:-1}"

PREFIX="drill-$(date +%s)"
WORK="$(mktemp -d)"
LOADGEN_BIN="$WORK/loadgen"
LOADGEN_LOG="$WORK/loadgen.log"
LOADGEN_PID=""

now() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }
say() { echo "promote_drill: $*"; }
sql() { psql "$1" -v ON_ERROR_STOP=1 -t -A -c "$2"; }

MANUAL_STEPS=()
manual() { MANUAL_STEPS+=("$1"); }

cleanup() {
	[ -n "$LOADGEN_PID" ] && kill -INT "$LOADGEN_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

wait_nodes_healthy() {
	local deadline=$(($(date +%s) + SHARD_WAIT_SECONDS)) p
	for p in "${NODE_PORTS[@]}"; do
		until curl -sf "http://localhost:$p/healthz" >/dev/null 2>&1; do
			[ "$(date +%s)" -lt "$deadline" ] || { say "node on :$p never became healthy" >&2; return 1; }
			sleep 1
		done
	done
}

# Every shard has a live owner according to the nodes themselves.
wait_shards_owned() {
	local deadline=$(($(date +%s) + SHARD_WAIT_SECONDS)) owned
	while :; do
		owned="$(curl -sf http://localhost:8081/v1/shards 2>/dev/null |
			jq '[.shards[] | select(.owner != null and .owner != "")] | length' 2>/dev/null || echo 0)"
		[ "${owned:-0}" = "16" ] && return 0
		[ "$(date +%s)" -lt "$deadline" ] || { say "only ${owned:-0}/16 shards owned after ${SHARD_WAIT_SECONDS}s" >&2; return 1; }
		sleep 1
	done
}

# ---- preflight --------------------------------------------------------------
say "preflight: nodes healthy and replication streaming"
wait_nodes_healthy
state="$(sql "$LEADER_URL" "select state from pg_stat_replication limit 1")"
[ "$state" = "streaming" ] || { say "standby is not streaming (state='$state'); run make up first" >&2; exit 1; }
if [ "$(sql "$STANDBY_URL" "select pg_is_in_recovery()")" != "t" ]; then
	say "the database on :5434 is not a standby; nothing to promote" >&2
	exit 1
fi

# ---- (a) pre-state ----------------------------------------------------------
say "== pre-state =="
docker exec "$LEADER_CTR" psql -U durexec -d durexec -c \
	"select application_name, state, sync_state, replay_lag from pg_stat_replication;"
PRE_L_EVENTS="$(sql "$LEADER_URL" "select count(*) from events")"
PRE_L_EXEC="$(sql "$LEADER_URL" "select count(*) from executions")"
PRE_S_EVENTS="$(sql "$STANDBY_URL" "select count(*) from events")"
PRE_S_EXEC="$(sql "$STANDBY_URL" "select count(*) from executions")"
say "leader : events=$PRE_L_EVENTS executions=$PRE_L_EXEC"
say "standby: events=$PRE_S_EVENTS executions=$PRE_S_EXEC"

# ---- (b) load ---------------------------------------------------------------
# The same generator the scaling and duplicate runs used (cmd/loadgen), built
# once so compile time does not delay the start of load.
say "== starting load: prefix $PREFIX, $LOAD_SHARDS shards, concurrency $LOAD_CONCURRENCY, $LOAD_DURATION =="
go build -o "$LOADGEN_BIN" ./cmd/loadgen
"$LOADGEN_BIN" -mode throughput -shards "$LOAD_SHARDS" -concurrency "$LOAD_CONCURRENCY" \
	-duration "$LOAD_DURATION" -warmup 2s -grace 5s -id-prefix "$PREFIX" >"$LOADGEN_LOG" 2>&1 &
LOADGEN_PID=$!
sleep "$KILL_AFTER_SECONDS"
UNDER_LOAD_EVENTS="$(sql "$LEADER_URL" "select count(*) from events where workflow_id like '$PREFIX%'")"
say "after ${KILL_AFTER_SECONDS}s of load the leader holds $UNDER_LOAD_EVENTS events for this run"

# ---- the failure ------------------------------------------------------------
# docker kill is SIGKILL: no shutdown checkpoint and, crucially, no chance for
# the walsender to flush the last WAL to the standby, which is what a clean
# `docker stop` would give for free and a real crash does not. The restart
# policy is switched off first or docker would bring the leader straight back
# and the standby would have nothing to take over from.
say "== killing the leader (SIGKILL) =="
docker update --restart=no "$LEADER_CTR" >/dev/null
T_KILL="$(now)"
docker kill "$LEADER_CTR" >/dev/null

# ---- (c) promote ------------------------------------------------------------
say "== promoting the standby with pg_promote() =="
REPLAY_LSN="$(sql "$STANDBY_URL" "select pg_last_wal_replay_lsn()")"
T_PROMO_START="$(now)"
PROMO_OK="$(sql "$STANDBY_URL" "select pg_promote(true, 60)")"
T_PROMOTED="$(now)"
IN_RECOVERY="$(sql "$STANDBY_URL" "select pg_is_in_recovery()")"
say "pg_promote returned $PROMO_OK; pg_is_in_recovery=$IN_RECOVERY"
if [ "$PROMO_OK" != "t" ] || [ "$IN_RECOVERY" != "f" ]; then
	say "PROMOTION FAILED" >&2
	exit 1
fi
# Snapshot what the promoted database holds before anything else writes to it.
PROMO_EVENTS="$(sql "$STANDBY_URL" "select count(*) from events")"
PROMO_EXEC="$(sql "$STANDBY_URL" "select count(*) from executions")"
PROMO_RUN_EXEC="$(sql "$STANDBY_URL" "select count(*) from executions where workflow_id like '$PREFIX%'")"
# Per-execution position, frozen now: once the nodes are back they finish the
# in-flight workflows and these numbers move, which would look like a diff.
sql "$STANDBY_URL" "select shard_id||'|'||workflow_id||'|'||run_id||'|'||last_seq from executions where workflow_id like '$PREFIX%'" | sort >"$WORK/standby_seq"
say "promoted database holds events=$PROMO_EVENTS executions=$PROMO_EXEC (run executions=$PROMO_RUN_EXEC)"

# ---- (d) repoint ------------------------------------------------------------
# The nodes were still pointed at the dead leader and, with it gone, could not
# renew their leases. They have to be recreated with a new DATABASE_URL: the
# connection string is fixed at container creation, and the engine has no
# reload. The node containers must be stopped BEFORE the old leader is started
# again below, or they would reconnect to it by name.
say "== repointing the engine nodes at postgres-standby =="
manual "docker update --restart=no on the leader and docker kill (the simulated failure itself)"
manual "pg_promote() on the standby (no automatic failover exists)"
manual "recreate node-a/b/c with DATABASE_URL pointed at the standby (deploy/docker-compose.promoted.yml)"
kill -INT "$LOADGEN_PID" 2>/dev/null || true
wait "$LOADGEN_PID" 2>/dev/null || true
LOADGEN_PID=""
"${COMPOSE[@]}" stop "${NODES[@]}" >/dev/null 2>&1 || true
T_REPOINT_START="$(now)"
"${COMPOSE_PROMOTED[@]}" up -d --no-build --no-deps --force-recreate "${NODES[@]}" >/dev/null 2>&1
wait_nodes_healthy
T_NODES_UP="$(now)"
say "nodes healthy again; waiting for all 16 shards to have an owner"
wait_shards_owned
T_SHARDS="$(now)"

# ---- (e) verify the engine on the promoted database -------------------------
say "== verifying the engine against the promoted database =="
VERIFY_ID="promo-verify-$(date +%s)"
VERIFY_STATUS="not-run"
http_code=""
for _ in $(seq 1 15); do
	http_code="$(curl -s -o "$WORK/start.json" -w '%{http_code}' -X POST http://localhost:8081/v1/workflows \
		-H 'Content-Type: application/json' \
		-d "{\"workflow_id\":\"$VERIFY_ID\",\"workflow_type\":\"DeliveryOrder\",\"input\":{\"order_id\":\"$VERIFY_ID\",\"customer_id\":\"cust-drill\",\"restaurant_id\":\"rest-drill\",\"items\":[{\"sku\":\"SKU-42\",\"quantity\":1}],\"amount_cents\":1999,\"currency\":\"USD\",\"prep_time_ns\":1000000000}}")"
	case "$http_code" in
		2??) break ;;
		409) # NotOwner: follow it to the owner the way `make demo` does.
			owner="$(jq -r '.current_owner // empty' "$WORK/start.json")"
			[ -n "$owner" ] && VERIFY_BASE_PORT="$(case "$owner" in node-a) echo 8081 ;; node-b) echo 8082 ;; node-c) echo 8083 ;; esac)"
			sleep 1 ;;
		*) sleep 1 ;;
	esac
done
T_ACCEPTED="$(now)"
VERIFY_BASE="http://localhost:${VERIFY_BASE_PORT:-8081}"
if [[ "$http_code" == 2* ]]; then
	for _ in $(seq 1 45); do
		code="$(curl -s -o "$WORK/status.json" -w '%{http_code}' "$VERIFY_BASE/v1/workflows/$VERIFY_ID")"
		if [ "$code" = "409" ]; then
			o="$(jq -r '.current_owner // empty' "$WORK/status.json")"
			case "$o" in node-a) VERIFY_BASE=http://localhost:8081 ;; node-b) VERIFY_BASE=http://localhost:8082 ;; node-c) VERIFY_BASE=http://localhost:8083 ;; esac
		fi
		VERIFY_STATUS="$(jq -r '.status // "unknown"' "$WORK/status.json" 2>/dev/null || echo unknown)"
		case "$VERIFY_STATUS" in COMPLETED | FAILED | COMPENSATING_FAILED) break ;; esac
		sleep 1
	done
else
	VERIFY_STATUS="start-rejected-HTTP-$http_code"
fi
T_TERMINAL="$(now)"
say "new workflow $VERIFY_ID -> $VERIFY_STATUS"

# Previously written history: a workflow that existed before the failure must
# still be readable through the engine's API, not just present in a table.
OLD_ID="$(sql "$STANDBY_URL" "select workflow_id from executions where workflow_id like '$PREFIX%' and status='COMPLETED' order by workflow_id limit 1")"
OLD_HIST="unreadable"
OLD_ID_NOTE=""
if [ -n "$OLD_ID" ]; then
	for port in "${NODE_PORTS[@]}"; do
		code="$(curl -s -o "$WORK/old.json" -w '%{http_code}' "http://localhost:$port/v1/workflows/$OLD_ID")"
		[ "$code" = "200" ] && { OLD_HIST="readable (status=$(jq -r .status "$WORK/old.json"))"; break; }
	done
	OLD_ID_NOTE="$OLD_ID"
else
	OLD_HIST="no completed pre-failure workflow existed to read"
fi
say "pre-failure workflow ${OLD_ID_NOTE:-n/a}: $OLD_HIST"

DUP_RESULT="clean"
DATABASE_URL="$STANDBY_URL" ./deploy/assert_no_duplicates.sh >/dev/null 2>&1 || DUP_RESULT="DUPLICATES FOUND"
say "duplicate-event assertion on the promoted database: $DUP_RESULT"

# ---- (f) measure what was lost ----------------------------------------------
# The ground truth is the dead leader's own data directory. Starting it is safe
# now: the nodes point at the standby, and it is only read.
say "== measuring loss against the old leader's own data =="
docker start "$LEADER_CTR" >/dev/null
until docker exec "$LEADER_CTR" pg_isready -U durexec -d durexec -q 2>/dev/null; do sleep 1; done
OLD_EVENTS="$(sql "$LEADER_URL" "select count(*) from events")"
OLD_EXEC="$(sql "$LEADER_URL" "select count(*) from executions")"
OLD_RUN_EXEC="$(sql "$LEADER_URL" "select count(*) from executions where workflow_id like '$PREFIX%'")"
OLD_END_LSN="$(sql "$LEADER_URL" "select pg_current_wal_lsn()")"
# Not all of this is data: the leader writes recovery/checkpoint records of its
# own while coming back up. The event counts below are the real measure.
LOST_WAL_BYTES="$(sql "$LEADER_URL" "select pg_wal_lsn_diff('$OLD_END_LSN', '$REPLAY_LSN')")"
sql "$LEADER_URL" "select shard_id||'|'||workflow_id||'|'||run_id||'|'||last_seq from executions where workflow_id like '$PREFIX%'" | sort >"$WORK/leader_seq"
cut -d'|' -f1-3 "$WORK/leader_seq" | sort >"$WORK/leader_keys"
cut -d'|' -f1-3 "$WORK/standby_seq" | sort >"$WORK/standby_keys"
LOST_WORKFLOWS="$(comm -23 "$WORK/leader_keys" "$WORK/standby_keys" | wc -l)"
LOST_EVENTS=$((OLD_EVENTS - PROMO_EVENTS))
# A workflow present on both sides but at a lower seq on the promoted side is a
# partially lost history, which is worse than a missing one.
TRUNC="$(comm -23 "$WORK/leader_seq" "$WORK/standby_seq" | cut -d'|' -f1-3 | sort -u | comm -12 - "$WORK/standby_keys" | wc -l)"

# ---- the report -------------------------------------------------------------
echo
echo "================ standby promotion drill ================"
echo "run prefix                        $PREFIX"
echo "load                              $LOAD_SHARDS shards, concurrency $LOAD_CONCURRENCY, killed ${KILL_AFTER_SECONDS}s in"
echo
echo "TIMING"
echo "  pg_promote() call               $(elapsed "$T_PROMO_START" "$T_PROMOTED") s"
echo "  kill -> promoted (writable)     $(elapsed "$T_KILL" "$T_PROMOTED") s"
echo "  promoted -> nodes healthy       $(elapsed "$T_PROMOTED" "$T_NODES_UP") s   (includes stopping the old load and recreating 3 containers)"
echo "  nodes healthy -> 16/16 owned    $(elapsed "$T_NODES_UP" "$T_SHARDS") s"
echo "  kill -> new workflow accepted   $(elapsed "$T_KILL" "$T_ACCEPTED") s   (HTTP $http_code)"
echo "  kill -> new workflow terminal   $(elapsed "$T_KILL" "$T_TERMINAL") s   ($VERIFY_STATUS)"
echo
echo "DATA"
echo "  standby replay LSN at kill      $REPLAY_LSN"
echo "  old leader LSN after recovery   $OLD_END_LSN"
echo "  WAL the standby never received  $LOST_WAL_BYTES bytes"
echo "  events      old leader / promoted   $OLD_EVENTS / $PROMO_EVENTS   (lost: $LOST_EVENTS)"
echo "  executions  old leader / promoted   $OLD_EXEC / $PROMO_EXEC"
echo "  this run's executions old leader / promoted   $OLD_RUN_EXEC / $PROMO_RUN_EXEC"
echo "  workflows committed on the leader but missing on promoted   $LOST_WORKFLOWS"
echo "  workflows present on both but behind on promoted (history truncated)   $TRUNC"
echo
echo "ENGINE ON THE PROMOTED DATABASE"
echo "  new workflow                    $VERIFY_ID -> $VERIFY_STATUS"
echo "  pre-failure history             $OLD_HIST"
echo "  duplicate-event assertion       $DUP_RESULT"
echo
echo "MANUAL STEPS REQUIRED"
for s in "${MANUAL_STEPS[@]}"; do echo "  - $s"; done
echo
echo "LOADGEN OUTPUT (interrupted at promotion, so it reports what it managed to measure)"
sed 's/^/  | /' "$LOADGEN_LOG"
echo "========================================================="

# ---- restore ----------------------------------------------------------------
if [ "$RESTORE" != "1" ]; then
	say "RESTORE=0: leaving the cluster promoted. Nodes point at the standby; the old leader is running read-only-by-convention and DIVERGED from it."
	exit 0
fi
say "== restoring the cluster: old leader primary, standby re-cloned from it =="
# The promoted standby has its own timeline and cannot rejoin the old leader;
# the only supported way back is to discard it and clone again. The leader
# keeps the replication slot, so the clone attaches to the same slot.
"${COMPOSE[@]}" stop "${NODES[@]}" >/dev/null 2>&1 || true
docker rm -f "$STANDBY_CTR" >/dev/null
standby_vol="$(docker volume ls -q | grep -E '(^|_)pg-standby-data$' | head -1)"
docker volume rm "$standby_vol" >/dev/null
docker update --restart=unless-stopped "$LEADER_CTR" >/dev/null
"${COMPOSE[@]}" up -d --no-build --wait >/dev/null 2>&1
wait_nodes_healthy
wait_shards_owned
for _ in $(seq 1 60); do
	RESTORED_STATE="$(sql "$LEADER_URL" "select state from pg_stat_replication limit 1" 2>/dev/null || true)"
	[ "$RESTORED_STATE" = "streaming" ] && break
	sleep 1
done
say "restored: replication state=$RESTORED_STATE, nodes healthy, 16/16 shards owned"
[ "$RESTORED_STATE" = "streaming" ] || { say "RESTORE INCOMPLETE: standby not streaming yet" >&2; exit 1; }
