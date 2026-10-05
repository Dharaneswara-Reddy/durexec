#!/bin/bash
# Entrypoint for a streaming replica.
#
# On first boot the data directory is empty, so the replica copies the leader
# byte for byte with pg_basebackup and then follows its write-ahead log. On
# every later boot the directory already exists and it just starts and catches
# up, which is why this script is safe to re-run.
set -euo pipefail

: "${PRIMARY_HOST:?PRIMARY_HOST must be set}"
: "${REPL_USER:?REPL_USER must be set}"
: "${REPL_PASSWORD:?REPL_PASSWORD must be set}"
: "${REPL_SLOT:?REPL_SLOT must be set}"

PGDATA="${PGDATA:-/var/lib/postgresql/data}"

if [ ! -s "$PGDATA/PG_VERSION" ]; then
	echo "standby: no data directory yet, cloning from $PRIMARY_HOST"

	# The leader's own healthcheck can pass before it has finished creating the
	# replication role, so wait for the role rather than for the port.
	until PGPASSWORD="$REPL_PASSWORD" pg_isready -h "$PRIMARY_HOST" -U "$REPL_USER" -q; do
		echo "standby: waiting for $PRIMARY_HOST"
		sleep 2
	done

	mkdir -p "$PGDATA"
	rm -rf "${PGDATA:?}"/*
	chown postgres:postgres "$PGDATA"
	chmod 0700 "$PGDATA"

	# PGPASSWORD has to be exported (not just prefixed onto the pg_isready
	# call above) so it is still in the environment for pg_basebackup below.
	# `VAR=val cmd` only sets VAR for that one command; without this export,
	# pg_basebackup ran with no password at all, scram-sha-256 auth on the
	# leader rejected it with "fe_sendauth: no password supplied", `set -e`
	# killed the script, and the container restarted into the same failure
	# forever — which is why the standby never left "unhealthy": postgres
	# itself never finished starting, so pg_isready had nothing to answer.
	export PGPASSWORD="$REPL_PASSWORD"

	# -R writes standby.signal and the primary_conninfo line, so the copy comes
	# back up already configured to follow. -Xs streams WAL during the copy,
	# which is what keeps a long clone from failing at the end.
	gosu postgres pg_basebackup \
		--host="$PRIMARY_HOST" \
		--username="$REPL_USER" \
		--pgdata="$PGDATA" \
		--wal-method=stream \
		--slot="$REPL_SLOT" \
		--write-recovery-conf \
		--checkpoint=fast \
		--progress \
		--verbose
	unset PGPASSWORD
	echo "standby: clone complete"
else
	echo "standby: existing data directory, following $PRIMARY_HOST"
fi

# Hand over to the stock entrypoint, which handles ownership and signals.
exec docker-entrypoint.sh postgres
