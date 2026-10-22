# Audit 03: test credibility, security, operations

Agent 3 of 3. Commit under audit: `3052fa7`. Audit run: 2026-10-21/22, local cluster. Running notes written during the audit are kept in `docs/audit/evidence/03-running-notes.md`.

## 1. Executive summary

**Verdict for my remit: NO-GO for production. The safety evidence is real but narrow, and one core guarantee is broken.**

| | |
|---|---|
| Most serious finding | **F-01 (Critical, reproduced three ways).** Starting a workflow is not atomic. A client timeout, a disconnect, or a database stall leaves a workflow that is reported as open and never runs. A retry gets HTTP 200 `already_open`. Nothing detects or repairs it. |
| Why the project's own tests missed it | They count a workflow as "accepted" only when HTTP 201 came back and the run finished. They never ask what happened to a request the client gave up on. |
| Biggest security exposure | The HTTP API has no authentication, no tenancy and no rate limit. `POST /v1/admin/fail-step` is live on the demo cluster, needs no credentials, and arms a failure for an activity type on a whole node, not for one workflow. Every service port is bound to `0.0.0.0`. The engine connects to Postgres as a superuser with the password `durexec`. |
| Biggest operational gap | An operator cannot tell that something is wrong. There are 10 metrics, no alert rules, no Alertmanager, no stuck-workflow signal, no replication-lag signal, no backup and no point-in-time recovery. `/healthz` stayed 200 for the whole of a 20 s database outage. |
| Claim not evidenced | "Zero steps executed twice." What was measured is "zero steps *journaled* twice". Every external effect in every test is simulated in-process. The project's own matrix says so (NFR-3), but the README does not. |

What the evidence does support (and I re-ran): across 1,000 workflows under node kills and a database stall, **0 duplicate `ActivityTaskCompleted` at the same `k`**, 0 accepted-and-stuck workflows with history, race detector clean 3x, 30 concurrent starts of one id produced exactly one run.

What I could not do: crash a node at a chosen transaction boundary (the crash points in `faultinj` are never wired in), run a real external-effect ledger, test a real database failover, test graceful drain with a long activity, or confirm whether CI has ever run.

State at the end: see section 10.

## 2. Test credibility, claim by claim

56 test files, 484 `Test*` functions. **Only 19 touch a real PostgreSQL** (eventlog 5, lease 5, taskq 7, timers 2). Every worker, dispatcher, runtime and failure-path test in `internal/runtime` uses in-memory fakes (`fakes_test.go`). The failure injection in live runs is `docker kill` at a wall-clock offset, not a transaction boundary.

| # | Claim | What was actually measured | Injection point | External effects | Verdict |
|---|---|---|---|---|---|
| C1 | "0 steps executed twice across 3,512 steps, 1,000 workflows under failure" (`duplicates-under-failure.txt`) | Count of `(shard, workflow, run, k)` with two `ActivityTaskCompleted` events, 878 runs. Re-run: 763 runs, 3,052 steps. | `docker kill` node-b at 8 s, node-c 12 s later. Two kills, one batch, one repeat. | **Simulated.** Activities are `sleep` plus an in-process counter (`app/delivery/activities.go`). | **Narrowed.** Holds for *journal* duplicates only. An activity that ran and whose result was lost before the journal write is invisible, as the file's own note admits. I re-ran the same check on 1,000 more workflows: 0. Not evidence about payments. |
| C2 | "0 accepted work lost" | 878 of 1,000 reached COMPLETED. 122 refused (409 NotOwner after 6 tries). | as C1 | n/a | **Defeated by F-01** for any client that times out or retries. The 122 are honestly reported; the problem is the definition of "accepted". In my run r3, 300 of 300 were told success and 17 were orphans. |
| C3 | What happened to the other 122 | They never created a row: execution statuses for the prefix are COMPLETED=878 only. Re-run: 237 refused, COMPLETED=763. | | | **Dropping them does not flatter the safety number** (a refused request cannot duplicate). It does flatter availability: the headline in `BASELINE_SUMMARY.md` and the README is the better of two runs, 87.8% versus 76.3%. |
| C4 | Acceptance matrix, 36 rows | Final-image run: **16 PASS, 17 MANUAL, 3 BLOCKED.** FR+NFR rows: 7 PASS, 16 MANUAL, 3 BLOCKED. | none, the tool "kills nothing" | read-only | **Weaker than it looks.** Every durability, failover, fencing and recovery row (FR-1, 2, 7, 8, 9, NFR-1 to 4) is MANUAL. The PASS rows are mostly wiring checks: FR-14 and C-8 are the same check, FR-10 and C-7 the same, C-1 "cannot fail while the key exists", C-10 is a text search. FR-13 (a suppressed duplicate) and FR-3 (a retried step) are BLOCKED: neither has ever happened on the live cluster. |
| C5 | NFR-1 row "20 RUNNING executions have no queued task and no timer" | A human was asked to decide. | | | **A real defect was hiding here.** 19 of those 20 are F-01 orphans (RUNNING, `last_seq=0`, no event). The 20th is a TravelAgent waiting for a signal. |
| C6 | "Valid" scaling at concurrency 64 and 128 (`scaling-saturated.txt`) | 13.50 / 26.50 / 32.10 wf/s (c=64), 12.80 / 23.15 / 46.30 (c=128) | | | **A second generator-bound point was not caught.** At c=64 the 4-shard point has p50 = 2.008 s. 64 / 2.008 = 31.9, and the measured value is 32.1. That is the same closed-loop trap the c=16 file was kept to warn about. The "valid" 2.00x uses the c=128 point only; the c=64 2-to-4 ratio is 1.21x. Also, 4 shards were owned by 3 nodes, which the project itself calls "not evidence of horizontal scaling" in the failover drill. |
| C7 | "Reproduces the published run twice" (`scaling-saturated-after-liveness-fix.txt`) | 6 sweeps. 3 reproduce. | | | **Post-hoc exclusion.** The rule "ignore sweeps with load above about 4" was written after seeing them. The file also holds sweeps with **0.00 wf/s at 1 shard** (twice: 0 completed of 128 started in 20 s) and sweeps where 2 shards are slower than 1 (7.35 vs 8.85, and 6.60 vs 9.55). They are explained only as "host load". |
| C8 | Failover "9 to 15 s, 0 to 2 events lost" | 3 drills; run 1 not kept. | `SIGKILL` of leader under load | simulated | **Real but n=2 on disk.** Run 1's numbers appear in the README table although its output was overwritten, so they cannot be re-checked. Raw run 3 file was not opened by me; run 2 was. |
| C9 | Semantic idempotency | 213 pairs, generated labels. Live-model run: 75 trials, 51 usable, 45 genuine rewordings. | n/a | simulated tool counter | **Honestly reported, and weak.** Live run measures catch rate only (34 of 45, Wilson 95% interval about 61 to 86%). It has **0 "drift" trials, so the false-suppression rate on real model output is unmeasured.** The 0.939 false-positive rate comes from the synthetic corpus. The engine's own suppression path is tested only with a scripted embedder. |
| C10 | "Replay 200 events in 0.9 ms p99" | In-memory benchmark | none | n/a | **Out of my remit** (Agent 2). Note only that the live cluster has no history longer than 18 events (matrix NFR-6). |
| C11 | CI "turns the safety claim into a guarantee on every push" (`ci.yml` stage 7) | The assertion is `assert_no_duplicates.sql`, a `GROUP BY` on the primary key with `HAVING count(*) > 1`. | | | **Vacuous.** It cannot return a row while the primary key exists. `make verify` runs the same query. |
| C12 | CI runs at all | `ci.yml` lives at `engine/.github/workflows/`. The git root is the parent directory. | | | **Likely never runs.** GitHub reads only the repository root's `.github/workflows`. `gh run list` returned nothing (not authenticated or no runs), so UNVERIFIED either way. |

