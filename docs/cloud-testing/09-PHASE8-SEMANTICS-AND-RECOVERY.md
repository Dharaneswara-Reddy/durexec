# Phase 8 — External-Effect Semantics, Recovery, and Release Blockers

**Contract, unchanged and restated:** **at-least-once activity invocation, with
effectively-once external effects only when the recipient honours the idempotency key.** This
phase closed the two boundaries Phase 7 left open and both held — but neither strengthens the
contract, because both are protected by the *same* journaled key, not by anything atomic.

**The two new results:**
- **Process death at the narrowest boundary** (effect confirmed → before the result journal)
  now has a deterministic seam and direct evidence: one effect across a real `SIGKILL`.
- **Ownership moving mid-flight** is safe, but **not for the reason expected**: the stale
  worker was stopped by the **task-claim check**, not by fencing. Fencing was never reached.

**Cross-run idempotency and external-effect compensation are DESIGNED, NOT BUILT** — see
[08-DESIGN-external-effect-contract.md](08-DESIGN-external-effect-contract.md). Both need an
API decision, and compensation additionally needs an answer to a question the engine cannot
currently answer (§4 below).

**Not a production-readiness statement.**

## 1. Provenance

| Field | Value |
|---|---|
| Commit | `fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac` — **matches expected**, unchanged |
| Stash | `stash@{0}` CommitDecision batching — **untouched** |
| Engine image | `durexec-engine:phase8`; simulator `durexec-extsimd:phase7` |
| Namespace / DB | `durexec-lab` / database `durexec` — the only database in the lab; the drill used a separate `durexec_drill`, since removed |
| Checks | `gofmt` clean · `go vet` clean · **27/27 packages pass** · `-race` clean on faultinj, extsim, runtime, activity, taskq |
| Raw evidence | `docs/results/phase8-boundaries.txt`, `docs/results/phase8-restore-drill.txt` |

## 2. Status of every scenario

| # | Scenario | Status | Evidence |
|---|---|---|---|
| H | Effect confirmed → **real SIGKILL** → before result journal | **PASS** | seam log line found; `Requests=2 Performed=1 Replayed=1 Injected=0`; 1 journal completion; 0 duplicate `(run_id,seq)` |
| I | Ownership moves while the effect is in flight; stale worker then journals | **PASS** | fencing token 63→64 while the call was blocked; `Requests=2 Performed=1 Replayed=1`; 1 distinct key; 0 duplicate effects; status COMPLETED |
| I-a | …original operation finishes **before** the replacement invocation | **NOT TESTED** | Needs a seam that *pauses* at the journal boundary; only a killing seam exists. Not inferred from fencing tests |
| — | Cross-run idempotency (explicit restart / continue-as-new reuse) | **NOT TESTED — by design** | Requires the `dx2:` key scheme and an API decision; proposed in the design doc, deliberately unbuilt |
| — | Compensation for a confirmed external effect | **BLOCKED** | Blocked on resolving ambiguous outcomes (§4). Tripwire tests added instead |
| — | Backup + restore usability | **PASS** | dump 2.0 s → 24.6 MB; restore 4.3 s; all row counts and the status breakdown match exactly; 0 duplicate journal records |
| — | Point-in-time recovery | **BLOCKED** | `archive_mode=off`, `archive_command` disabled, `restore_command` empty — PITR does not exist |
| — | Restore into a genuinely isolated instance | **NOT TESTED** | The drill restored into a second database in the **same** postmaster, disk and volume |

## 3. H — effect confirmed, then real process death before the journal

A new crash point, `BeforeResultJournal`, sits immediately before
`e.Committer.CommitDecision` in the activity worker. At that instant the activity has
returned, so the effect **has happened and been confirmed**, and nothing durable records it.

It is the only seam in the repository that **kills the process**, because §5 rules out the
alternative: returning an error leaves memory intact and so cannot model losing the knowledge
that a call succeeded. It uses `SIGKILL` to self rather than `os.Exit`, so no defer, drain or
buffered write survives. The kill lives in exactly one audited function,
`faultinj.KillProcessIfArmed`, behind the same three gates as every other seam — inert
injector by default, explicit `FAULT_INJECTION=true`, point named in `FAULT_CRASH_AT`,
validated by `config.Validate`. **No endpoint can reach it.**

