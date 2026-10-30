# Audit 04 — Independent risk review of the five fix commits

Reviewer: independent, adversarial. Remit: find what the five commits got wrong.
Baseline: `71c77ac`. Range reviewed: `git log b599c1f..71c77ac`.
Everything below was produced by reading the code and running it, on an isolated
throwaway PostgreSQL (ports 5497/55494/55495/55497). The demo cluster on 5433
and 5434 was never touched.

---

## 1. Verdict in one table

| Commit | Claim | Verdict |
|---|---|---|
| 8562607 | A workflow start is one transaction | **Fixed** |
| bbb0a9a | Activity inputs are told apart; divergence parks | **Fixed** |
| 2344ee4 | The dedup key is proved against an independent ledger | **Partly fixed** — proved in a test harness only; no shipped activity reads the key |
| bf6ffb1 | The poll is served by an index; saturation is measured honestly | **Partly fixed** — two regressions, one of them a production write-outage on every node boot |
| 71c77ac | CI runs, and the suite proves itself | **Partly fixed** — the file is in the right place, but no run has ever happened and the suite is not yet reliable |

Two regressions were introduced by the five commits. Both are new; neither
existed at `b599c1f`.

- **R-01 (High)** — `tasks_pollable` is rebuilt and dropped on every migration
  pass, i.e. on every node boot. **Fixed in this review** (one line); see §5.
- **R-02 (Critical, test-only)** — `internal/runtime` segfaults when packages run
  in parallel: **8 of 15 observed runs**, not an occasional flake. The suite's own
  inventory reports it as `0 failed`. **Not fixed** — reported with a one-line fix
  for the student to apply. On this evidence the `-race` CI stage will fail more
  often than it passes.

---

## 2. Commit by commit

### 8562607 — start atomicity (F-01): **genuinely fixed**

| Check | Result |
|---|---|
| Any `INSERT INTO executions` outside the fenced transaction? | No. `grep -rn "INSERT INTO executions" --include=*.go` gives exactly one non-test hit: `internal/eventlog/commit.go:201`, inside `CommitDecision` after `verifyToken ... FOR UPDATE` |
| `core.ExecutionStore` read-only? | Yes — `internal/core/ports.go:90-93` is `Get` and `List` only |
| `eventlog.Create` deleted? | Yes — no `func (s *Store) Create` remains in `internal/eventlog/` |
| Order inside the transaction | fencing token → `createExecution` → events → tasks → timers → intents → status → delete task. Admission after the fence, before the log: correct both ways round |
| Stale owner cannot start | `StartWorkflow` checks `TokenFor` first (advisory, saves writes) and `CommitDecision` re-verifies `FOR UPDATE`. `TestStaleOwnerCannotStartAWorkflow` passes |
| `fencing_rejections_total` fires only on a real mismatch | Yes. `verifyToken` returns a wrapped `core.ErrLostOwnership` for a missing lease row and a typed `*FencedError` only for a token mismatch; `FencingCounter` counts via `errors.As(&fenced)`. `TestStartOnAShardWithNoLeaseRowIsRefusedAndNotCountedAsFencing` passes |
| Ambiguous commit | Reported as an error, never success; a retry resolves on the business key. Both retry tests pass |
| Pre-fix orphans cannot be answered "already open" | `startedRun` refuses `!IsTerminal() && LastSeq == 0` with `ErrWorkflowOrphaned` |

The 11 scenarios in `start_atomicity_test.go` were re-run, not trusted: client
timeouts, database stall, connection killed mid-commit, concurrent starts of one
id, stale owner, crash before/inside/after commit. All pass, three times, on a
fresh database, with and without `-race`.

**Reconciler** (`internal/runtime/reconciler.go`) — does it do harm?