### Did any other run have an uncaught defect like GENERATOR-BOUND?

Yes: C6 (c=64 4-shard point) and C7 (selective sweeps). I found no second invalid *safety* run.

### Were retries and recovery exercised, not just started?

| Mechanism | Exercised live? |
|---|---|
| Lease takeover after SIGKILL | Yes, repeatedly (fencing tokens show at least 1,232 ownership changes) |
| Fencing rejection | **No.** `fencing_rejections_total` is "not exported yet"; it has never fired live. Only `TestCommitDecisionRejectsStaleToken` (real Postgres) covers it. |
| Sequence-race recovery, claim-lost recovery | No (counters stayed 0 across about 8,900 runs, per the README) |
| Activity retry with backoff | **Not observable.** I armed 3 retryable failures on `ChargePayment` and the workflow completed with a normal 12-event history. Retries leave no event, no log line and no metric (probe: `evidence/fr3-retry-probe.txt`). FR-3 is BLOCKED for that reason. |
| Semantic DUPLICATE suppression | No (FR-13 BLOCKED) |
| Non-determinism failure, compensation | Unit tests only |
| Crash at a transaction boundary | **No.** `faultinj` defines `AfterActivityJournaled`, `BeforeDecisionCommit`, `AfterLeaseRenew`; `ShouldCrash` is called from tests only. Nothing in production code checks them. |

### Commands I ran (all results are mine, recorded in the appendix)

- `go test -count=1 ./...` (no database): all packages ok, 1.9 s.
- `go vet ./...`, `gofmt -l .`: clean.
- Throwaway Postgres (`pgvector/pgvector:pg16`, port 55432, not the demo cluster): `go test -count=1 ./...` x3 parallel and x3 with `-p 1`: pass. `go test -race -count=3 -p 1 ./...`: pass, no race, 55 s.
- **One failure worth recording:** the first run against a brand-new database, with four DB packages in parallel, failed 14 tests with `relation "shard_leases" does not exist`. `lease`, `taskq` and `timers` integration tests rely on the schema that the `eventlog` package's test creates. It passed on repeat. In CI (fresh Postgres, `go test ./...`) it is a scheduling race. See F-09.

## 3. Threat model (none exists in the repository; this is proposed)

| Item | Assumption I derived from the compose file and README |
|---|---|
| Deployment | Three engine nodes on one Docker network, Postgres leader and standby, Prometheus, Grafana. Intended as a demo and research cluster. |
| Assets | Workflow inputs (payment amounts, recipients, addresses), the event log, the lease table, the Groq API key. |
| Actors | Any caller who can reach ports 8081 to 8083 (the API), 5433/5434 (Postgres), 9090, 3000, 8000. |
| Trust boundary | None enforced. Everything on the network is trusted. |
| Out of scope | Multi-tenant SaaS. There is no tenant concept (grep for tenant returns nothing). I do not call the engine insecure for lacking one. The exposure is: **if this were put on any network with an untrusted caller, every control is missing.** |

## 4. Security findings

### Confirmed