Measured: effect confirmed (`Performed=1`) → seam fired → task redelivered → second
invocation under the **same journaled key** → recipient absorbed it → **one effect, one
journaled result**. `Injected=0` is what makes this attributable: the simulator injected
nothing, so the second invocation can only have come from the engine having lost the result.

Two honest caveats. The container restart count read 0/0 because the detection loop exits on
the log signal before the kubelet updates it; the kill is evidenced by the seam line plus the
otherwise-unexplainable second invocation. And the seam is armed **per process, not per
workflow**, so it kills on the first activity result *anywhere* — which is why the scenario
now refuses to arm until the cluster is quiescent.

## 4. I — ownership loss mid-flight, and what actually protected it

Ownership was moved by **pausing** the owner, not killing it: a paused process cannot renew
its lease, so the lease expires and the other node adopts the shard — and the paused worker
is still *alive*, so on thaw it wakes believing it still owns the shard. Killing it would
have destroyed the very worker whose stale write was under test. Determinism came from the
simulator holding the response 75 s, longer than `LEASE_TTL` (10 s) + the 2×TTL adoption
guard + `TASK_LOCK` (30 s), so ownership and the claim both moved while the call was blocked.

**The finding:** `stale-write refusals observed in logs: 0`. The stale worker never attempted
a commit. The log says why:

```
"activity task claim lost while running, abandoning the result"
shard=12 workflow_id=p8-I-… activity=ExternalPayment task_id=567634
```

So the protection that fired was the **task-claim check**, which runs *before* the commit —
not fencing. Fencing is the second line of defence here and was never reached. Phase 9's
fencing evidence stands on its own and is **not** what made this scenario safe. Reporting
otherwise would have credited the wrong mechanism.

## 5. Database durability — what exists, and what is simply absent

| Setting | Value | Consequence |
|---|---|---|
| `fsync`, `full_page_writes`, `synchronous_commit` | **on / on / on** | A committed transaction **is** durable on local disk |
| `archive_mode` / `archive_command` / `restore_command` | **off / disabled / empty** | **No WAL archiving, therefore no PITR.** RPO can never be better than the last dump |
| `synchronous_standby_names` | **empty** | Any standby would be **asynchronous** — promotion can lose acknowledged commits |
| `wal_keep_size` | **0 MB** | A standby that falls behind cannot catch up; it must be rebuilt |
| `data_checksums` | **off** | **Silent corruption would not be detected**, by PostgreSQL or by a backup |
| `wal_log_hints` | **off** | `pg_rewind` is unavailable, so a demoted primary cannot be re-attached |
| Storage | 8 Gi PVC, `k8s.io/minikube-hostpath`, `ReclaimPolicy: Delete` | Data lives on **one VM's disk**. No redundancy, no snapshots, and deleting the PVC deletes the data |
| Standby in this lab | **none** (only the compose deployment has one) | Replication lag remains unmeasurable here |

**Restore drill — PASS, with its limits stated.** 247 MB → 24.6 MB dump in 2.0 s;
`pg_restore -l` enumerated 30 objects with no error; restore in 4.3 s; `executions`
46793=46793, `events` 561170=561170, `shard_leases` 16, **0** duplicate
`(shard, workflow, run, seq)` groups, status breakdown preserved. The drill database was
dropped afterwards.

What the drill does **not** show, and must not be read as showing:
- It restored into a second database in the **same instance** — same postmaster, same disk,
  same potential corruption. Recovery from losing the instance, volume or node is **NOT
  TESTED**.
- `tasks=0` and `activity_intents=0` in the dump. A dump taken while work is **in flight**
  captures executions with **no driving task row**, so those runs are stranded after restore
  until the reconciler adopts them. That path was not exercised.
- **Restoring to an earlier point rewinds the journal behind external reality.** Activities
  whose effects already happened would replay and be re-invoked. They carry the *same*
  journaled key, so a key-honouring recipient absorbs them — which makes the Phase 7
  conditional guarantee **load-bearing for disaster recovery too**, not just for retries.

