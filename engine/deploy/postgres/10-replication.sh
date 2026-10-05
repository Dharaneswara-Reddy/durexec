#!/bin/bash
# Runs once, on an empty data directory, before the leader accepts outside
# connections. It does the two things streaming replication needs and that the
# stock image does not do for you.
set -euo pipefail

: "${REPL_USER:?REPL_USER must be set}"
: "${REPL_PASSWORD:?REPL_PASSWORD must be set}"
: "${REPL_SLOT:?REPL_SLOT must be set}"

# A dedicated role with REPLICATION and nothing else. The follower never needs
# to read a table, so it is not given the ability to.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-SQL
	CREATE ROLE "$REPL_USER" WITH REPLICATION LOGIN PASSWORD '$REPL_PASSWORD';

	-- A physical slot makes the leader keep WAL the follower has not consumed
	-- yet. Without one, a follower that falls behind during a restart finds the
	-- segments it needs already recycled and has to be rebuilt from scratch.
	SELECT pg_create_physical_replication_slot('$REPL_SLOT');
SQL

# initdb's default pg_hba covers ordinary connections but not the special
# "replication" pseudo-database, so the follower would be refused without this.
echo "host replication $REPL_USER all scram-sha-256" >> "$PGDATA/pg_hba.conf"