| ID | Finding | Evidence |
|---|---|---|
| S1 | **No authentication or authorisation** on any `/v1` route (`internal/api/api.go:254-266`, no middleware other than request id, no-store, routing headers). Anyone can start, signal, cancel, list and read any workflow, including full payloads via `/v1/workflows/{id}/history`. | `curl localhost:8081/v1/workflows?limit=1` returned data with no credentials. |
| S2 | **No tenant isolation.** One global id namespace. Starting an id that exists returns the existing run in full (`already_open`). A caller can squat or probe ids. | code: `handleStartWorkflow` |
| S3 | **`POST /v1/admin/fail-step` is open and armed on the demo.** It is registered unconditionally. The guard is that `FAULT_INJECTION=true` must be set in the environment; the compose file sets it. The injector is keyed by activity type only (`FailNext(activityType, n)`); `workflow_id` is used just to pick the node. So one request makes the next N `ChargePayment` calls on that node fail for **all** workflows. With `non_retryable` it triggers compensation (refunds). I demonstrated only a harmless arm and disarm on a nonexistent activity type: both returned 202 with no credentials. | `evidence/s3-failstep-unauthenticated.txt` |
| S4 | The guard is a flag, not a boundary. Anyone who copies the compose file to production enables it. If the endpoint were reachable, the impact is an unauthenticated one-request denial of service and forced refunds. Mitigation: do not register the route unless the flag is set, and bind it to a separate admin listener. | `internal/config/config.go:386`, `internal/runtime/faults.go` |
| S5 | **Every service is published on `0.0.0.0`**: API 8081 to 8083, Postgres 5433 and 5434, Prometheus 9090, Grafana 3000, embedder 8000. | `docker ps` |
| S6 | **Over-privileged, weakly authenticated database.** The engine connects as `durexec`, which is a superuser (`rolsuper = t`), password `durexec`, `sslmode=disable`. Replication role password is `replicator`. Grafana is `admin/admin` with anonymous Viewer enabled. | `pg_roles` query; compose file |
| S7 | **Resource limits are partial.** Body capped at 1 MiB (tested: 1 MiB plus returns 400); list limit clamped to 1000. But only `ReadHeaderTimeout` is set (no read, write or idle timeout), there is no rate limit, no cap on workflows or payload per caller, and no retention job, so storage grows without bound (about 6 KB per workflow, 71 MB for 11.8k). Pool is 25 connections per node against `max_connections = 100`, so a fourth node exhausts the server. | `cmd/engined/main.go:369,450`; DB size query |
| S8 | **Poison task.** An unregistered `workflow_type` is accepted (201) and its workflow task retries forever with jittered backoff (attempt 12+ after two minutes). Activity tasks have an attempt cap; workflow tasks do not. There is no dead-letter state. Any caller can create unlimited immortal tasks. | `evidence/adversarial-api-probes.txt` |
| S9 | **Dependencies.** `govulncheck ./...`: 21 vulnerabilities reachable from source (20 in the Go standard library, 1 in `golang.org/x/text v0.40.0`). The shipped binary was built with go1.25.14 and `govulncheck -mode=binary` still reports 14, fixed only in Go 1.26.9/1.26.10 or `x/text v0.41.0`. Base images are floating tags (`golang:1.25`, `pgvector/pgvector:pg16`, `distroless/static-debian12:nonroot`, `python:3.11-slim`). All 16 modules in `go.mod` are marked `// indirect`. | `govulncheck` output |

### Checked and found fine

| Check | Result |
|---|---|
| SQL injection | All statements use `$n` parameters. A grep for `Sprintf`/string concatenation near SQL keywords across `internal` and `cmd` found one use: `LIMIT $%d` (a placeholder index) in `eventlog/store.go:215`. I did not read every one of the 56 query call sites. No user text is concatenated into SQL that I found. |
| Shard id manipulation | Shard is derived server-side from `workflow_id`. A caller cannot choose a shard. `X-Durexec-Forwarded-By` only disables forwarding (yields 409). |
| Secrets in git | `git log --all -p` over 88 commits, searched for `gsk_...`, `GROQ_API_KEY=<value>`, AWS key shape: **no match**. `engine/.env` is gitignored and untracked. |
| Secrets in logs | Startup log redacts the DB password (`****`). The node log has 0 occurrences of `gsk_`. |
| Container hardening | Distroless, non-root, no shell. Good. |

### Not verified

| Item | Why |
|---|---|
| The Groq key in container environment | It is in `docker inspect` for anyone with Docker access. Normal for compose, noted only. I redacted it in my output. |
| Per-vulnerability exploitability | I did not analyse each govulncheck item. |
| TLS to Groq, embedder trust | Not tested. |

## 5. Operational readiness

### Can an incident be diagnosed without reading database rows?

| Question | Answerable from metrics, logs or API? |
|---|---|
| Which workflows are stuck? | **No.** Needs SQL. I found 19 orphans only by querying `executions`. |
| Which shard owns a workflow, and when does the lease expire? | **Yes**, `GET /v1/shards` gives owner, token, `expired`. |
| Tasks pending, leased, retrying, dead-lettered? | **Partly.** `task_queue_depth` counts only tasks pollable now. Leased and retrying are not separated. **There is no dead-letter state at all.** |
| Overdue timers? | **No** metric. |
| Is replay time growing? | Yes, `replay_duration_seconds` (in-process replay only). |
| Replication lag threatening durability? | **No.** No Postgres exporter. I read `pg_stat_replication` by hand (replay lag about 1 ms, `async`). |
| Compensations failing? | **No** metric. `workflow_completed_total{status}` only. |
| Workflows with ambiguous external outcome? | **No.** The engine is at-least-once for external calls (`activity_worker.go` comments say so). It has no "outcome unknown" state. |
| Activity retries happening? | **No.** Invisible (probe above). |
| Which build is each node running, is config the same? | **No.** `-X main.version` is set in the Dockerfile but `main.go` has no `version` variable, so it does nothing. No build-info metric. The project has already been bitten by three nodes on three builds. |