**Recovery requirements (proposed, none implemented):** `archive_mode=on` with WAL archiving
to off-node storage, enabling PITR; scheduled `pg_dump -Fc` plus a periodic
restore-and-verify into a *separate* instance, because a dump that has never been restored is
not a backup; `data_checksums=on` (requires `initdb` or `pg_checksums` on a stopped cluster —
**authorization required**, not done here); `wal_log_hints=on` to permit `pg_rewind`;
`synchronous_standby_names` set if zero acknowledged-commit loss is required, accepting the
write-latency cost. Measured here: dump 2.0 s and restore 4.3 s at 247 MB, so RTO is
dominated by detection and decision, not by the restore. **RPO today is unbounded: no backup
is scheduled and no WAL is archived.**

## 6. Code changes

| Change | Rationale |
|---|---|
| `faultinj.BeforeResultJournal` + `KillProcessIfArmed` + `CrashPoint.KillsProcess()` | The §5 boundary needs real process death. One audited kill function; `KillsProcess()` exists so a harness knows it must disarm immediately or the node crash-loops |
| `internal/runtime/activity_worker.go` | The seam, immediately before `CommitDecision` |
| `internal/faultinj/crashpoint_test.go` | `KillProcessIfArmed` must not even call its pre-kill callback when disarmed — the observable proxy for "the kill was unreachable" |
| `internal/extsim/probe_test.go` | **Tripwire:** fails if anyone registers a compensation on `ExternalPayment`, pointing at the design doc. Plus a test that `fail_after` genuinely fails, so a run leaving a payment un-refunded cannot look clean |
| `08-DESIGN-external-effect-contract.md` | Cross-run key scheme (`dx2:`) and compensation semantics, written down rather than invented in code |

No change was made to the existing `dx1:` key format, to replay semantics, or to replication
mode.

## 7. Harness defects — three in scenario H alone, all preserved

| # | Defect | Why it mattered |
|---|---|---|
| 1 | Armed the killing seam while a Phase 7 workflow was still on a 12-minute timer | That unrelated workflow's activity result killed both nodes; the probe's own start got HTTP 000. Fixed with a quiescence precondition |
| 2 | Quiescence query used `NOT IN` with single quotes nested inside a double-quoted command substitution | The escaping collapsed and the predicate matched **every** row: reported `nonterminal=46791` and BLOCKED an idle cluster. Rewritten as a positive status list needing no escaping |
| 3 | Checked for the seam's log line **after** the disarm rollout had replaced the pod | `--previous` then pointed at the wrong container, so the kill looked unconfirmed. Both signals are now captured before any restart — the same evidence-destroying mistake as Phase 7's `disarm()` |

Also: `kubectl apply -k` reset `EXTSIM_URL` to its new empty default when I patched it *before*
applying — the three-way-merge trap I documented in Phase 6 and then walked into.

## 8. Resource constraints

Host swap reached 100% again. Prometheus and `postgres-exporter` remain scaled to **0**
throughout this phase, which is why no metric-derived figures appear above. Restore with
`kubectl -n durexec-lab scale deploy/prometheus deploy/postgres-exporter --replicas=1`. The
durable fix for the host is moving `UV_CACHE_DIR` off `/tmp`, which is tmpfs and therefore
RAM; `/home` has 63 GB free and would serve.

## 9. Remaining release blockers

1. **Exactly-once external effects: unproven and unprovable for arbitrary recipients.** The
   contract is at-least-once invocation plus effectively-once effect **iff** the recipient
   honours the key.
2. **No compensation for external effects.** A failed saga leaves one standing. Blocked on
   resolving ambiguous outcomes, not on plumbing.
3. **Keys are run-scoped.** A re-run under the same business key can repeat an effect.
4. **No PITR, no scheduled backup, no off-node copy, no checksums.** RPO unbounded against
   volume loss.
5. **Asynchronous replication**; promotion can lose acknowledged commits; database failover
   9–15 s and manual.
6. **No importable SDK, no workflow versioning story.**
7. Horizontal autoscaling impossible by design (R-2); 9 commits unpushed and CI never run.
