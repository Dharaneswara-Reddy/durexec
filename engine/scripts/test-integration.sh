#!/usr/bin/env bash
# Run the whole Go test suite against a THROWAWAY PostgreSQL, never the demo
# cluster, with packages in parallel, and print an executed/skipped inventory.
#
#   scripts/test-integration.sh              # one run, fresh database
#   REPEAT=3 scripts/test-integration.sh     # 3 runs, a fresh database each
#   RACE=1  scripts/test-integration.sh      # add the race detector
#   PREMIGRATE=0 PKGS="./internal/lease/... ./internal/timers/... ./internal/eventlog/..." \
#       scripts/test-integration.sh          # prove those packages build their own schema
#
# With DUREXEC_TEST_DSN already set (CI does this, against its postgres service)
# no container is started and the database is used as given.
#
# Exit status is non-zero if any test fails OR any required test is skipped.
set -euo pipefail
cd "$(dirname "$0")/.."

REPEAT="${REPEAT:-1}"
RACE="${RACE:-0}"
PREMIGRATE="${PREMIGRATE:-1}"
PKGS="${PKGS:-./...}"
PARALLEL="${PARALLEL:-4}"
PORT="${TEST_PG_PORT:-55433}"
NAME="durexec-test-pg-$$"
IMAGE="${TEST_PG_IMAGE:-pgvector/pgvector:pg16}"

started=0
cleanup() { [ "$started" = 1 ] && docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

fresh_db() {
	if [ "${EXTERNAL_DSN:-}" ]; then
		# An externally provided database is reset by dropping our schema objects.
		psql "$EXTERNAL_DSN" -v ON_ERROR_STOP=1 -q -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null
		return
	fi
	cleanup; started=0
	docker run -d --rm --name "$NAME" -e POSTGRES_USER=durexec -e POSTGRES_PASSWORD=durexec \
		-e POSTGRES_DB=durexec -p "127.0.0.1:${PORT}:5432" --tmpfs /var/lib/postgresql/data \
		"$IMAGE" >/dev/null
	started=1
	for _ in $(seq 1 60); do
		# pg_isready is true during the image's init-time restart, so require a real query
		if psql "$DSN" -qtAc 'select 1' >/dev/null 2>&1 && sleep 1 && psql "$DSN" -qtAc 'select 1' >/dev/null 2>&1; then return; fi
		sleep 1
	done
	echo "throwaway postgres did not become ready" >&2; exit 2
}

if [ -n "${DUREXEC_TEST_DSN:-}" ]; then
	DSN="$DUREXEC_TEST_DSN"; EXTERNAL_DSN="$DSN"
else
	DSN="postgres://durexec:durexec@127.0.0.1:${PORT}/durexec?sslmode=disable"
fi
export DUREXEC_TEST_DSN="$DSN" DUREXEC_REQUIRE_DB=1
# the throwaway port is not the demo's 5433/5434, so the demo-DB guard stays on

status=0
for i in $(seq 1 "$REPEAT"); do
	echo "=== run $i of $REPEAT: fresh database, packages in parallel (-p $PARALLEL), race=$RACE, premigrate=$PREMIGRATE"
	fresh_db
	if [ "$PREMIGRATE" = 1 ]; then
		# Explicit one-off schema setup before any package starts, for packages
		# (taskq, runtime) whose own tests do not create it.
		for f in migrations/*.up.sql; do psql "$DSN" -v ON_ERROR_STOP=1 -q -f "$f" >/dev/null; done
	fi
	flags=(-count=1 -p "$PARALLEL" -timeout "${TEST_TIMEOUT:-4m}")
	[ "$RACE" = 1 ] && flags+=(-race)
	# shellcheck disable=SC2086
	if ! go test -json "${flags[@]}" $PKGS | go run ./cmd/testinventory -strict; then status=1; fi
done
exit $status
