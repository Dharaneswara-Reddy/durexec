# Testing: what runs, what skips, what is blocked

**Short version.** `make test` runs with no database and skips the PostgreSQL tests. `make test-integration` runs everything against a throwaway PostgreSQL and fails if a required test is skipped. CI runs the second one.

## Commands

| Command | Database | Use |
|---|---|---|
| `make test` | none | Fast logic check. PostgreSQL tests skip and say why. **Not** evidence they pass. |
| `make test-integration` | throwaway container, port 55433 | Whole suite, packages in parallel, fresh database per run, inventory printed. |
| `REPEAT=3 make test-integration` | same, new container each repeat | Proves nothing depends on an earlier run. |
| `make test-race` | same | Same suite under `-race`. |
| `make test-inventory` | none | Lists what ran and what skipped. |

## What counts as what

| Word | Meaning |
|---|---|
| PASS | Ran. Every assertion held. |
| FAIL | Ran. An assertion did not hold. Fails the build. |
| SKIPPED | Did not run because an optional input is absent. Carries a reason. Not a pass. |
| BLOCKED | Written, cannot run yet because something it needs is not built. Reason starts `BLOCKED-ON-`. Not a pass. |

`DUREXEC_REQUIRE_DB=1` turns "no database" from SKIPPED into FAIL. CI and `make test-integration` set it. `cmd/testinventory -strict` also fails on any other unexpected skip.

## Everything that can silently disable a test

| Switch | Effect | Status |
|---|---|---|
| `DUREXEC_TEST_DSN` unset | PostgreSQL tests skip | Loud when `DUREXEC_REQUIRE_DB=1`. `internal/taskq` reads it directly, so it still skips quietly on its own; the inventory catches it |
| `go test -short` | 2 timing tests in `internal/bench` skip | Optional, allow-listed |
| `deploy/docker-compose.yml` missing | one `cmd/acceptance` test skips | Optional, allow-listed |
| A pair scores differently from expected | one `internal/idem/semantic` test skips | Data-dependent, allow-listed |
| `DUREXEC_ALLOW_DEMO_DB` unset | a DSN on port 5433 or 5434 (the demo cluster) is refused | By design: tests delete rows |

## What the database tests need

Each package builds its own schema (`internal/testdb`, same advisory lock as `eventlog.Migrate`). `internal/taskq` does not, so the script and CI apply the migrations once before any package starts.

## Known limits

- The acceptance assertions in `internal/acceptit` simulate a crash at the database boundary (abandoned lock, stale token, restart). They do **not** kill a real node mid-transaction. That needs fault-injection hooks and an external-effect simulator and is BLOCKED.
- Nothing here measures that a real external effect happened once. Every effect in every test is simulated in-process.
- CI has never run. `gh api repos/.../actions/runs` returned 0 runs on 2026-10-22, because the workflow sat one directory too deep. It is now at `.github/workflows/ci.yml` in the repository root. It has not run yet either; the first push will be its first run.
