# Phase 7 — External-Effect Correctness and Recovery

**Verdict first. Exactly-once external effects are NOT proven, and cannot be proven in
general.** What is now proven, on the real engine, is **at-least-once invocation with
effectively-once effect, conditional on the recipient honouring an idempotency key.** The
engine's contribution is a correct one: a stable key, derived from `(scheme, workflow_id,
run_id, k)`, journaled into the scheduled event and therefore byte-identical across every
retry, replay and crash. The deduplication itself is done by the **recipient**. Against a
service that ignores the key, every scenario below that shows one effect would show two.

**Second finding: this had never run.** `internal/extsim` was referenced only from
`internal/runtime/*_test.go` — zero references from `cmd/engined`. `ExternalPayment` was a
carefully built activity that **nothing could schedule**, because only a workflow can
schedule an activity and no shipped workflow named it. The mechanism the project's central
claim rests on had never executed inside the engine. It does now.

**Not a production-readiness statement.**

## 1. Provenance

| Field | Value |
|---|---|
| Commit | `fde6ae096ccb4f4d1e12650fe3cfc0f3ac49c2ac`, unchanged. Phase 6 + 7 edits uncommitted |
| Stash | `stash@{0}` CommitDecision batching — **untouched** |
| Engine image | `durexec-engine:phase7`; simulator `durexec-extsimd:phase7` |
| Cluster | minikube kvm2, 2 vCPU / 3.72 GiB, namespace `durexec-lab` (isolated; the only database is this lab's `durexec`) |
| Checks | `gofmt` clean · `go vet` clean · **27/27 packages pass** · `-race` clean on extsim, activity, taskq, runtime, config, api |
| Raw evidence | `docs/results/phase7-extsim-matrix.txt` |

## 2. The activity lifecycle, and where a crash lands

`task claimed → activity invoked → EXTERNAL EFFECT → result journaled → task deleted`

| Boundary | Persistence | On process death here |
|---|---|---|
| before invoke | task row claimed, `locked_until` set | claim lapses after `TASK_LOCK` (30 s), task redelivered, **no effect yet** |
| during invoke | nothing | effect may or may not have occurred — **ambiguous**; retry under the same key |
| after effect, before journal | nothing in the engine; effect exists externally | retry re-invokes; safety depends **entirely** on the key |
| after journal, before ack | result durable in `events` | replay returns the journaled result; the activity is **not** re-invoked |

The dedup key is computed once at decision time (`internal/workflow/decision.go:96`), stored
in the `ActivityTaskScheduled` event, and read back by the worker as `sched.DedupKey`
(`internal/runtime/activity_worker.go:139`). It is **not** recomputed at invocation time,
which is what makes it survive replay. `internal/activity/dedup.go:53` length-prefixes every
field, so a separator inside a workflow id cannot make two runs collide.

## 3. Guarantees: what the engine actually provides

| Guarantee | Supported? | Under what assumption |
|---|---|---|
| At-most-once invocation | **No** | Retries are intrinsic; an ambiguous timeout *must* be retried |
| At-least-once invocation | **Yes** | Unconditional |
| Durable result recovery | **Yes** | Proven: scenario C, `Requests` stayed at 1 across a crash |
| Effectively-once effect | **Yes, conditionally** | **Only if the recipient deduplicates on `Idempotency-Key`** |
| Exactly-once external effect | **No** | Not achievable for arbitrary external systems |

Two further limits are structural, not bugs:
- The key contains `run_id`. A **new run** of the same business key gets a **different** key,
  so a restarted-as-new-run workflow can repeat an effect. Within a run it is stable.
- A unique journal row is **not** evidence an effect happened once. The journal write and
  the HTTP call are not one atomic action and cannot be made so.

`ExternalPayment` **refuses to call** when no key is present (`ErrNoDedupKey`,
non-retryable). That is the right trade: a loud workflow failure instead of a silent
double-charge.

## 4. Failure matrix — measured, with the fault confirmed fired

Each scenario reads two independent sources: PostgreSQL, and the simulator's ledger in a
**separate pod** that survives the engine being killed. `Requests` counts invocations;
`Performed` counts effects; `Replayed` counts invocations the recipient absorbed.

| # | Scenario | Req | **Effects** | Replayed | Journal | Status | Result |
|---|---|---|---|---|---|---|---|
| A | Effect lands, response discarded (**ambiguous**) | 2 | **1** | 1 | 1/1 | COMPLETED | **PASS** |
| B | Transient failure ×2 *before* the effect | 3 | **1** | 0 | 1/1 | COMPLETED | **PASS** |
| C | Result journaled, then crash before ack | **1** | **1** | 0 | 1/1 | COMPLETED | **PASS** |
| D | Crash before the operation is ever invoked | **0** | **0** | 0 | 0 events | — | **PASS** |
| E | Later step fails after the effect landed (saga) | 1 | 1 | 0 | 1/1 | **FAILED** | **PASS, with a gap** |
| F | Task claim expires mid-flight (45 s hang > 30 s lock) | 2 | **1** | 1 | 1/1 | COMPLETED | **PASS** |
| G | **Node-level SIGKILL of the owner** mid-flight | 2 | **1** | 1 | 1/1 | COMPLETED | **PASS** |

Every row: **0 duplicate `(run_id, seq)` groups, 0 pending tasks, 0 duplicate effects.**
Fault firing was confirmed per scenario — simulator `Injected` counts for A/B/F/G, the
engine's `injected crash` log line for C, and HTTP 500 plus zero rows for D.

**A is the central result.** The effect landed, the response was thrown away, and the engine
retried because it could not know otherwise. Two invocations produced **one** effect, and the
journal recorded one result.

**F is the narrow window.** The effect had landed and was **not yet journaled** when the
claim expired, so the task was genuinely delivered twice. The key absorbed the second
delivery.

**G used a real SIGKILL** (`docker kill --signal=KILL` inside the node), not a pod delete,
which would send SIGTERM and allow a graceful drain. Honest caveat: by the time the kill
landed the result had **already been journaled**, so G demonstrates durable result recovery
across real process death. The narrower window "effect landed, result not journaled, process
SIGKILLed" was **not hit deterministically** — F covers the same window via claim expiry, but
without loss of in-memory state. See §7.

### E: the gap worth stating plainly

The effect landed, a later step failed, the workflow ended **FAILED**, and **0 compensation
events ran**. That is correct behaviour for the code as written: `ExternalPayment` registers
no compensation, deliberately. `PayDefinition`'s own comment says why — a refund is itself an
external effect needing its own key, and **the relationship between an effect's key and its
compensation's key is a design question this engine has not answered.** So a failed saga can
leave a real external effect standing with nothing to undo it.

## 5. Code changes

| Change | Why |
|---|---|
| `cmd/extsimd/` (new, test-only binary) | The ledger must outlive the engine. An in-process simulator means SIGKILLing the engine destroys the only record of whether the effect happened — evidence and subject sharing a fate |
| `internal/extsim/probe.go` (new) | `ExternalEffectProbe`, the only workflow that schedules `ExternalPayment`. Without it the activity was unreachable |
| `EXTSIM_URL` in `internal/config` | Opt-in gate, validated as an http/https URL with a host. A malformed value would fail at call time as a *transport error*, which the retry policy reads as **ambiguous** — the one verdict a typo must never manufacture |
| `cmd/engined/main.go` wiring | Registers the pair only when `EXTSIM_URL` is set, and logs a WARN naming both as test-only |
| `20-postgres.yaml` probe fix | **Defect.** Probes never set `timeoutSeconds`, so it defaulted to **1 s**; `pg_isready` timing out on a busy node made the kubelet kill a **healthy** database **25 times** (186 readiness timeouts logged). Now 5 s readiness / 10 s liveness at a 30 s period, and liveness is deliberately far more forgiving than readiness because liveness *restarts PostgreSQL* |
| CPU requests trimmed | The node sat at **98%** of 2000m subscribed, so every probe competed with the workload it measured |

**Safety of the test-only path, asserted not asserted-in-prose:** `app/` and
`internal/runtime/` never name either symbol; only `cmd/engined` calls `RegisterProbe`, gated
on `cfg.ExtsimURL != ""`; `base/10-config.yaml` ships `EXTSIM_URL` **empty**; and
`TestProbeAbsentUnlessRegistered` plus `TestNoExtsimByDefault` pin both halves. **No endpoint
was added to `engined`.** The simulator's `/arm` endpoint is unauthenticated and lives only
in the isolated lab namespace, never in the engine image.

## 6. Commands actually executed

`gofmt -l .` (clean) · `go vet ./...` (clean) · `go test ./... -p 2` → **27 ok, 0 fail** ·
`go test -race` on extsim/activity/taskq/runtime/config/api → all ok ·
targeted run of the 11 fault-injection and config safety tests → all PASS ·
`experiments/scripts/extsim-matrix.sh all` against the live cluster ·
live duplicate-start check → `accepted=1, already_open=2` on the owner and `not_owned=3` on
the forwarding node, i.e. the Phase 6 counter and its documented double-count caveat both
still hold.

## 7. Failed, invalid and preserved runs

| # | Problem | Consequence | Resolution |
|---|---|---|---|
| 1 | My ledger reader used double quotes inside a single-quoted shell string nested in a function | **Every** scenario recorded `(ledger unreadable)` while still printing PASS-shaped output around it | Rewritten as `read_effects.py`. The `counters` line carried the decisive numbers, so evidence was *degraded, not wrong* — but it is the same silent-failure pattern as the earlier `-c engine` and `events.type` mistakes |
| 2 | Postgres killed 25× by its own liveness probe | Cluster unusable; engine rollout blocked on the init container | Probe timeouts fixed (§5) |
| 3 | Host swap reached **100%**, qemu thrashing | API server stopped answering for >4 min | `minikube stop`/`start` (VM disk and PVC preserved, profile **not** recreated); Prometheus and postgres_exporter scaled to **0** for this phase |
| 4 | G's SIGKILL landed after journaling | The "effect landed, not journaled, process killed" window was not hit | Recorded as **NOT TESTED deterministically**; F covers the redelivery half |

## 8. NOT TESTED

§4.7 lease ownership change *during* execution and §4.8 a stale worker journaling after
losing ownership were **not** re-tested here. Phase 9 proved the fencing half (0 stale
tokens, 0 orphans, 0 duplicate journal records) by freezing and thawing a process, but not
with an external effect in flight. §4.11 duplicate delivery was covered only indirectly, via
F. These are recorded as untested rather than inferred from the fencing result.

## 9. Remaining limitations and blockers

- **Exactly-once external effects remain unproven and unprovable in general.** The supported
  contract is at-least-once invocation plus effectively-once effect **iff the recipient
  deduplicates on `Idempotency-Key`**.
- No compensation story for external effects — a failed saga can leave one standing (§4 E).
- The dedup key is scoped to a **run**, so a re-run under the same business key can repeat an
  effect.
- No durable intent/outbox record, and no reconciliation path for an outcome that stays
  ambiguous after retries are exhausted.
- `extsim` is a **simulator**: it deduplicates because a mutex makes it, standing in for a
  unique index. No real provider has been tested.
- Unchanged from Phase 6: no importable SDK, no workflow versioning, no backup/PITR,
  asynchronous replication, database failover 9–15 s and manual, autoscaling impossible by
  design (R-2), 9 commits unpushed and CI never run.