**Metrics inventory (all of it):** `workflow_completed_total`, `replay_duration_seconds`, `shard_recovery_seconds`, `lease_transitions_total`, `fencing_rejections_total`, `dedup_verdict_total`, `dedup_latency_seconds`, `task_queue_depth`, `dedup_escalation_total`, `worker_task_failures_total`. **No alert rules, no `rule_files`, no Alertmanager, no runbook.** Seven Grafana panels.

### Green-but-meaningless signals

| Signal | Why it is green while wrong | Evidence |
|---|---|---|
| `/healthz` status | The handler never touches the database (`handlers.go:550`). Container HEALTHCHECK reads only the status code. | r3: DB paused 20 s, all three nodes answered 200 throughout (`healthz-during-db-pause.log`). |
| `owned_shards` | Fixed in 9223122 to read the lease manager, but that is the node's *belief*. It stayed 4/9/3 for about 17 s after every lease had expired, and dropped to 0/0/0 only after the database came back. | same log |
| Acceptance matrix NFR-1 "MANUAL" | Printed the 19 orphans as a count and left it to a human. | matrix |
| `sync_commit=true` in the startup line | Means local fsync, not synchronous replication. Replication is `async`. Easy to misread. | `pg_stat_replication` |
| Fake embedder | If `EMBEDDER_URL` is empty the node silently uses a hash encoder. The live DB holds 4 verdicts from the fake and 4 from MiniLM. Only a WARN line at startup. | config, DB |
| Embedder outage | Degrades to DISTINCT (activity runs). Safe for availability, but semantic protection is silently off. No metric. | `embedder.go:84` |

### Lifecycle

| Area | Finding |
|---|---|
| Graceful shutdown | Design is sound: stop HTTP, drain workers, then release leases (`main.go:400-436`). Logs show `engine node stopped cleanly`, exit 0. In the 2026-10-20 stop, Postgres died within 12 ms of the nodes' signal and lease release failed, so leases had to expire. **Drain with a long-running activity is untested.** On a shutdown timeout the node returns exit 0 while possibly still holding leases. |
| Restart policy | `restart: "no"` on the nodes (deliberate for demos). A crashed node stays down. At the start of this audit all three nodes had been stopped for 29 hours. |
| Rolling deployment | **Unsafe for workflow code changes.** There is no workflow versioning (no `GetVersion` or patch marker). A changed workflow replays old history, hits non-determinism, and is failed terminally (`TestWorkflowWorkerFailsTerminallyOnNonDeterminism`). A mixed-version cluster during a rollout will fail in-flight workflows. |
| Migrations | `Migrate` runs every `*.up.sql` on every node start, under an advisory lock, with no version table. Today's only file is all `IF NOT EXISTS`. The first non-idempotent migration will fail on the second start. Down migration exists; no rollback procedure documented. |
| Backward compatibility | No schema or event-format version field was found (grep). Unknown. |
| Backup and point-in-time recovery | **None recorded; verified.** `archive_mode = off`; no `pg_dump`, no WAL archive, no base-backup schedule in the repo. The standby is a replica, not a backup: a bad `DELETE` replicates in milliseconds. |
| Secret rotation | Key keyring supports several `GROQ_API_KEY*` variables; DB password rotation means editing the compose file and restarting all nodes. No procedure. |
| Config drift | The three nodes share one YAML anchor. Nothing checks it at runtime; the hash ring is a pure function of `CLUSTER_NODES`, so a node with a different list would route differently with no alarm. |

### Consequences of the manual failover

Drill facts (`standby-promotion-run2.txt`): promotion 0.14 s, first new workflow accepted 15.4 s after the kill, **2 committed events lost** (run 3: 1), three manual steps.

| Consequence | Detail |
|---|---|
| Availability | 9 to 15 s with no writes, plus a human who must be awake. Detection is also manual: nothing alarms when the leader dies (no alert rules). Real time to recover is paging time plus 15 s. |
| Durability | Replication is async, so an acknowledged commit can vanish. A lost `ActivityTaskCompleted` for a step whose external effect already happened makes the promoted engine **run that step again**. Whether that is a second payment depends entirely on the downstream honouring the dedup key, which no test checks. |
| Split brain | Nothing fences the old leader. The drill keeps its data directory and the old leader keeps its own copy. Fencing tokens protect writes within one database only. |
| Lease state | The promoted database has whatever `shard_leases` the standby had. Token monotonicity across a lossy promotion is not argued or tested. |
| Restore | The drill re-clones the standby from the old leader, so the lost events are discarded. An operator needs a rule for reconciling them. |

### Recommended alerts

Cardinality rule: labels are `shard` (16), `node`, `workflow_type`, `activity_type`, `reason`. Never `workflow_id`, `run_id` or `dedup_key`.

