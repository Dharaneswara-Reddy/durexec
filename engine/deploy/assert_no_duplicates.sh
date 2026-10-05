#!/usr/bin/env bash
# Runs deploy/assert_no_duplicates.sql against DATABASE_URL and fails the
# build if it returns even one row. This is the check that turns "we believe
# the engine never double-appends an event" into something a CI run actually
# verified, on that run, against real data — see the fault-injection-smoke
# stage in .github/workflows/ci.yml for why that distinction matters.
set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL must be set, e.g. postgres://durexec:durexec@localhost:5433/durexec?sslmode=disable}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SQL_FILE="$SCRIPT_DIR/assert_no_duplicates.sql"

echo "assert_no_duplicates: checking events for duplicate (shard_id, workflow_id, run_id, seq) tuples..."

# -t -A: tuples-only, unaligned. The pass/fail test is simply "did the query
# produce any output at all" — a formatted table would still print header and
# footer punctuation on zero rows, which would make an empty result look
# non-empty to this check.
rows="$(psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -t -A -F $'\t' -f "$SQL_FILE")"

if [ -n "$rows" ]; then
  echo "assert_no_duplicates: SAFETY VIOLATION — duplicate (shard_id, workflow_id, run_id, seq) tuples in events:" >&2
  echo "----------------------------------------------------------------------" >&2
  # A second, human-formatted run for the CI log; the pass/fail decision
  # above has already been made from the unaligned output, so a failure here
  # cannot mask the violation.
  psql "$DATABASE_URL" -f "$SQL_FILE" >&2 || true
  echo "----------------------------------------------------------------------" >&2
  exit 1
fi

echo "assert_no_duplicates: OK — no duplicate event tuples found."
exit 0