| Hazard | Finding |
|---|---|
| Deletes a row that came alive since the scan | Safe. Every condition is re-tested inside `deleteAbandonedStartSQL`; `DeleteAbandonedStart` reports `RowsAffected()==1` so a loser knows it lost |
| Mistakes an ESCALATE hold for an orphan | Safe. `stuckRunningSQL` excludes an `ESCALATE` verdict with no later `__adjudicate` signal |
| Races the start path | Safe. The executions row is invisible until the single start transaction commits |
| Writes an event or sets a status | No. It only deletes a no-history row or enqueues a `WORKFLOW` task. The "never fail a run from out here" discipline holds |
| **Re-drives healthy signal-parked runs** | **F04-03, Medium.** `CmdWaitSignal` journals nothing, its task row is deleted on commit, and it creates no timer. After `Stale` (default 5 min) such a run matches `stuckRunningSQL` exactly and is re-driven — replayed and re-committed — every pass, forever, with a `WARN re-drove a stuck workflow` each time |
| **"Every write goes through the fencing check"** | **F04-05, Low.** False. The redrive calls `e.Tasks.Enqueue` directly — a plain `INSERT` on the main pool with no token. `TokenFor` is checked before the scan, so a lease lost in between lets a node insert a task into a shard it no longer owns. Harmless by replay purity, but the comment overstates it |

### bbb0a9a — input digest and divergence parking (F-03, F-04): **genuinely fixed**

The canonicalisation rules were attacked directly, not read. Probes written and
run against `inputDigest`/`canonicalise` (temporary test file, since removed):

| Probe | Result |
|---|---|
| Key order `{"a":1,"b":2}` vs `{"b":2,"a":1}` | same digest (intended) |
| `1` / `1.0` / `1e2` vs `100` / `0.5` vs `5e-1` | same digest (intended) |
| Array order `[1,2]` vs `[2,1]` | different (intended) |
| Absent vs explicit null `{}` vs `{"a":null}` | different (intended, documented) |
| `"1"` vs `1`, `{"a":1}` vs `{"a":"1"}` | different |
| `["a",1]` vs `{"a":1}`, `{"a:1":2}` vs `{"a":{"1":2}}` | different — no separator confusion |
| Key containing `","` and `":"` | different; the string goes through `json.Marshal`, so separators are escaped |
| `"raw:1"` (valid JSON string) vs `raw:1` (unparseable) | different — the quoted form starts with `"`, so the `raw:` prefix cannot be forged |
| `-0` vs `0` | same digest. Mathematically equal; accepted |
| `10000000000000000001` vs `...002` | different — `big.Rat` is exact, so no float merging |
| Large exponents `1e2000000` … `1e40000000` | **no DoS.** `big.Rat.SetString` refuses them and the value falls back to the `num:` prefix: 0 s, 0 MiB. I went looking for an unbounded allocation here and there is none |

No pair of different-in-meaning inputs produced the same digest, and no pair of
identical-in-meaning inputs produced different digests beyond the two documented
cases (absent-vs-null, `omitempty`). The full signature is
`"activity:" + activityType + "#" + inputDigest` — a collision across activity
types is impossible because the digest is 32 hex characters and cannot contain
`#`.

Divergence parking: `workflow_worker.go:170-200` returns before any commit and
calls `w.release(...)` with jittered backoff. History is genuinely untouched —
`Dispatcher.Execute` returns the error before `newCoroutine()`. Redeploying
compatible code resumes the run, which `versioning_test.go` covers.
Park loop is bounded, not free: `Attempt` is incremented by the poll
(`attempt = t.attempt + 1`), so the backoff ceiling reaches the 60 s cap after
~10 attempts and a parked run then costs one poll every ~30 s indefinitely. That
is bounded but never clears itself, and there is no dead-letter metric to alert
on. **F04-06, Low.**

### 2344ee4 — the external-effect ledger (F-02): **partly fixed**

The strongest claim in the set holds, and it is real evidence:

- `TestAnEffectWhoseResultCommitIsLostIsNotDuplicatedOnReplay` runs the real
  `ActivityWorker.process` against a real `pgxpool`, a real `taskq.Queue`, a real
  `eventlog.Store` and a real `httptest` server, arms `faultinj.BeforeDecisionCommit`
  at the real commit seam, and then asks the **service** how many effects it
  performed. 2 requests, 1 effect, 0 journaled completions, then 1 after
  recovery. The ledger is independent: `internal/extsim` reads no engine table
  and the engine writes nothing into it, and `performed` is append-only and
  separate from the idempotency cache so expiry cannot shorten it.
- The key-expiry test really does assert a **duplicate**: `if n != 2 { t.Fatalf }`.
  It does not quietly pass.

What is **not** fixed, and must be said before an examiner:

- **F04-01, High (honesty).** `grep -rn "InfoFromContext\|DedupKey" app/` returns
  **nothing**, and `extsim` is referenced only from `_test.go` files. No shipped
  activity in `app/delivery` or `app/agent` reads the dedup key or makes an HTTP
  call. F-02's root condition — "every shipped activity was an in-process sleep"
  — is unchanged. The mechanism is now proved to work; it is still not *used* by
  the demo the project presents.
- **F04-02, Medium.** `internal/acceptit` still reports
  `TestAcceptSagaRecoveryOnLiveClusterBlocked` and
  `TestAcceptNoDuplicateExternalEffectBlocked` as `BLOCKED-ON-AGENT-A`, citing
  "needs fault-injection hooks at real transaction boundaries" and "needs the
  external-effect simulator". Both now exist. The acceptance matrix therefore
  understates what is verifiable, on the two claims that matter most.
- **F04-07, Medium.** `BeforeDecisionCommit` is reachable only from Go tests.
  `eventlog.Store.UseFaultInjector` is called from `_test.go` files and from
  nowhere in `cmd/engined`, so the production store's `faults` field is always
  nil. `AfterActivityJournaled` *is* wired in production (`Engine.Faults`, set in
  `main.go:352`). And `runtime.Faults` exposes only `FailStep` over HTTP — no
  `CrashAt` — so no crash point can be armed on a running cluster at all. F-12 is
  half fixed: the seams are called from production code, but one cannot be
  reached in a production build and neither can be armed from outside the process.

### bf6ffb1 — the poll index and the age metric: **partly fixed, two regressions**

The index claim itself is sound. Verified against PostgreSQL 16, exhaustively,
over a grid of `visible_at` × `locked_until` including NULL, past, future,
`-infinity` and `infinity`:

| `visible_at` vs now | `locked_until` | old predicate | `GREATEST(v,l) <= now()` | differs |
|---|---|---|---|---|
| any | NULL / `-infinity` / past | = | = | no |
| any | future / `infinity` | = | = | no |
| past or equal | **exactly now()** | false | **true** | **yes** |

**The claimed equivalence is not exact.** The old predicate was
`locked_until < now()`; the new one is `l <= now()`. A lock whose `locked_until`
equals the polling `now()` to the microsecond is claimable under the new
predicate and was not under the old. This is **not** a safety defect — a lock
granted as `now() + interval` that happens to equal a later `now()` exactly has
in fact expired at that instant, and it cannot hand a *live* lock to a second
worker — but the migration comment says "exactly the predicate the poll already
used" and that sentence is wrong. **F04-04, Low (documentation).**

| Other check | Result |
|---|---|
| Drift guard | `TestPollUsesTheReadyIndex` builds its `EXPLAIN` from the Go `readyAt` constant and asserts `tasks_poll_ready` appears with no `Sort` and no `Seq Scan`. It genuinely catches Go-vs-migration drift. It does **not** EXPLAIN `pollSQL` itself, so a change to `pollSQL`'s shape that left `readyAt` alone would pass. Low |
| FIFO relaxation | Nothing in the repo depends on `task_id` order. Two tasks for one run are serialised by the events primary key. Starvation is impossible: oldest-ready-first is monotone, and a released task is pushed to the back by its own `visible_at` |
| `task_queue_depth` → `task_queue_oldest_ready_seconds` | Grafana (`deploy/grafana/dashboards/durexec.json:193`) was updated to `max by (shard, task_type) (task_queue_oldest_ready_seconds)`. No code reads the old name. **Stale:** `docs/DESIGN_DOCUMENT.html:859` and `docs/results/acceptance-matrix.*` still list `task_queue_depth`. Cosmetic |
| New metric increments | `TestQueueAgeReportsOnlyWhatAWorkerCouldPollNow` passes; the gauge is cleared, not zeroed, on ownership loss |
| Connection budget | Arithmetic correct: per node `20 + 4 = 24`; 3 nodes `72 + 2 + 4 + 2 + 18 = 98` of 100 (fits, 2 spare); 4 nodes `122` (refused). `cmd/engined/pool_test.go:100` asserts the 4-node refusal. The refusal message itemises every term and is accurate. **But** the comment on `maxPoolConns` says "20 rather than 25 is what makes a fourth node fit" — with the 4-connection renewal pool added, a fourth node does **not** fit at `max_connections=100`. The prose contradicts the code's own test. **F04-08, Low** |
| Renewal pool deadlock | None possible. `Extend` is a single `UPDATE` on one row; the decision transaction can *block* it on that row lock but there is no cycle, and `RenewTimeout` bounds the wait |
| Renewal timeout ≠ `ErrLockLost` | Correct. `Extend` returns the wrapped `context.DeadlineExceeded`; `keepClaimed` cancels only on `errors.Is(err, taskq.ErrLockLost)` |

