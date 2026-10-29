# Cloud acceptance matrix

Status as of **2026-10-28, before any deployment**. Every cloud row is
`NOT RUN` because nothing has been deployed — that is the honest state, not a
placeholder. Local rows were actually executed and their results are real.

Legend: **PASS/FAIL** = ran, result observed. **NOT RUN** = prepared, awaiting
deployment. **BLOCKED** = cannot run until something else exists. **CANNOT** =
the chosen architecture provably cannot test it.

## Tier 0 — local, already executed

| # | Check | Command | Status | Result |
|---|---|---|---|---|
| L1 | Terraform formats and validates | `make aws-validate` | **PASS** | `fmt` clean, `validate` Success |
| L2 | Plan creates only what is intended | `make aws-plan` | **PASS** | 18 resources; 1 ingress rule (tcp/22 from the given /32); no `0.0.0.0/0` ingress; both volumes encrypted; IMDSv2 required; only `AmazonSSMManagedInstanceCore` |
| L3 | Nothing would be publicly bound | `./scripts/aws/assert_no_public_bind.sh` | **PASS** | 7 published ports, all `127.0.0.1`; `extsim` publishes none; no Terraform rule names a service port |
| L4 | Both images build for linux/amd64 | `make aws-build` | **PASS** | engine 21.9 MB, extsimd 8.05 MB, tagged `fde6ae0-dirty` |
| L5 | The simulator honours an idempotency key | manual, local container | **PASS** | same key twice → one effect, `replayed:true`, `eff_0001` reused |
| L6 | An unkeyed call is still performed | manual, local container | **PASS** | two unkeyed calls → two distinct effects. **But see the caveat below.** |
| L7 | `gofmt`, `go vet` | `gofmt -l .`, `go vet ./...` | **PASS** | both clean |
| L8 | Full suite against a real database | `make test-integration` | **PASS** | **1220 passed, 0 failed, 0 skipped, 2 blocked** |
| L9 | Race detector on critical packages | `go test -count=1 -race ...` | **PASS** | extsim, faultinj, config, api, runtime, activity, taskq all clean |

**L6 caveat, and it changes an assertion.** `duplicate_effect_count` is computed
as *effects − distinct keys*. Two legitimately different unkeyed payments share
the empty key, so the ledger reported `duplicate_effect_count: 1` for two
correct effects. **`duplicate_effect_count == 0` is only meaningful when
`Unkeyed == 0`.** Every external-effect check below asserts both together. The
engine never sends an unkeyed call — `extsim.ErrNoDedupKey` refuses
non-retryably — so `Unkeyed` should be 0 in practice, which is exactly why it
must be checked rather than assumed.

## Tier 1 — must pass on the first deployment

All run by `./scripts/aws/acceptance_tier1.sh`, which records one CSV row per
case and resumes where it stopped.

| # | Check | How it is judged | Status |
|---|---|---|---|
| T1.1 | Engine starts and reports **truthful** health | `/v1/shards`: 16 shards, 0 unowned, 0 expired. Not `/healthz` 200 — and not `/healthz` either, which gives only a *count* and cannot distinguish 16-owned-once from two nodes claiming one shard | NOT RUN |
| T1.2 | A workflow is started, durably recorded, completed | status `COMPLETED` **and** a non-empty history | NOT RUN |
| T1.3 | Duplicate starts behave correctly | second start returns the **same `run_id`**. Phase 6's counter wrongly read a duplicate as a second acceptance, so identity is checked, not the status code | NOT RUN |
| T1.4 | Concurrency across nodes | 12 workflows started round-robin through all three nodes (so forwarding is exercised); 12/12 complete | NOT RUN |
| T1.5 | Engine death recovers with no duplicate journal entries | `docker kill --signal=KILL node-b`, wait past the 10 s TTL; 6/6 complete **and** duplicate `(run_id, seq)` groups = 0 | NOT RUN |
| T1.6 | Ownership moves; fencing tokens behave | shards move, every moved shard's token **increases**, **no** token regresses, none left unowned | NOT RUN |
| T1.7 | PostgreSQL restart and reconnection | restart the leader; a new workflow still completes | NOT RUN |
| T1.8 | No unexplained leftover work | non-terminal = 0, outstanding tasks = 0, duplicates = 0 | NOT RUN |
| T1.9 | Load: throughput, latency, errors, saturation, in-flight | `cmd/loadgen`. **Absolute rates are not comparable** — three nodes and two databases share 2 vCPUs. Report scaling shape and correctness counts. | NOT RUN |
| T1.10 | Logs and evidence survive a failure | `collect_evidence.sh` **before** any disarm or restart | NOT RUN |