| Alert | Suggested metric (new unless noted) | Condition | Why |
|---|---|---|---|
| Orphan or stuck start | `executions_orphaned{shard}` (gauge from a sweep: RUNNING, `last_seq=0`, older than 60 s) | > 0 for 2 min | F-01 |
| Stuck workflows | `workflow_oldest_running_age_seconds{workflow_type,shard}` | above the type's budget | no signal today |
| Shard unowned | `shard_lease_expired{shard}` (from `shard_leases`) or `lease_transitions_total{direction="lost"}` (exists) | any shard unowned > 15 s, or fewer than 16 owned across nodes | the 4-dead-shards incident |
| Node believes it owns shards the DB disagrees with | `shards_owned_local` vs `shards_owned_db{node}` | differ > 5 s | the 17 s lie |
| Fencing rejections | `fencing_rejections_total{shard}` (exists, never fired live) | any increase | split-brain evidence |
| Task backlog and retry storms | `task_queue_depth{shard,task_type}` (exists) plus `task_attempts_max{task_type}` and `tasks_locked{...}` | depth rising 10 min, or max attempt > 5 | poison tasks (S8) |
| Dead letters | `tasks_dead_total{task_type}` (needs the feature) | any | none exist |
| Overdue timers | `timers_overdue{shard}` (now() - oldest `fire_at`) | > 30 s | no signal |
| Replay time | `replay_duration_seconds` p99 (exists) | above budget | |
| Replication lag | `pg_stat_replication_replay_lag_seconds` via `postgres_exporter`, plus `pg_replication_slot_wal_bytes` | lag > 5 s, or no streaming replica | durability |
| DB connections | `pg_stat_activity_count / max_connections` | > 80% | pool is 25 per node |
| Compensation failures | `workflow_completed_total{status="COMPENSATING_FAILED"}` (status exists) | any | money left half-undone |
| Ambiguous external outcome | `activity_outcome_unknown_total{activity_type}` (needs a state) | any | payments |
| Semantic suppression | `dedup_verdict_total{verdict="DUPLICATE",activity_type}` (exists) | any, until measured safe | each one is a skipped real action |
| Embedder health | `embedder_failures_total` | rate > 0 | silent fallback |
| Node build and config drift | `build_info{version,config_hash}` | more than one value | repeat of a past incident |
| Backup freshness | `last_successful_backup_age_seconds` | > 24 h | no backup exists |

## 6. Semantic idempotency: should it be advertised?

**No, not as a safety feature.** Facts from the project's own files:

| Fact | Source |
|---|---|
| It is **on by default** (`dedup=semantic`, tau 0.85 / 0.90, `escalation_blocks=false`) for the two agent activities `SearchFlights` and `BookFlight` | `config.go`, node startup line, `app/agent/activities.go` |
| On the generated corpus at the shipped threshold, **108 of 115 distinct pairs are called DUPLICATE** (false-positive rate 0.939, hard cases 1.000) | `semantic-experiment-minilm.txt` |
| On a live model it caught **34 of 45** reworded repeats (61 to 86% interval). 11 of 45 would still run twice. | `rephrase-live-model.txt` |
| That live run has no false-suppression measurement (0 drift trials) | same |
| The hybrid comparator's 1.000 never consulted the model, and its extractor reads 0% of four of the five domains | `results/README.md`, rephrase file |
| A DUPLICATE verdict **does not run the activity and journals a reused result.** The workflow carries on believing the booking happened. | `runtime/dedup.go` |

The danger is that the failure is **silent and asymmetric**. A wrongly suppressed booking produces a successful-looking workflow, no error, no retry, no compensation and no page. A missed duplicate at least has a visible effect. No alert exists, there is no shadow (log-only) mode, no per-request opt-out, and the only human review path (`ESCALATION_BLOCKS`) covers only the uncertain band, not the DUPLICATE band that does the damage.

Recommendation: ship it **off**, or in a log-only mode that journals the verdict and runs the activity. Use it only to raise ESCALATE. Move to activities declaring their own deciding fields, which is where the project's own conclusion already points.

## 7. Adversarial test matrix

Legend: **E** existing test, **N** newly executed by me, **P** proposed, **X** could not run. Evidence paths are under `docs/audit/evidence/` unless stated.