**R-01 / F04-09 — High. The dropped index is rebuilt on every node boot.**

`0001_init.up.sql` created `tasks_pollable`; `0002` drops it. There is no
`schema_migrations` table — `eventlog.Migrate` re-applies every `*.up.sql` on
every boot, by design. So each pass **built** `tasks_pollable` (measured at
13 MB on 160,000 rows by the commit's own notes) under `ACCESS EXCLUSIVE` on
`tasks`, blocking every poll, and then dropped it. Reproduced directly:

```
after 0001: tasks_pkey, tasks_poll_ready, tasks_pollable
after 0002: tasks_pkey, tasks_poll_ready
```

It is also one cause of the test-suite migration deadlock seen under `-p 4`:
`compensation_resume_test.go:102: testdb: apply 0002_poll_ready_index.up.sql: ERROR: deadlock detected (SQLSTATE 40P01)` —
one package's `testdb.Pool(t)` taking `ACCESS EXCLUSIVE` on `tasks` against
another package's in-flight `FOR UPDATE SKIP LOCKED` polls.

**Fixed in this review** for the production cost. See §5. It does **not**
eliminate the deadlock; see F04-14 below, which is the broader cause.

**F04-14 — Medium, test-only. `internal/testdb` re-migrates on every
`Pool(t)` call, in a shared schema, against live pollers.** After the §5 fix the
deadlock moved rather than disappeared:
`integration_test.go:77: testdb: apply 0001_init.up.sql: ERROR: deadlock detected (SQLSTATE 40P01)`
(1 of 10 runs). `CREATE INDEX IF NOT EXISTS` on an index that already exists
still takes a `ShareLock` on the table, which conflicts with the
`RowExclusiveLock` every concurrent `INSERT`/`UPDATE` holds. Because
`testdb.Pool` applies the whole migration set on *every* call while other
packages are polling the same tables, the lock windows are frequent.
Fix: a `sync.Once` so a test binary migrates once, plus a schema per package via
`search_path` — which also fixes F04-11.

### 71c77ac — CI: **partly fixed**

| Check | Result |
|---|---|
| Location | Correct: `.github/workflows/ci.yml` at the repository root. `engine/.github` is gone |
| YAML valid | Yes — parses; 7 jobs: `lint-and-vet`, `unit-tests`, `integration-tests`, `race-tests`, `build-image`, `compose-up`, `fault-injection-smoke` |
| `working-directory` | Correct. `defaults.run.working-directory: engine`; the Go module, `migrations/`, `Dockerfile` and `deploy/` are all under `engine/` |
| PostgreSQL wired so tests run rather than skip | Yes. `pgvector/pgvector:pg16` service, `DUREXEC_TEST_DSN` on 5432, `DUREXEC_REQUIRE_DB=1` (a missing DSN becomes a failure), and `cmd/testinventory -strict` fails the job on an unexpected skip. `psql` is preinstalled on `ubuntu-latest`. `GO_VERSION: "1.25"` matches `go.mod`'s `go 1.25.10` |
| Has a run ever happened? | **No.** `gh api repos/Dharaneswara-Reddy/durexec/actions/runs --jq .total_count` → **0**. The five commits are local; nothing has been pushed. Every claim about CI behaviour is **UNVERIFIED in GitHub Actions** |
| Dead `env` | Top-level `DATABASE_URL` points at port 5433 and is used by no job. Harmless, misleading |
| Stage 7 `docker kill "$owner"` | Depends on `/v1/shards` returning the container name. `deploy/docker-compose.yml` sets `NODE_ID: node-a` with `container_name: node-a`, so it lines up. Not executed here |

**Will CI be flaky? Yes, at `-p 4`, for two reasons — and `-p 2` is the wrong fix
for either.**

1. **R-02 / F04-10 — Critical, test-only. `internal/runtime` segfaults.**
   Reproduced in **8 of 15** parallel runs on a clean checkout of `71c77ac`:

   ```
   panic: runtime error: invalid memory address or nil pointer dereference
   workflow.(*Dispatcher).Execute(0x0, ...)        dispatcher.go:127
   runtime.(*WorkflowWorker).process(...)          workflow_worker.go:165
   runtime.(*WorkflowWorker).Run(...)              workflow_worker.go:96
   runtime.(*WorkerPool).Start.func1()             pool.go:200
   ```

   Cause: the drain/effect harness (`newEffectHarness` → `newDrainHarness`)
   builds an `Engine` with **no `Dispatcher`** and then starts a real
   `WorkerPool`. When the drained activity commits, its decision enqueues the
   normal `WORKFLOW` wake-up task; the pool's `WorkflowWorker` polls it and calls
   `e.Dispatcher.Execute` on a nil receiver. Whether it gets there before the
   test ends is a timing race, which is why it only shows up under load.
   `Engine.codeVersion()` guards a nil `Dispatcher`; `process` does not.

   Worse than the crash: **the suite's own inventory reports it as zero
   failures.** In two of three clean `-race` runs the table read
   `internal/runtime  57  0  0  0` against `93  0  0  0` in the third — 36 tests
   silently absent, `failed=0`, and no "package crashed" line. Only the shell
   exit code revealed it. This is precisely the failure mode the audit was
   commissioned over.

   Fix, one line, in the test harness: set `h.engine.Dispatcher` in
   `newEffectHarness` (a `workflow.NewDispatcher(workflow.NewRegistry())` is
   enough — an unregistered type parks, it does not crash). Optionally also
   harden `WorkflowWorker.process` to park on a nil `Dispatcher` rather than
   segfault. **Not applied** — it is test code and the student should choose.
   `cmd/testinventory` should also fail loudly when a package emits a `fail`
   action with no failing test.

2. **F04-11 — Medium. The test packages' shard-id ranges overlap, in one shared
   `public` schema.** `internal/testdb` puts every package in `public`, so
   packages share tables, and each package picks a random shard and cleans up
   with `DELETE ... WHERE shard_id = $1`:

   | Package | Range |
   |---|---|
   | `eventlog` | 100,000 – 1,099,999 |
   | `taskq` | 200,000 – 1,199,999 |
   | `timers` | 300,000 – 1,299,999 and 400,000 – 1,399,999 |
   | `lease` | 500,000 – 1,499,999 |
   | `runtime` | 500,000 – 599,999 |
   | `acceptit` | 700,000 – 1,699,999 |

   Every range overlaps every other. A collision makes one package delete
   another's rows or poll its tasks. This is the most likely explanation for the
   one `internal/taskq TestPollLocksOneTaskAndThenFindsNothing` failure seen
   under `-race` (a foreign task at the same shard makes the second poll return
   a row instead of `ErrNoTask`), and the new oldest-ready-first ordering makes a
   colliding foreign row *more* likely to be picked.

   **Recommended fix, in priority order:** (a) give each package a disjoint
   shard band (one constant per package, e.g. `eventlog` 1_000_000–1_099_999,
   `taskq` 1_100_000–1_199_999, …); or (b) give each package its own PostgreSQL
   **schema** via `search_path` in `internal/testdb`, which also removes the
   migrator-versus-poller lock contention. **Do not** drop to `-p 2`: it hides
   both defects without fixing either, and it is the parallel run that makes the
   suite worth having.

**Operations, unchanged and still to be stated honestly:**

- **No backup and no point-in-time recovery.** `archive_mode` is nowhere in
  `deploy/`; the only `pg_basebackup` is the standby's initial clone in
  `deploy/postgres/standby-entrypoint.sh`. No WAL archive, no restore drill. F-05
  stands in full.
- **Replication is still asynchronous.** No `synchronous_commit` or
  `synchronous_standby_names` anywhere in `deploy/`. An acknowledged write can be
  lost on promotion. Say the 9–15 s manual failover number first, before an
  examiner asks.

---

## 3. Findings register

| ID | Severity | Confidence | Location | Reproduction | Fix |
|---|---|---|---|---|---|
| F04-09 / R-01 | **High** | Certain | `engine/migrations/0001_init.up.sql` + `0002`, `internal/eventlog/migrate.go` | Apply `0001` then `0002` twice; `tasks_pollable` appears after `0001` every time | **Applied** — removed the `tasks_pollable` create from `0001` (§5) |
| F04-10 / R-02 | **Critical** (test-only) | Certain | `internal/runtime/external_effect_test.go` (`newEffectHarness`), `internal/runtime/workflow_worker.go:165` | `go test -race -p 4 ./internal/runtime/ ./internal/taskq/ ./internal/lease/ ./internal/eventlog/ ./internal/timers/ ./internal/acceptit/` repeatedly; **8 of 15** | Set `Dispatcher` in the harness; optionally park on nil in `process`; make `testinventory` fail on a crashed package |
| F04-14 | Medium (test-only) | Certain | `internal/testdb/testdb.go` `Pool`/`Migrate` | Same command; 1 of 10 after the §5 fix, as `apply 0001_init.up.sql: deadlock detected` | `sync.Once` per test binary, plus a schema per package via `search_path` |
| F04-01 | **High** (honesty) | Certain | `app/delivery/activities.go`, `app/agent/activities.go` | `grep -rn "InfoFromContext\|DedupKey" app/` → nothing | Have one shipped activity read `activity.Info` and call an external service, or state plainly that the ledger evidence is harness-only |
| F04-03 | Medium | High | `internal/eventlog/store.go` `stuckRunningSQL`; `internal/runtime/reconciler.go` | Park a run on `WaitForSignal`, age `updated_at` past `Stale`, run `Reconciler.Once` | Exclude a run whose last event is consumed-signal-less, or require "no event for longer than Stale AND an unresolved blocking command", the way the ESCALATE hold is excluded |
| F04-07 | Medium | Certain | `cmd/engined/main.go`, `internal/eventlog/store.go:50`, `internal/runtime/faults.go` | `grep -rn UseFaultInjector` → tests only | Call `events.UseFaultInjector(faults)` in `main.go`; expose `CrashAt` on the fault endpoint, still gated on `FAULT_INJECTION` |
| F04-02 | Medium | Certain | `internal/acceptit/acceptit_test.go:397,404` | Any suite run prints both as BLOCKED | Implement them against `extsim` + `BeforeDecisionCommit`, both of which now exist |
| F04-11 | Medium | High | `internal/testdb/testdb.go`; the six packages' shard pickers | Table in §2 | Disjoint per-package shard bands, or a schema per package |
| F04-04 | Low | Certain | `migrations/0002_poll_ready_index.up.sql`, `internal/taskq/queue.go` `readyAt` comment | SQL grid in §2 | Reword: "the same predicate except at `locked_until = now()` exactly, where the new one treats the lock as already expired" |
| F04-05 | Low | Certain | `internal/runtime/reconciler.go` (`Tasks.Enqueue`) and its doc comment | Read the comment against the call | Either route the redrive through the committer, or correct the comment |
| F04-06 | Low | Certain | `internal/runtime/workflow_worker.go:170-200` | Park a run; watch it polled every ~30 s forever | A `workflow_parked_total{reason}` counter and an alert; no auto-clear |
| F04-08 | Low | Certain | `cmd/engined/main.go:493-498` comment | `clusterConnBudget(4) = 122 > 100` | Correct the comment: 20 makes a fourth node fit **only** with `max_connections ≥ 122` |
| F04-12 | Low | Certain | `docs/DESIGN_DOCUMENT.html:859`, `docs/results/acceptance-matrix.*`, `.github/workflows/ci.yml` top-level `DATABASE_URL` | grep `task_queue_depth` | Rename in the design document; drop the dead CI `env` |
| F04-13 | Low | Medium | `internal/eventlog/store.go` `deleteAbandonedStartSQL` vs `Engine.Signal` | Signal a 5-min-stale pre-fix orphan at the instant the reconciler deletes it | Legacy rows only (the engine can no longer create one). Accept, or make `Signal` require `RowsAffected()==1` on the executions update |

Also noted, not a finding: `engine/internal/eventlog/commitbench_test.go` is
untracked and was being edited by another process during this review (its line
numbers moved between runs). It is not part of the five commits and was left
alone. One `-race` sweep was contaminated by it and was re-run on a clean
worktree.

---

## 4. What I ran, and what I did not

**Ran.**

| Thing | Result |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt -l .` | Clean |
| `go test -count=1 ./...` with **no** database | 26 packages ok, 0 failures |
| `scripts/test-integration.sh REPEAT=3` — fresh throwaway DB each run, `-p 4` | 1191 passed / 0 failed / 0 skipped / 2 blocked, three times |
| Same, `RACE=1 REPEAT=3`, on a **clean worktree** of `71c77ac` | 0 failed, 0 skipped, **no data races** — but `internal/runtime` showed 57, 57, 93 passed: the crash in F04-10 |
| `go test -race -p 4` over the six database-heavy packages, 15 runs (5 before the §5 fix, 10 after) | **8 segfaults** (F04-10), 2 migration deadlocks (F04-09 on `0002` before the fix, F04-14 on `0001` after), 1 `taskq` poll failure (F04-11) |
| `internal/runtime` alone, `-race` | ok, 7.7 s — it only fails in parallel |
| Old-vs-new poll predicate, exhaustive SQL grid on PostgreSQL 16 | One divergence, at `locked_until = now()` exactly |
| Migration idempotence, before and after the fix | Reproduced the rebuild; confirmed the fix on both a fresh and a previously-migrated database |
| `scripts/test-integration.sh REPEAT=3` **after** the §5 fix | 1191 passed / 0 failed, three times — the change breaks nothing. (The one "unexpected skip" it reports is `TestCommitPathCost` in the untracked `commitbench_test.go`, which is not part of these commits) |
| Adversarial digest probes (collisions, separator confusion, `raw:` forging, big exponents) | No defect found; temporary test file removed |
| Fault injection at the real seams | `BeforeDecisionCommit` exercised by the start-atomicity and external-effect tests; `AfterActivityJournaled` present and wired in production |
| `gh api .../actions/runs` | `total_count: 0` |
| CI YAML parse, job graph, `working-directory`, Go version vs `go.mod` | Consistent |

**Did not run — NOT RUN, and therefore not passing.**

- The GitHub Actions workflow itself. Nothing has been pushed; `total_count` is 0.
  Stages 5, 6 and 7 (`docker build`, `compose-up`, `fault-injection-smoke`) have
  never executed anywhere.
- `make demo`, `make verify`, `make kill-owner`, `make promote-drill`,
  `make check-replication` — all drive the demo cluster on 5433/5434, which was
  out of scope.
- A failover or promotion drill, and anything touching replication lag.
- `govulncheck`, and the F-13 toolchain/base-image findings.
- `internal/bench` as a measurement (it ran as a correctness test only).
- A long-horizon reconciler run, so F04-03's cost is reasoned from the code and
  one forced pass, not observed over hours.

---

## 5. The one change this review made

**File:** `engine/migrations/0001_init.up.sql`
**Change:** deleted the line
`CREATE INDEX IF NOT EXISTS tasks_pollable ON tasks (shard_id, task_type, visible_at, task_id);`
and replaced it with a comment explaining why the poll index lives in `0002`.

**Why.** `0002` drops that index and there is no migration bookkeeping, so every
node boot rebuilt it under `ACCESS EXCLUSIVE` on `tasks` and dropped it again —
a write outage on the hottest table, proportional to queue size, on every
restart of every node. It is also what deadlocked the test suite's migrator
against concurrent pollers.

**What it does not fix.** The test-suite migration deadlock. It removes the real
index build, but `testdb` still re-applies every migration on every `Pool(t)`
call and the no-op `CREATE INDEX IF NOT EXISTS` statements still take a
`ShareLock` on each table. See F04-14.

**Why it is safe.** `0002` keeps `DROP INDEX IF EXISTS tasks_pollable`, so a
database created before this change still has the index removed. Verified both
ways: a previously-migrated database and a from-scratch database both converge
to `tasks_pkey, tasks_poll_ready`, and nothing is built-then-dropped on a repeat
pass.

Nothing else was modified. No commit was made. `docs/results/`, `docs/audit/01`
to `03` and `Report_Template_AIDS_LaTeX/` were not touched.