`tasks` has no `completed_at`: a finished task is **deleted**, so any remaining
row is work owed. Terminal statuses are `COMPLETED`, `FAILED`,
`COMPENSATING_FAILED` — `CANCELLED` and `TERMINATED` do not exist in this
engine. An earlier draft of the smoke script queried a `completed_at` column
and would simply have errored.

## Tier 2 — durability and operational recovery

| # | Check | Status | Note |
|---|---|---|---|
| T2.1 | Create and restore a backup | NOT RUN | `pg_dump -Fc`, `pg_restore -l`, restore into a second database, compare counts |
| T2.2 | Restore into an isolated **instance** | NOT RUN | Phase 8 restored into the same instance, so instance/volume loss stayed untested |
| T2.3 | **Engine instance loss** | NOT RUN | Newly possible: PGDATA is on a detachable EBS volume. `user_data` refuses to format an existing filesystem |
| T2.4 | Measure recovery time and any accepted-work loss | NOT RUN | RTO from terminate to `smoke.sh` PASS; RPO by comparing counts against the pre-loss evidence bundle |
| T2.5 | **EBS volume loss** | **CANNOT (safely)** | Would need a deliberate volume delete with only a snapshot to recover from. Possible, but out of scope for a first deployment |
| T2.6 | Restored journal vs external reality can diverge | NOT RUN | Needs the simulator; restoring to an earlier point rewinds the journal behind effects that really happened |
| T2.7 | Document the absence of PITR and checksums | **PASS (documented)** | `archive_mode=off` → no PITR at all; `data_checksums=off` → silent corruption undetectable; `wal_log_hints=off` → no `pg_rewind`. `collect_evidence.sh` re-reads all nine settings in the cloud |

## Tier 3 — explicitly bounded, deliberately not made green

Nothing below was implemented to improve a number. Each is a recorded limit.

| Item | Status | Why it stays |
|---|---|---|
| Exactly-once external effects | **NOT CLAIMED** | The contract is at-least-once invocation with effectively-once effects **only when the recipient honours the idempotency key**. Unchanged by this phase |
| Cross-run idempotency (`dx2:` scheme) | **BLOCKED — designed, not built** | `08-DESIGN-external-effect-contract.md`. Switching schemes mid-run would give a redelivered activity a different key, turning a safe retry into a duplicate effect |
| External-effect compensation | **BLOCKED on a product decision** | Compensating an *ambiguous* outcome can create a negative effect that never had a positive one. A tripwire test fails if anyone registers a compensation on `ExternalPayment` |
| PITR, off-node backups, `data_checksums` | **ABSENT** | Would change the database's durability configuration; needs explicit authorization |
| Importable SDK, workflow versioning | **ABSENT** | No story exists; none invented here |
| Automatic DB failover | **WORSE THAN PUBLISHED WORK** | 9–15 s and manual, against AMBROSIA's 1.8 s automatic with no loss. State this first to an examiner |
| `TestAcceptNoDuplicateExternalEffectBlocked` | **BLOCKED, but its prerequisites now exist** | It waits on "the simulator counting calls by dedup key **and** a crash between the effect and its journal write". `cmd/extsimd` is the first; `faultinj.BeforeResultJournal` is the second; Phase 8 scenario H already showed the behaviour by hand. **Automating it is the single highest-value next task.** Not done here — the brief forbids adding capability to turn the matrix green |
| `TestAcceptSagaRecoveryOnLiveClusterBlocked` | **BLOCKED, genuinely** | Asserts a compensation is "neither skipped nor repeated against a real effect". That is the compensation question above — a product decision, not missing plumbing |