| ID | Component | Scenario | Preconditions | Injection point | Expected | Current coverage | Risk | Automation | Result | Evidence |
|---|---|---|---|---|---|---|---|---|---|---|
| T01 | Start path | Client aborts mid `POST /v1/workflows` | 3 nodes up | 2-30 ms client timeout | No orphan | none | **Critical** | N | **FAIL: 151 orphans of 553 rows** | `abort-ab1.*` |
| T02 | Start path | Retry an orphaned id | T01 | resend same id | Workflow runs or error | none | Critical | N | **FAIL: 200 `already_open`, still inert after 20 s** | `abort-ab1.retry.txt` |
| T03 | Start path | Shard not owned between `Create` and `CommitDecision` | throwaway Postgres | `TokenFor` false | No row left | none | Critical | N (characterisation test, passes while defect exists) | **FAIL (defect confirmed)** | `engine/internal/runtime/audit03_start_orphan_test.go` |
| T04 | Postgres | Leader paused 20 s under load | 300 wf over 60 s | `docker pause` | All accepted run | none | Critical | N | **FAIL: 17 of 300 orphans**, 0 dup k | `chaos-r3.*` |
| T05 | Health | `/healthz` during T04 | | | Red or degraded | none | High | N | **FAIL: 200 throughout, owned_shards stale 17 s** | `healthz-during-db-pause.log` |
| T06 | Failover | Two nodes SIGKILLed 4 s apart | 400 wf | `docker kill` | No loss, no dup | partly (published) | High | N | PASS: 391 accepted, 0 orphans, 0 stuck, 0 dup | `chaos-r2.*` |
| T07 | Rolling restart | node-c graceful stop, node-b SIGKILL, restarts | 300 wf | `docker stop`/`kill` | no loss | none | High | N | PASS: 288 accepted, 0 orphans, 0 stuck, 0 dup; 12 refused after 10 tries | `chaos-r1.*` |
| T08 | Concurrency | 30 concurrent starts of one id over 3 nodes | | race | 1 run | none | High | N | PASS: one 201, 29 x 200, one start event | `adversarial-api-probes.txt` |
| T09 | Input | Body over 1 MiB | | | rejected | partial | Low | N | PASS (400) | probes |
| T10 | Input | Wrong-typed input | | | clean failure | none | Medium | N | PASS: FAILED at seq 2 | probes |
| T11 | Poison | Unregistered workflow type | | | reject or dead-letter | none | Medium | N | **FAIL: 201, retries forever** | probes |
| T12 | Admin | Unauthenticated `fail-step` | FAULT_INJECTION=true | none | 401 | none | High | N | **FAIL: 202 with no credentials** | `s3-failstep-unauthenticated.txt` |
| T13 | Retry | 3 injected failures then success | | `fail-step` | visible retries | BLOCKED in matrix | Medium | N | Not observable: no event, log or metric | `fr3-retry-probe.txt` |
| T14 | Storage | Integration tests on a fresh DB, packages in parallel | empty Postgres | scheduling | pass | partial | Medium | N | **FAIL once, 14 tests, missing relation**; passes on repeat | appendix |
| T15 | Race | `go test -race -count=3` | | | clean | none | Medium | N | PASS | appendix |
| T16 | Crash | Kill between activity journal and next decision | | `faultinj.AfterActivityJournaled` | no re-run | defined, never wired | **High** | X | NOT RUN: no production hook | |
| T17 | Crash | Kill after `CommitDecision` COMMIT, before ack | | `pg_terminate_backend` at commit | idempotent retry | none | High | P | NOT RUN | |
| T18 | External effect | Stub payment API counting calls by dedup key, with SIGKILL after the call and before the journal | stub service | kill | one charge per key | none: effects simulated | **Critical** | P | NOT RUN | |
| T19 | External effect | Same, after promotion that lost the completion event | lossy promote | `make promote-drill` | one charge | none | Critical | P | NOT RUN | |
| T20 | Lease | Clock skew of 5 s between a node and Postgres | | `libfaketime` | no dual owner | `lease` unit tests with fake clock only | High | P | NOT RUN | |
| T21 | Lease | Node paused (SIGSTOP) past TTL then resumed with a commit in flight | | `docker pause` node | commit fenced | `TestCommitDecisionRejectsStaleToken` (real PG) | High | P | NOT RUN live | |
| T22 | Network | Partition node from Postgres, not from clients | | `iptables`/`docker network disconnect` | node stops serving | none | High | P | NOT RUN | |
| T23 | Queue | Poll with 8 workers x 16 shards and DB pool 25 | high load | | no starvation | none | Medium | P | NOT RUN (Agent 2) | |
| T24 | Queue | Task lock expiry during a 40 s activity | | long activity | claim renewed | `TestLongActivityRenewsItsClaim` | Medium | E | PASS (fakes) | |
| T25 | Timers | 100k timers due at once; timer fires during failover | | | none lost, fired once | 2 DB tests | Medium | P | NOT RUN | |
| T26 | Replay | History of 10k events | | | bounded replay | benchmark at 200 | Medium | P | NOT RUN | |
| T27 | Replay | Deploy changed workflow code with runs in flight | | new image | runs continue | non-determinism test only | **High** | P | NOT RUN | |
| T28 | Saga | Compensation activity fails 3 times | | `fail-step` non-retryable on `RefundPayment` | `COMPENSATING_FAILED` visible | unit test only; FR-11 MANUAL | High | P | NOT RUN | |
| T29 | Data growth | 1M workflows, no retention | | | bounded storage | none | Medium | P | NOT RUN | |
| T30 | Backup | Restore to a point before a bad `DELETE` | | | recoverable | none | **High** | P | NOT RUN: no backup exists | |
| T31 | Semantic | 100 distinct bookings differing only in date or passenger count | real embedder | | none suppressed | synthetic corpus only | **High** | P | NOT RUN | |
| T32 | Semantic | Embedder down, then `EMBEDDER_URL` unset | | | alert | none | Medium | P | NOT RUN | |

Prior safety evidence I could not strengthen: T16 to T22 are the tests that would actually falsify the exactly-once-effect claim. None can be run without production hooks or a stub service.

## 8. Findings register

Launch-blocking means blocking for a production launch handling money. For a supervised demo, none of these block.

