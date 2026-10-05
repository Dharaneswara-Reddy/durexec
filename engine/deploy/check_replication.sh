#!/usr/bin/env bash
# Proves the standby is actually replicating, rather than merely reporting
# "healthy". A container healthcheck only proves postgres is accepting
# connections — it says nothing about whether WAL from the leader is really
# being applied. This script writes a one-off sentinel row on the leader,
# waits briefly, and reads it back from the standby; if the row never shows
# up there, replication is not working, whatever docker compose ps says.
#
# This is what makes the FR-9 ("the standby is a real, verified replica")
# claim demonstrable rather than assumed. See `make check-replication`.
set -euo pipefail

LEADER_URL="${LEADER_URL:-postgres://durexec:durexec@localhost:5433/durexec?sslmode=disable}"
STANDBY_URL="${STANDBY_URL:-postgres://durexec:durexec@localhost:5434/durexec?sslmode=disable}"

# How long to keep polling the standby before giving up. Streaming
# replication with no load behind it lands in well under a second; this
# budget is generous enough to absorb a slow host without masking a genuine
# failure to replicate.
ATTEMPTS="${ATTEMPTS:-15}"
SLEEP_SECONDS="${SLEEP_SECONDS:-1}"

sentinel="replication-check-$(date +%s)-$$-${RANDOM}"

echo "check_replication: ensuring the sentinel table exists on the leader..."
psql "$LEADER_URL" -v ON_ERROR_STOP=1 -q -c \
	"CREATE TABLE IF NOT EXISTS replication_check (id text PRIMARY KEY, written_at timestamptz NOT NULL DEFAULT now());"

echo "check_replication: writing sentinel row '$sentinel' on the leader..."
psql "$LEADER_URL" -v ON_ERROR_STOP=1 -q -c \
	"INSERT INTO replication_check (id) VALUES ('$sentinel');"

echo "check_replication: waiting for '$sentinel' to reach the standby (up to $((ATTEMPTS * SLEEP_SECONDS))s)..."
for attempt in $(seq 1 "$ATTEMPTS"); do
	# The standby is read-only (it is following WAL, not accepting writes), so
	# this is a plain SELECT. A connection failure here just means the standby
	# is not up yet — swallow it and keep polling rather than aborting on the
	# first attempt; ON_ERROR_STOP would otherwise kill the whole script on a
	# transient "connection refused" during startup.
	found="$(psql "$STANDBY_URL" -t -A -c \
		"SELECT 1 FROM replication_check WHERE id = '$sentinel';" 2>/dev/null || true)"
	if [ "$found" = "1" ]; then
		echo "check_replication: OK — sentinel row reached the standby after attempt $attempt/$ATTEMPTS."
		exit 0
	fi
	sleep "$SLEEP_SECONDS"
done

echo "check_replication: FAILED — '$sentinel' never appeared on the standby after $ATTEMPTS attempts (${SLEEP_SECONDS}s apart)." >&2
echo "check_replication: the standby container may be reporting healthy while streaming replication is not actually delivering data." >&2
exit 1