| ID | Sev | Conf | Location | Preconditions | Failure sequence | Impact | Reproduction | Fix | Regression test | Blocking |
|---|---|---|---|---|---|---|---|---|---|---|
| F-01 | **Critical** | Confirmed | `engine/internal/runtime/engine.go:104-153` (`StartWorkflow`), `internal/eventlog/store.go:111` (`Create`) | Any client timeout or disconnect, lease handover, or DB stall during start | `Create` commits the row. `TokenFor` or `CommitDecision` then fails or is cancelled. Row stays RUNNING, `last_seq=0`, no event, no task. Retry gets 200 `already_open`. | Accepted workflow never runs, silently, permanently. Contradicts the function's own comment and the README's "0 accepted work lost". 19 already in the live DB. | `python3 -I docs/audit/evidence/audit03_client_abort_orphans.py ab1 600` (151 orphans); `DUREXEC_TEST_DSN=<throwaway> go test -run Audit03 ./internal/runtime` | Insert the execution row inside the same fenced transaction as the first event and task. In the interim, a sweeper that re-enqueues the first task for RUNNING rows with `last_seq=0`. | Flip the Audit03 test to assert no orphan; keep the abort script as a nightly check | **Yes** |
| F-02 | High | Confirmed | `app/*/activities.go`, `loadgen` audit, README | Any payment-like use | The engine is at-least-once for external calls. Safety depends on the downstream honouring `DedupKey`. No test uses a real or stub external service. The delivery stub ignores the key and its ids (`pay_000001`) repeat per node. | "Executed twice: zero" is unproven. A lost journal write after a charge means a second charge unless the provider dedups. | Read `activity_worker.go` header; run T18 (proposed) | A counting stub service that records dedup keys; document the downstream idempotency requirement; reword the README | T18, T19 | **Yes, for payments** |
| F-03 | High | Confirmed | `internal/api/api.go:254-266` | API reachable by anyone | No auth, no tenancy, fail-step open, cross-workflow effect. All ports on 0.0.0.0. | Read, cancel or sabotage any workflow. | S1, S3 above | Authn on `/v1`, admin on a separate listener, route not registered unless the flag is set, bind to loopback or an internal network | Handler test: 401 without token | **Yes, off a trusted network** |
| F-04 | High | Confirmed | `deploy/promote_standby.sh`, `docker-compose.yml` | Leader failure | Manual detection and promotion, async replication loses 1 to 2 events, old leader not fenced. | 9 to 15 s of write outage plus paging time; possible re-execution of a step whose completion was lost. | `docs/results/standby-promotion-run2.txt` | Automatic failover (Patroni), synchronous replication for at least one standby, fence the old leader | Failover drill in CI with an effect ledger (T19) | **Yes** |
| F-05 | High | Confirmed | `deploy/` | Any data-corrupting bug or operator error | No backup, no WAL archive (`archive_mode=off`), no restore drill. | Unrecoverable loss. | `show archive_mode` | `pg_basebackup` plus WAL archiving, tested restore | T30 | **Yes** |
| F-06 | High | Confirmed | `deploy/prometheus.yml`, `internal/api/handlers.go:550` | Any incident | 10 metrics, no alert rules, `/healthz` is static. | Failures are found by users. | r3 health log | Alerts in section 5; health check that pings the DB and checks lease freshness | T05 | **Yes** |
| F-07 | Medium | Confirmed (code) | `internal/workflow/*`, `eventlog/migrate.go` | Any deploy of changed workflow code | No versioning; replay divergence fails runs terminally. Migrations rerun on every start, no version table. | A rolling deploy fails in-flight workflows. | `TestWorkflowWorkerFailsTerminallyOnNonDeterminism` | Workflow version markers; a migration table | T27 | Yes if deploys happen with runs in flight |
| F-08 | Medium | Confirmed | `config.go` defaults, `runtime/dedup.go` | Semantic enabled on `BookFlight` | Section 6 | Silent wrong suppression; fake-embedder fallback; silent off when the embedder is down | `semantic-experiment-minilm.txt` | Default off or shadow mode; fail startup if `EMBEDDER_URL` is empty and semantic is on | T31, T32 | **Yes, for advertising the feature** |
| F-09 | Medium | Confirmed | `engine/.github/workflows/ci.yml`, `deploy/assert_no_duplicates.sql`, `lease|taskq|timers/integration_test.go` | | CI file is outside the repo root; stage 7 assertion is vacuous; integration tests depend on another package creating the schema. | The "tested on every push" claim is untrue. | `gh run list`; fresh-DB parallel test run | Move to `.github/workflows` at the root; assert on `k`, not the primary key; each test migrates for itself | Fresh-DB CI job | No |
| F-10 | Medium | Confirmed | `docs/results/scaling-saturated.txt`, `...-after-liveness-fix.txt` | | c=64 4-shard point generator-bound (64 / 2.008 = 31.9 vs 32.1); Only 3 of 6 re-run sweeps reproduce; the others are set aside by a load threshold chosen afterwards, and some show 0.00 wf/s at 1 shard. | Scaling claim rests on one concurrency level. | arithmetic from the files | Mark the c=64 point invalid; publish all six sweeps | n/a | No |
| F-11 | Medium | Confirmed | `README.md`, `BASELINE_SUMMARY.md` | | Headline availability and "0 lost" use the better run (878) not the later run (763). "Executed twice" wording. | Overstated. | compare the two files | Report both; reword to "journaled twice" | n/a | No |
| F-12 | Medium | Confirmed | `internal/faultinj/injector.go`, `runtime` | | Crash points `AfterActivityJournaled`, `BeforeDecisionCommit`, `AfterLeaseRenew` are never called from production code. | No test can crash at a transaction boundary. | `grep -rn ShouldCrash` | Wire the hooks behind the flag, add boundary tests | T16, T17 | No |
| F-13 | Medium | Confirmed | `Dockerfile`, `go.mod` | | 14 to 21 govulncheck findings, floating base tags, `-X main.version` no-op. | Unpatched stdlib, unreproducible builds. | `govulncheck` | Go 1.26.10 toolchain, pin digests, expose build info | CI govulncheck | No (conditional on exposure) |
| F-14 | Medium | Confirmed | compose, `10-replication.sh` | | Superuser DB account, default passwords, `sslmode=disable`, Grafana admin/admin. | Credential reuse. | `pg_roles` | Least-privilege role, secrets from the environment, TLS | | Yes off a trusted network |
| F-15 | Medium | Confirmed | `workflow_worker.go:333` | | Unknown type retried forever. | Immortal tasks, log noise, no dead letter. | T11 | Cap attempts, terminal state | T11 | No |
| F-16 | Low | Confirmed | `main.go:369`, `main.go:450` | | No read/write/idle timeouts, no rate limit, no retention, pool 25 x 3 = 75 of 100. | Slow-client and growth risks; a fourth node starves the DB. | | Timeouts, retention job, PgBouncer | | No |
| F-17 | Low | Confirmed | `docs/results/README.md` | | Run 1 numbers published but its output is gone. | Unverifiable. | | Mark them or drop them | | No |
| F-18 | Low | Confirmed | live DB | | 48 tasks at shards 900 to 911 left by an earlier test run against the demo DB. | Test pollution. | `select ... from tasks` | Clean up; tests use their own DB | | No |

## 9. Items for other agents

F-01 overlaps Agent 1's remit (correctness). The 25-connection pool and 16 shards x 12 workers per node overlaps Agent 2's. I report them here only where they affect operations or evidence.

## 10. Appendix: state left, evidence, and what I could not check

### State left

| Item | State |
|---|---|
| Engine nodes | **node-a, node-b, node-c are running.** They were found **stopped** (exited 0, about 29 h earlier, stopped together with the database on 2026-10-20 13:00 UTC) at the start of this audit. I ran `docker start` on them. After the last test all three answer `/healthz` ok (4 + 9 + 3 = 16 owned). |
| Lease rows | 16 of 16 live. |
| Embedder overlay | Applied (the nodes carry `EMBEDDER_URL=http://embedder:8000/embed`; the embedder answers `ready`). |
| Postgres | leader and standby healthy, replication streaming, `async`. |
| Rows I added to the demo DB | 1,368 `a03-*` executions (completed), with their events. I **deleted** my own orphans (151 + 17) and my poison task. **I left the 19 pre-existing orphans untouched**, plus the 48 pre-existing test tasks at shards 900 to 911. |
| Throwaway Postgres (`audit03-pg`, port 55432) | Removed. |
| Disturbances to the cluster | Node stop/kill/start at 23:38 to 23:48 IST and a leader `pause` at 23:56:22 to 23:56:42 IST on 2026-10-21. Anyone benchmarking at that time got perturbed numbers. |
| Files added | `docs/audit/evidence/*` (scripts, CSVs, logs), `engine/internal/runtime/audit03_start_orphan_test.go`. Nothing committed. Nothing in `docs/results/` touched. |

### Commands, in order

1. `go test -count=1 ./...` with no DSN, 1.9 s: pass.
2. `docker run -d --name audit03-pg ... pgvector/pgvector:pg16 -p 55432:5432`, then `DUREXEC_TEST_DSN=postgres://durexec:durexec@localhost:55432/durexec?sslmode=disable go test ...` (details in section 2).
3. `go vet ./...`, `gofmt -l .`, `go test -race -count=3 -p 1 ./...`.
4. `govulncheck ./...` and `govulncheck -mode=binary` on the binary copied out of `node-a`.
5. `python3 -I docs/audit/evidence/audit03_chaos_accounting.py r1 300 60 "10:stop:node-c,25:start:node-c,35:kill:node-b,50:start:node-b"`; `r2 400 60 "8:kill:node-a,12:kill:node-c,30:start:node-a,32:start:node-c"`; `r3 300 60 "15:pause:deploy-postgres-leader-1,35:unpause:deploy-postgres-leader-1"`.
6. `python3 -I docs/audit/evidence/audit03_client_abort_orphans.py ab1 600`.
7. `curl` probes recorded in `adversarial-api-probes.txt` and `fr3-retry-probe.txt`.

Both Python scripts append one fsynced row per result and recompute the summary from disk. Resume: re-run the same command.

### Files inspected

`docs/results/README.md`, `acceptance-matrix-final-image.txt`, `duplicates-under-failure.txt`, `duplicates-under-failure-after-liveness-fix.txt`, `duplicates-undisturbed.txt`, `standby-promotion-run2.txt`, `scaling-saturated.txt`, `scaling-saturated-after-liveness-fix.txt`, `scaling-c16-GENERATOR-BOUND.csv` (head), `green-but-wrong-findings.txt` (first 120 lines), `semantic-experiment-minilm.txt` (first 60 lines), `rephrase-live-model.txt` and its NOTES, `field-extractors.NOTES.txt`, `field-extractors-llm-ratelimited-rows.NOTES.txt`; `docs/BASELINE_SUMMARY.md`; `README.md`, `CLAUDE.md`, `engine/CONTRACT.md`; `engine/Dockerfile`, `go.mod`, `Makefile`, `deploy/docker-compose.yml`, `docker-compose.embedder.yml`, `prometheus.yml`, `postgres/10-replication.sh`, `assert_no_duplicates.*`, Grafana dashboard JSON, `embedder/Dockerfile`, `healthcheck`; `engine/.github/workflows/ci.yml`; `migrations/0001_init.up.sql` (grep); `internal/api/{api,handlers,forward}.go`, `internal/runtime/{engine,activity_worker,faults,dedup}.go`, `internal/eventlog/{commit,store,migrate}.go`, `internal/faultinj/injector.go`, `internal/config/config.go`, `cmd/engined/main.go`, `app/delivery/activities.go`, test file inventory by name and function count.

### Not checked

| Item | Why |
|---|---|
| Whether CI has ever run | `gh run list` gave no output; no network confirmation. |
| ~20 files in `docs/results/` | Not opened: the HTML reports, hybrid and field-extractor CSVs, `LIVENESS_FIX_BEFORE_AFTER.md`, `liveness-fix-demonstrations.txt`, `acceptance-matrix.txt`, `acceptance-matrix-after-liveness-fix*.txt`, `standby-promotion-run3.txt` raw. I relied on the README's statement that outcomes match for the matrix files. |
| Real database failover | Would restructure the cluster the student demos from. |
| Crash at a transaction boundary, real external-effect ledger | Needs production hooks and a stub service (T16 to T19). |
| Graceful drain with a long activity, clock skew, network partition | Not run. |
| Grafana rendering, alert delivery | No alerts exist. |
| Per-vulnerability reachability | Not analysed. |
| Agents 1 and 2 | Their files `01-...` and `02-...` were present when I finished; I did not read them. |
