# Audit 01 — Durable execution, recovery correctness, fault tolerance

Auditor: Agent 1 of 3, independent production-readiness review.
Repository state: `main` @ `3052fa7`, working tree clean at the start of the audit.
Remit: durable execution guarantees, recovery correctness, fault tolerance.
Out of remit: raw performance, the semantic comparator's accuracy, security,
dashboards, build hygiene.

---

## 1. Executive summary and verdict

### Verdict for this remit: **NO-GO**

One defect is enough to carry that on its own, and it is not a theoretical one.

**`StartWorkflow` is not atomic, and the live demo cluster contains 19 workflows
that were accepted, acknowledged, and silently abandoned two days ago.** They
sit in `executions` with `status = 'RUNNING'`, `last_seq = 0`, zero events, zero
task rows and zero timers. Nothing in the engine will ever touch them again —
there is no reaper — and the API reports them as `RUNNING` forever. If this
engine were running real orders tomorrow, those would be 19 paid-for orders that
no system believes are missing. This is finding **F-01**, confirmed by code
reading and then by direct query against the running database.

That single finding contradicts the project's headline durability claim in the
plainest possible way, and it was found by a query the existing evidence suite
never runs.

The verdict is **not** a judgement that the design is bad. It is not. The core
of this engine — one `CommitDecision` transaction, the fencing token re-read
`FOR UPDATE` inside that transaction, the events primary key as the real
concurrency control, and full replay from history on every task — is correct,
carefully reasoned, and in several places better than the comments claim. The
`SeqRaceError` / `FencedError` split and the `keepClaimed` renewal added in
`2bffefd` and `9223122` are both genuine, correct fixes and I verified them
rather than assuming. The engine's *recovery* story is sound. Its *admission*
story is broken, and its *operator-recovery* story is missing.

### What actually holds, stated precisely

The brief asked for a rigorous separation. Here it is, and the differences
matter more than the headline:

| Guarantee | Does the implementation provide it? |
|---|---|
| **Exactly-once workflow state transitions** | **Yes**, for a workflow that is successfully started. Enforced by `PRIMARY KEY (shard_id, workflow_id, run_id, seq)` on `events` plus the fencing check inside the same transaction. Two racing writers cannot both land; the loser rolls back entirely. This is real and it is the project's strongest result. |
| **Exactly-once task completion records** | **Yes.** A task row is deleted in the same transaction as the events it produced (`commit.go:186`, `commit.go:255-259`), so "the task finished" and "here is what it decided" are one fact. |
| **At-least-once activity invocation** | **Yes** — and this is the ceiling, not a limitation to be engineered away. Correctly documented at `activity_worker.go:16-27`. |
| **Exactly-once external effects** | **No**, and it cannot be. Correctly admitted in the code. |
| **Effectively-once via downstream idempotency** | **Designed, plumbed, and never once exercised.** `activity.DedupKey` is a correct, stable, length-prefixed key, and it is delivered to activity implementations through `activity.Info`. **No activity in this repository reads it.** `grep` for `InfoFromContext` across `app/` returns nothing. Every shipped activity is an in-process simulation that sleeps and returns a generated id; there is no external system, no HTTP call, and therefore no idempotency key ever leaves the process. See **F-02**. |

So the honest statement of the execution model is: *exactly-once workflow state
and task records, at-least-once activity invocation, and a dedup-key mechanism
whose effectiveness is entirely untested.* The project's own documentation says
the first three. It presents the fourth as working.

### The three most serious findings

1. **F-01 (Critical, confirmed empirically).** `StartWorkflow` writes the
   `executions` row in one transaction and the start event plus the first task
   in another. A crash, a lost lease or any commit error between them orphans
   the workflow permanently, and a client retry is answered `200 already_open,
   status RUNNING`. 19 live instances on the demo cluster.
2. **F-02 (Critical, confirmed).** The "zero duplicate completed steps across
   3,512 completed activities" evidence cannot detect a duplicate external
   effect, for a structural reason: **every retry path in the engine leaves no
   event at all.** `releaseForRetry`, the claim-lost abandon, the fenced abandon
   and a lapsed task lock all return without journaling anything. The event log
   therefore cannot record that an activity ran twice, and no metric counts
   invocations. The claim is true and is evidence of something — journal
   integrity — but not of what the write-up uses it for.
3. **F-03 (High, confirmed).** There is no workflow versioning of any kind, and
   on divergence the worker *immediately commits a terminal failure*
   (`workflow_worker.go:151` → `failTerminally`), which directly contradicts
   `dispatcher.go:140-141`'s claim that history is left untouched so an operator
   can redeploy the old code. A rolling deployment that changes workflow code
   terminally fails every in-flight workflow that lands on a new node — with no
   compensation run, because the status goes straight to `FAILED`.

### What I could not assess

- **Whether any duplicate external effect has ever occurred.** Unobservable by
  construction (F-02). No instrumentation exists that would answer it.
- **Database failover under a torn commit.** I did not kill the primary. The
  recorded drills are the only evidence and they do not check whether the
  truncated workflow resumed.
- **Long-horizon behaviour** (multi-day timers, signal waits measured in weeks).
  Nothing in the evidence runs longer than minutes.
- **The cause of one specific orphan** (`real-agent-1792350128`, shard 14). I
  can prove it is stuck; I cannot prove why, and I say so rather than guess.

---

## 2. Invariants

| # | Invariant | Status | What would raise it |
|---|---|---|---|
| I-1 | **Workflow durability** — an accepted workflow always reaches a terminal state | **VIOLATED** | Fix F-01 (fold the `executions` insert into `CommitDecision`). Then a test asserting no `RUNNING`/`last_seq=0`/no-task row can survive an injected failure, plus the orphan probe query added to the acceptance matrix. |
| I-2 | **Event-log ordering and collision-freedom** — no two writers append the same `(shard, workflow, run, seq)` | **PROVEN BY CODE AND TESTS** | Already strong: enforced by the primary key, not by application logic. `deploy/assert_no_duplicates.sql` confirms it was never bypassed. The audit code itself correctly labels this "enforced by the schema, not evidence". |
| I-3 | **At-most-one-owner-per-epoch** | **PROVEN BY CODE** | `acquireSQL` is a single conditional `UPDATE`; two racing acquires cannot both succeed. Verified by reading, not assumed. |
| I-4 | **Fencing-token monotonicity** | **PROVEN BY CODE** | `fencing_token = fencing_token + 1` on every acquire including re-acquire by the same owner; never reset (`releaseSQL` deliberately keeps the row). Live cluster shows tokens from 5 to 214, all monotone. |
| I-5 | **Fencing token checked at EVERY write boundary** | **PARTIALLY SUPPORTED** | It is checked at the one boundary that matters (`CommitDecision`) and at **none** of: `Queue.Poll`, `Queue.Extend`, `Queue.Release`, `Queue.Enqueue`, `Store.Create` (executions), `timers.Delete`. See F-09. Raising it requires either passing the token into those paths or accepting the documented argument that only `CommitDecision` can mutate workflow state — which is *almost* true, and the exception is that polling leads to external effects. |
| I-6 | **Activity result reuse** — a resolved activity is never re-run | **PROVEN BY CODE** | `resolvedAt(hist, sched.K)` at `activity_worker.go:107` plus the replay-mode check in `wfContext.replay`. Correct and belt-and-braces. |
| I-7 | **Task safety** — at most one worker executes a task at a time | **PARTIALLY SUPPORTED** | The `tasks` row lock is the real cross-node mutual exclusion and it works. The activity path now renews it (`keepClaimed`, fix verified). The **workflow path does not renew at all** — see F-07. Safety survives via I-2; liveness and wasted work do not. |
| I-8 | **Timer safety** — a timer fires at least once, never resolves a workflow twice | **PROVEN BY CODE** | `ScanShard` enqueues before deleting, and the duplicate wake-up is idempotent because history is the authority. The reasoning at `scanner.go:209-217` is correct. |
| I-9 | **Saga safety** — compensations run in reverse order, exactly once each | **PARTIALLY SUPPORTED** | The LIFO stack as a pure function of history is a genuinely elegant and correct argument — *given unchanged code*. It breaks silently under F-03/F-04. And a failed rollback is unrecoverable: see F-05. |
| I-10 | **Shard mapping stability** | **PROVEN BY CODE AND MEASUREMENT** | Verified independently this audit: adding a fourth node moves exactly 5 of 16 shards. The ring is order-independent and deterministic. See §5 claim 4 — the claim is true; the imbalance it comes with is not stated. |
| I-11 | **Deterministic replay** — same history, same commands | **PARTIALLY SUPPORTED** | Excellent within a single code version: the two-channel coroutine handover makes concurrency a non-issue, the clock is derived from history, `SideEffect` journals non-determinism, and map iteration is explicitly sorted in both `preface.go:275` and `dedup.go:279-286`. **Across versions it is unsupported**: no version markers, and the replay signature omits the activity input. See F-03, F-04. |
| I-12 | **No work is lost on graceful shutdown** | **PARTIALLY SUPPORTED** | Shutdown *ordering* is correct and carefully done (HTTP stop → drain → release leases). But `StopAll` **cancels** in-flight activities rather than draining them, so every rolling restart re-invokes whatever was in flight. See F-08b. |
| I-13 | **Operator recoverability** — any stuck state has a documented recovery path | **VIOLATED** | No reaper for orphaned executions (F-01), no resume for `COMPENSATING_FAILED` (F-05), no deadline on an `ESCALATE` hold (F-06). All three require direct SQL. |

---

## 3. Traced paths

All line numbers are at `3052fa7`.

1. **Creation → durable acceptance.** `api/handlers.go:136` `handleStartWorkflow`
   → `forward.go:89` `route` → `runtime/engine.go:104` `StartWorkflow` →
   `eventlog/store.go:112` `Create` (**separate transaction, no fencing check**)
   → `engine.go:121` `TokenFor` → `engine.go:152` `CommitDecision`. **Two
   transactions. This is F-01.**
2. **Decision → event-log commit.** `workflow_worker.go:136`
   `Dispatcher.Execute` → `workflow/decision.go:52` `BuildDecision` →
   `eventlog/commit.go:210` `CommitDecision`: `verifyToken` `FOR UPDATE` (a),
   events (b), tasks and timers (c), intents (d), execution pointer (e), task
   delete (f). One transaction. Correct.
3. **Event-log commit → activity execution.** `BuildDecision` emits
   `EvActivityScheduled` and an `ACTIVITY` task carrying that event's seq
   (`decision.go:116-123`) → `taskq/queue.go:134` `Poll` (CTE +
   `FOR UPDATE SKIP LOCKED`) → `activity_worker.go:98` `scheduledAt` reads the
   call back out of the log. The worker holds no memory across a crash. Correct.
4. **Activity completion → result persistence.** `activity_worker.go:120`
   `Executor.Execute` → `:153-176` build the result event **and the workflow
   wake-up task in the same Decision** → `:183` `CommitDecision`. The wake-up
   being in the same transaction as the result is the right call and is
   explicitly reasoned at `:167-169`.
5. **Worker crash → replay.** Task lock lapses (no action required by anyone) →
   another worker polls → `Events.Load` → `indexHistory` → re-execute from line
   one. `activity_worker.go:107` `resolvedAt` prevents re-running a resolved
   activity. Correct, and the "lease queue, not delete-on-read" choice
   (`migrations/0001_init.up.sql:39-40`) is what makes it free.
6. **Lease lifecycle.** `lease/store.go`: `acquireSQL:98` (single conditional
   UPDATE, token++), `renewSQL:126` (token in the WHERE clause — a fenced-out
   goroutine cannot keep an old claim alive), `releaseSQL:156` (owner NULL,
   expiry now, **token preserved**), `verifyTokenSQL:253` (`FOR UPDATE`).
   Policy in `lease/manager.go:108` `Decide` — expiry tested before renewal,
   which is the correct order. Handback at `:461`. All of this is right. The one
   gap: `Decide` compares the **local** clock against `LastRenew` with zero
   safety margin (`:112`), so a node can believe it owns a shard for up to one
   tick after the database says otherwise. Writes are safe (fencing); external
   effects are not. F-09.
7. **Task lifecycle.** `Poll:134` → `Extend:196` (**correctly refuses to renew a
   lapsed lock** via `AND locked_until > now()`) → `Release:220` → deleted
   inside `CommitDecision`. The `pollSQL` CTE comment (`:100-117`) documents a
   real, measured PostgreSQL `EvalPlanQual` trap and the fix is correct.
8. **Timer lifecycle.** `BuildDecision` writes `EvTimerStarted` + a `timers` row
   atomically → `timers/scanner.go:218` `ScanShard` enqueues then deletes →
   `runtime/preface.go:91-110` journals `EvTimerFired` **in the workflow
   worker's own commit**. Keeping the right to append to the log in one component
   is the correct design decision and is stated as such.
9. **Saga.** `saga/saga.go:140` `Compensate` runs each compensation as an
   ordinary journaled activity through `workflow.SagaRunner`. Stack rebuilt from
   history on every replay. Stops at first failure →
   `core.CompensationFailedError` → `dispatcher.go:153`
   `StatusCompensatingFailed`. Correct in isolation; unrecoverable in practice
   (F-05).
10. **Node join / reassignment / takeover.** `cmd/engined/main.go:223` ring →
    `:239` `Wants` (ring preference, or orphan adoption after `2 × TTL`) →
    `:263` `Releases` (hand back when the preferred owner's `/healthz` answers)
    → `OnLost` → `pool.go:214` `Stop`. `pool.go:267` `Supervise` reconciles
    workers against the lease manager's owned set. The `Wants`/`Releases` pair
    and the reasoning for why liveness comes from `/healthz` rather than the
    lease table are both correct. Observed live: node-a acquired all 16 shards
    at boot then released 12 — see F-10b.
11. **Database failover.** Manual only. `pg_promote()`, then nodes must be
    recreated because the engine cannot repoint `DATABASE_URL` without a
    restart. Async replication (`sync_state = async`). See §5 claim and
    scenario 5.
12. **Graceful shutdown.** `cmd/engined/main.go:416-427`: HTTP shutdown →
    `pool2.StopAll()` → `cancelBackground()` (releases leases). **Ordering is
    correct and deliberately so.** But `StopAll` cancels worker contexts, so
    "drain" is a cancel. F-08b.

---

## 4. Crash-point matrix

`D` = durable after the crash. `Ext` = may have happened externally.
"Auto" = recovers with no human action.

| # | Crash point | Durable | Ext? | Recovering node observes | Replays | Duplicate / lost / inconsistent | Auto |
|---|---|---|---|---|---|---|---|
| C-1 | Before the transaction | nothing | no | nothing happened | nothing | none | yes |
| C-2 | **After `executions.Create`, before `CommitDecision`** | the `executions` row only | no | `RUNNING`, `last_seq=0`, no events, no task | **nothing — ever** | **LOST WORK, silent, permanent** | **NO (F-01)** |
| C-3 | After `verifyToken` `FOR UPDATE`, before writes | nothing | no | lease row lock released by rollback | n/a | none | yes |
| C-4 | After writing events, before `COMMIT` | nothing | no | pre-state exactly, task row intact | the whole task | none — this is the designed path | yes |
| C-5 | **After `COMMIT`, before the client sees the response** | everything | no | a complete, consistent state | n/a | none in the engine; the **client** cannot tell success from failure. Retrying `StartWorkflow` is safe (`ErrWorkflowAlreadyOpen` → 200) | yes |
| C-6 | After a task is scheduled, before it is claimed | event + task row | no | a pollable task | the task | none | yes |
| C-7 | After claiming, before executing | task row, `locked_until` set | no | nothing for `TASK_LOCK` (30 s), then pollable | the task | none; 30 s delay | yes |
| C-8 | **DURING external execution** | event + task row | **YES, unknown** | a pollable task after the lock lapses | re-invokes with the **same dedup key** | **duplicate invocation certain; duplicate effect depends entirely on the downstream honouring the key — and nothing here does (F-02)** | yes (invocation), no (effect) |
| C-9 | **External system succeeded, result not yet persisted** | event + task row | **YES** | identical to C-8 — indistinguishable | re-invokes | **same as C-8. This is the engine's documented ceiling and it is honest about it** | yes |
| C-10 | During lease renewal (DB unreachable) | lease row unchanged | no | `LastRenew` not advanced → `ActionDeclareLost` at TTL → stand down | n/a | none; correct by design (`manager.go:140-145`) | yes |
| C-11 | **During lease expiry (node paused/GC)** | lease row expired | **possibly** | stale node still believes it owns the shard for up to one tick | polls and may execute | **writes refused (fencing); external effects NOT prevented.** Task row lock stops two nodes running the *same* task | partial (F-09) |
| C-12 | **During ownership handoff with work in flight** | events committed so far | possibly | fenced worker returns `errShardGone` **without releasing the task** | new owner waits out `TASK_LOCK` | no safety issue; **up to 30 s extra stall on top of the 20 s orphan-adoption wait** | yes, slowly (F-08) |
| C-13 | During timer expiry | timer row still present | no | timer still due | enqueues a second wake-up | duplicate task, harmless (history is authority) | yes |
| C-14 | **During compensation** | compensations completed so far | yes, partially | the same stack rebuilt from history, resuming at the failed step | remaining compensations | correct **unless** the retry budget ran out → `COMPENSATING_FAILED`, **terminal, no resume path** | NO (F-05) |
| C-15 | **During database failover** | everything the standby received | possibly | a history shortened by whole transactions | the lost transaction's pre-state, **including its task row** — so it self-heals | **no inconsistency** (WAL is atomic per transaction); **one extra activity invocation per lost transaction** | yes (see scenario 5) |
| C-16 | **During shutdown / rolling deployment** | everything committed | **yes** | released leases, released tasks | re-invokes in-flight activities | **duplicate invocation on every rolling restart** (F-08b); **plus terminal failure of in-flight workflows if the code changed** (F-03) | partial |

### Beyond process crashes

| Fault | Behaviour | Assessment |
|---|---|---|
| **Long GC pause / paused VM** | `Decide` stands the node down at TTL on the **local** clock; writes refused by fencing | Safe for state. C-11 applies to external effects. |
| **Delayed / reordered messages** | No inter-node messaging except HTTP forwarding, which is request/response | Not applicable — a real benefit of the no-broker design. |
| **Dropped response (C-5)** | Client cannot distinguish; `StartWorkflow` retry is idempotent on the business key | Good, and it is the reason the `executions` PK deliberately excludes `run_id`. |
| **Network partition (node ↔ DB)** | Node cannot renew, stands down at TTL; DB is the single source of truth | Correct. No quorum needed because there is no quorum. |
| **Network partition (node ↔ node)** | Only affects `/healthz`-based handback and request forwarding; `Releases` returns false, so a shard is simply not handed back | Degrades to imbalance, not incorrectness. Good. |
| **Stalled process between `Execute` and `CommitDecision`** | Claim lapses, another worker runs it, first worker's commit **collides on the event seq and rolls back** | Safe — I traced this specifically. The events PK catches it. |
| **Disk error / `fsync` failure** | `synchronous_commit=on` by default (`main.go` `newPool`), so a commit means durable on the primary | Correct default, and the comment explaining why it is not negotiable is right. |
| **Clock skew (node vs DB)** | `Wants`' orphan-adoption test mixes `time.Since` (local) with `expires_at` (DB), but `acquireSQL` re-checks on the DB clock | **Not a defect** — I checked this specifically and the database is the authority. Worth stating because it looks like one. |

---

## 5. The four claims, checked

| Claim | Verdict |
|---|---|
| "zero committed work lost after two nodes killed mid-run; 878 accepted workflows all completed" | **TRUE AS MEASURED, BUT THE MEASUREMENT CANNOT SEE F-01.** The audit counts runs *that have events* (`Audit` → `ActivityCompletedEvents` → `countRuns`). An F-01 orphan has no events, so it is invisible to both the numerator and the denominator. The live cluster has 19 of them. The claim should read "878 workflows that began executing all completed". |
| "zero duplicate completed steps across 3,512 completed activities" | **TRUE, AND CORRECTLY CAVEATED IN THE CODE, BUT USED FOR MORE THAN IT CAN BEAR.** `cmd/loadgen/duplicates.go:33-37` is admirably honest that the k-check is the one that matters. What is not stated anywhere: **no retry path writes an event**, so a duplicate *invocation* is structurally unrecordable. Zero duplicate completions is consistent with any number of duplicate invocations. F-02. |
| "state rebuilt from 200 events in 0.9 ms p99" | **NOT ASSESSED HERE** (performance; Agent 2). I note only that the replay path is pure in-process work over a `map[int]*histEntry`, so the figure is plausible, and that `ObserveReplay` measures `Dispatcher.Execute` only — not `Events.Load`, which is the database round trip and the part that actually scales with history length. The quoted number is the cheap half. |
| "adding a fourth node to a three-node cluster moves 5 of 16 shards" | **VERIFIED EXACTLY — and it is good evidence.** Independently recomputed this audit: 5 of 16 move, specifically shards 2, 3, 6, 8, 12. The 3-node distribution is also reproduced exactly on the live cluster (node-a 4, node-b 9, node-c 3). **What the claim omits** is that the same ring gives a 4/9/3 split — node-b carries 56% of the workload against an ideal 33% — and that losing node-b moves 9 of 16 shards. Minimal movement and even balance are different properties; only the first was measured. F-10. |

### Excluded and invalid runs — does anything hide a problem?

I checked each exclusion in `docs/results/README.md` against what it would
conceal. **The exclusions are honest.** `scaling-c16-GENERATOR-BOUND.csv` is
correctly diagnosed (closed-loop generator measuring itself: 16 ÷ 1.9 s ≈ 8.4
wf/s matches the plateau); keeping it is good practice, not concealment.
`duplicates-undisturbed.txt` is correctly called weak. The hybrid comparator's
1.000 is disclosed as measuring a hand-written extractor with the model never
consulted — that is a harder admission than most projects make. Run 1 of the
promotion drill is excluded for a stated, checkable reason.

**The problem is not what was excluded. It is what was never queried.** No
evidence file contains an orphan probe, a "RUNNING with no task row" check, or
any invocation counter. F-01 and F-02 survive in the gap between the
measurements, not behind an exclusion.

---

## 6. The ten scenarios

**1. Worker performs an external effect then crashes before persisting the result.**
Boundary: between `Executor.Execute` returning at `activity_worker.go:120` and
`CommitDecision` at `:183`. Durable: the `ActivityTaskScheduled` event and the
undeleted task row. The task lock lapses; another worker loads the same
`ActivityTaskScheduled`, `resolvedAt` is false, and the activity is re-invoked
with a **byte-identical dedup key** (pure function of workflow, run, k).
Invariant violated: **none in the engine.** This is the designed ceiling and it
is documented honestly. The actual gap is that the key's effectiveness is
unexercised (F-02) — the engine upholds its half of a contract whose other half
has never been connected to anything.

**2. Transaction commits but the client loses its connection.**
Boundary: after `tx.Commit` at `commit.go:261`, before the HTTP response. The
engine is consistent. The client cannot tell. A retried `StartWorkflow` is
answered `200 already_open` because the `executions` primary key excludes
`run_id` — a deliberate and correct design choice. **No invariant violated.**
The sting is that this same mechanism is what makes F-01 silent: the response
that means "you already have this" and the response that means "you already have
a corpse" are identical.

**3. Lease expires while the old owner is paused; a new owner starts processing.**
`Decide` (`manager.go:112`) stands the paused node down at TTL on its local
clock, with **no safety margin** against the DB's own expiry and only one
`Tick` (1 s) of resolution. So for up to ~1 s plus skew, two nodes believe they
own the shard. Every *write* by the stale node is refused — `verifyToken ... FOR
UPDATE` serialises ownership against the writes themselves, which is the correct
construction. Every *read and poll* succeeds. Invariant: I-3 and I-4 hold; I-5
does not. The reason this is not worse is accidental and worth stating: the
`tasks` row lock lives in the same shared database, so it serialises activity
execution across nodes **independently of the lease**. The lease is not what
prevents concurrent execution; the task lock is.

**4. Stale worker resumes after a newer fencing token was issued.**
`verifyToken` reads `fencing_token ... FOR UPDATE` and compares
(`commit.go:28-40`); mismatch → `FencedError` → the whole transaction rolls
back. `FencingCounter` (`:121`) counts it, and deliberately only counts a real
token mismatch, not a missing row or a seq race — which is exactly the right
discipline, because conflating them would put operational faults into the one
number that is supposed to be evidence against split brain. **Invariant holds.
This is the engine's best-executed mechanism.** Counterexample I ruled out:
could a stale worker write via a path with no token check? `Queue.Poll/Extend/
Release/Enqueue`, `Executions.Create` and `timers.Delete` all lack one — but
none of them mutate workflow history. `Create` is the exception that matters, and
it is F-01.

**5. PostgreSQL primary dies during a commit; the standby is promoted.**
Replication is async, so whole transactions at the tail are lost: measured at 2
and 1 events across runs 2 and 3, with "1 workflow present on both but behind on
promoted". **The important correctness point is not in the write-up:** because a
task row's deletion is committed in the *same* transaction as the events it
produced, losing that transaction restores the pre-state **including the task
row**. The affected workflow therefore self-heals — the task is polled again and
the step re-executes. A shortened history is not a stuck workflow; it is one
extra activity invocation. That is a genuinely good property of the design and
the project should claim it.

Two things remain wrong. First, the drill never checks it: it compares event
counts and then starts a *new* workflow, so whether the truncated workflow
actually resumed and completed on the promoted database is **NOT MEASURED**.
Second, failover is manual, takes 9–15 s, and the engine cannot repoint
`DATABASE_URL` without a restart — all correctly and plainly stated in
`docs/results/README.md`, and worse than AMBROSIA's 1.8 s automatic, as the
project's own CLAUDE.md insists on saying first.

**6. A workflow is replayed after its code changed between deployments.**
**This is the worst-handled scenario in the engine.** There are no version
markers, no `GetVersion`-style patch API, and no deployment gate. Two distinct
failures:

- *Divergence detected* (the activity type at some k changed): `wfContext.replay`
  → `diverge` → `NonDeterminismError` → `workflow_worker.go:151`
  **`failTerminally`, which commits `EvWorkflowFailed` and `StatusFailed`
  immediately.** `dispatcher.go:140-141` says returning the error "leaves
  history untouched, which is what lets an operator deploy the old code back and
  resume the run". The worker makes that false one call later. The workflow is
  terminal, and because the status is `FAILED` rather than reached through the
  workflow's own error handling, **no compensation runs** — forward steps that
  moved money are simply abandoned.
- *Divergence NOT detected* (F-04): the replay signature is
  `"activity:" + activityType` (`history.go:30`) and **does not include the
  input**. New code calling the same activity type at the same k with different
  arguments silently reuses the old recorded result. `ChargePayment(amount=100)`
  replayed under code that now asks for `amount=200` resolves from history as if
  200 had been charged. Silent and unbounded.

A third, milder case: `indexHistory`'s `default:` arm (`history.go:230`) errors
on an unknown event type, so an *older* node meeting a newer event type retries
the task forever rather than failing it — self-healing only if a newer node
eventually polls it.

**7. Two nodes process the same workflow or task simultaneously.**
Reachable (scenario 3), and safe. Two workflow workers on the same run both
replay, both compute decisions from the same `last_seq`, and both commit; the
events primary key makes one lose with `SeqRaceError`, whose whole transaction —
events, tasks, timers, intents, status, task deletion — rolls back. The loser
hands the task back and re-derives the same decision from the longer log. **I-2
and I-6 hold.** This is the mechanism that makes the no-coordinator design
defensible, and it is correct.

**8. A shard moves while its current owner has work in flight.**
Writes are refused. The gap is liveness: on the fenced path both workers
`return errShardGone` **without releasing the task** (`activity_worker.go:209`,
`workflow_worker.go:248`), so the row stays locked for the balance of
`TASK_LOCK` (30 s). Added to the deliberate `2 × TTL` (20 s) orphan-adoption
wait, a shard's in-flight work can be stalled ~50 s. `docs/results/README.md`
accounts for the 30 s it measured and attributes it to orphan adoption; the
unreleased-task component is not accounted for. F-08.

**9. A timer fires concurrently with cancellation, retry, or completion.**
Safe, by three independent mechanisms. `buildPreface` refuses to fire a timer
that is not yet due (`preface.go:96-100`) or already resolved (`:94`); a
`WORKFLOW` task for a terminal run is cleared without decisions
(`workflow_worker.go:97-103`, `dispatcher.go:102-109`); and cancellation is
delivered as a reserved `__cancel` signal through the same durable path as
everything else (`engine.go:195`), rather than as a second mechanism that would
also have to be made crash-safe. That last choice is right. One leak, not a
correctness issue: a timer row for a workflow that completed early is not
deleted until it fires.

**10. Compensation begins after some forward actions succeeded and another fails.**
Forward progress is correct: the stack is rebuilt from history on every replay
and a compensation is popped only after it succeeds, so a crash mid-rollback
resumes at the same step. The failure is what happens at the end.
`Compensate` stops at the first failure — correctly reasoned at
`saga.go:134-141` — and returns `CompensationFailedError` →
`StatusCompensatingFailed`, which `IsTerminal()` reports true. **Therefore
`Signal` refuses it** (`engine.go:166-168` returns `ErrWorkflowNotFound` for any
terminal run). There is **no engine API that can resume a half-rolled-back
saga**: not signal, not cancel, not start. The remaining compensations are owed
and unreachable. Worse, `core.StatusCompensating` is **dead code** — never
assigned anywhere in the repository — so a workflow that is actively rolling
back is indistinguishable from one running forward. F-05.

---

## 7. Findings register

### F-01 — `StartWorkflow` is two transactions; a failure between them orphans an accepted workflow permanently

- **Severity: Critical. Confidence: Confirmed (code + live database).**
- **Location:** `engine/internal/runtime/engine.go:104-156`;
  `engine/internal/eventlog/store.go:102-121` (`Create` uses `s.pool.Exec`, its
  own autocommit transaction, with **no fencing-token check**);
  `engine/internal/api/handlers.go:165-178` (reports the orphan as success).
- **The doc comment is false.** `engine.go:101-103` claims the row, event and
  task land "all in one Decision, so a start either fully happens or does not
  happen at all." There are two round trips.
- **Preconditions:** a node accepts `POST /v1/workflows` and then, within
  milliseconds, either dies, loses the shard (`TokenFor` → `!ok` at
  `engine.go:121`, which returns `NotOwnerError` **with the row already
  inserted**), or has `CommitDecision` fail for any reason.
- **Failure sequence:** (1) `Create` commits — `executions` now has the key as
  `RUNNING`, `last_seq=0`. (2) node is SIGKILLed / fenced / the commit errors.
  (3) no event, no task, no timer exists. (4) the client retries; `Create` hits
  the `(shard_id, workflow_id)` primary key → `ErrWorkflowAlreadyOpen` →
  `handlers.go:167` looks the row up and returns **`200 {status:"RUNNING",
  already_open:true}`**. (5) forever.
- **Impact:** silent, permanent loss of accepted work, reported as healthy. For
  the delivery saga: an acknowledged order never charged, never dispatched,
  never refunded. There is **no reaper** — I grepped `StatusRunning`,
  `exec_running`, `orphan`, `reap`, `stuck` across `internal/` and
  `cmd/engined/`; the `exec_running` partial index
  (`migrations/0001_init.up.sql:37`) serves only the admin list endpoint.
- **Live evidence (this audit, read-only query against `postgres-leader`):**
  19 executions with `status='RUNNING'` and **zero events**; 20 with no task row
  and no timer row; out of 11,807 executions total. All 19 have
  `created_at == updated_at` to the microsecond — never touched again — and were
  created on 2026-10-19 by the project's own `loadgen` runs. Oldest is
  ~2 days stale at the time of audit.
- **Reproduction (code, deterministic):** inject a `Committer` that returns an
  error, call `Engine.StartWorkflow`, then assert `Executions.Get` reports
  **not found**. Today it reports `RUNNING`.
- **Smallest correct fix:** add a `CreateExecution` field to `core.Decision` and
  perform the `INSERT INTO executions` inside `eventlog.CommitDecision`
  (`commit.go:210`), immediately after `verifyToken` and before `insertEvents`,
  mapping a unique violation to `core.ErrWorkflowAlreadyOpen`. `StartWorkflow`
  then becomes one `CommitDecision` call, and the start path gains the fencing
  check it currently lacks entirely. A compensating `DELETE` on error narrows
  the window but does not close it (the process can die between the two) and
  must not be presented as a fix.
- **Weaker alternative, if the above is too invasive:** a per-shard reconciler
  that finds `RUNNING` executions with `last_seq=0` older than a threshold and
  either enqueues a task or fails the run with a clear reason. This converts
  silent loss into visible, recoverable loss. State it as the weaker option.
- **Regression test:** `engine/internal/runtime/start_atomicity_test.go` — table
  test over commit-error, `ErrLostOwnership` and token-missing, asserting no
  `executions` row survives a failed start. **NOT WRITTEN — designed only.**
- **Blocks launch: YES.**
- **Counterexamples ruled out:** the timer scanner reads only `timers`
  (`scanner.go:218`); `WorkerPool.Supervise` reconciles workers against leases,
  never workflows against tasks (`pool.go:267`); every task insert is either
  inside a `Decision` (which requires a worker already on this run) or
  `Queue.Enqueue` from the timer scanner. A client that treated `already_open`
  as "unknown" would mask it — nothing documents that, and the shipped loadgen
  does not do it.

### F-02 — The duplicate-step evidence cannot detect a duplicate external effect, because no retry path writes an event

- **Severity: Critical (as a guarantee claim, not as a code defect). Confidence: Confirmed.**
- **Location:** `engine/internal/runtime/activity_worker.go:134-138`
  (claim-lost abandon), `:140-151` (`shouldRetryLater` → `releaseForRetry`),
  `:193-197` (seq race), `:207-209` (fenced) — **every one returns without
  journaling anything**; `engine/cmd/loadgen/duplicates.go:205-228` (`Audit`);
  `engine/internal/obs/metrics.go:56-110` (no invocation counter among the eight
  metrics).
- **Two independent holes.**
  (a) *Structural unobservability.* A duplicate invocation leaves **no trace**:
  the task row is released or left to lapse, no event is written, and
  `tasks.attempt` is destroyed when the row is finally deleted. The event log
  can only ever show one completion per k. "Zero duplicate completions" is
  therefore consistent with any number of duplicate invocations, and no metric
  closes the gap.
  (b) *The dedup key is never used.* `activity.DedupKey` is correct — stable,
  length-prefixed, a pure function of `(workflowID, runID, k)` with sound
  reasoning at `dedup.go:20-52` — and it is delivered via `activity.Info`. But
  `grep InfoFromContext` across `app/` returns **nothing**. Every shipped
  activity is an in-process simulation (`app/delivery/activities.go` sleeps and
  increments an `atomic.Uint64`). No HTTP call, no external system, no
  `Idempotency-Key` header anywhere in the repository. The mechanism the entire
  "effectively-once" argument rests on has never been executed end to end.
- **Impact:** the project's second headline claim does not support the
  conclusion it is used for. This is a reporting defect rather than a code
  defect, which is exactly why it is dangerous in a research write-up — an
  examiner who asks "how would a duplicate invocation have shown up in this
  data?" gets no answer.
- **Reproduction:** register an activity that counts invocations per dedup key
  in a durable table and forwards the key; run `duplicates` mode with node
  kills; compare invocations to completions. That number has never been
  measured.
- **Fix:** (1) add `activity_invocations_total{activity_type}` incremented in
  `Executor.Execute` before `invoke`, and a `redelivery` counter on every
  release/abandon path, so at-least-once becomes *visible* rather than merely
  admitted; (2) make one shipped activity a real idempotent HTTP call that
  forwards `Info.DedupKey`, with a stub server that counts distinct keys versus
  total requests. That single test turns the strongest claim in the project from
  an assertion into a measurement.
- **Regression test:** `engine/internal/activity/dedupkey_forwarded_test.go` plus
  a loadgen mode that reports invocations/completions. **NOT WRITTEN.**
- **Blocks launch: YES** for the claim; the engine itself can ship with the
  claim restated.

### F-03 — No workflow versioning, and divergence commits a terminal failure that skips compensation

- **Severity: High. Confidence: Confirmed.**
- **Location:** `engine/internal/workflow/context.go:153-160` (`diverge`);
  `engine/internal/workflow/dispatcher.go:138-147`;
  `engine/internal/runtime/workflow_worker.go:142-152` →
  `failTerminally:266-304`.
- **Preconditions:** any deployment that changes workflow code while workflows
  are in flight — i.e. every deployment, since these are long-running workflows.
  The project's own documented hazard "three nodes running three different
  builds" makes mixed-version replay the expected state during a rollout.
- **Failure sequence:** a `WORKFLOW` task for an in-flight run lands on a
  new-code node → replay reaches a k whose recorded signature differs →
  `NonDeterminismError` → `failTerminally` commits `EvWorkflowFailed` and
  `StatusFailed`. The run is terminal. **`CmdFailWorkflow` is never routed
  through the workflow's own error handling, so the saga never compensates** —
  forward steps that charged a card are abandoned with no refund.
- **Direct contradiction:** `dispatcher.go:140-141` states that returning the
  error "leaves history untouched, which is what lets an operator deploy the old
  code back and resume the run". The worker destroys that property one call
  later. Either the comment or the worker is wrong; the worker wins.
- **Impact:** a routine rolling deployment terminally fails in-flight workflows
  and strands their side effects. Irreversible effects are left un-compensated.
- **Reproduction:** start a workflow that schedules activity `A` at k=0; let it
  complete; change the workflow to schedule `B` at k=0; replay. Observe
  `FAILED`, not a recoverable park.
- **Fix (two parts, both needed):** (1) **Do not journal a terminal failure on
  divergence.** `releaseAfterFailure` instead, and raise a metric and a loud
  log; the run then parks and resumes correctly if the old code is redeployed —
  which is what the comment already promises. (2) Add a version marker: record
  a `code_version` in `WorkflowExecutionStarted` and either refuse to replay a
  run whose version this binary does not claim to support (routing it back to
  the queue), or provide an explicit `ctx.Version(changeID, min, max)` so new
  branches can be gated. (1) alone converts a data-loss bug into a stall, and
  is the smaller, safer change.
- **Regression test:** `engine/internal/runtime/divergence_is_not_terminal_test.go`
  asserting that a `NonDeterminismError` leaves the run `RUNNING` with its task
  re-released and history unextended. **NOT WRITTEN.**
- **Blocks launch: YES.**

### F-04 — The replay signature omits the activity input, so a changed argument silently reuses the old result

- **Severity: High. Confidence: Confirmed (code reading).**
- **Location:** `engine/internal/workflow/history.go:30`
  (`sigActivity = "activity:" + activityType`) and its only comparison site,
  `engine/internal/workflow/context.go:176-178`.
- **Preconditions:** new workflow code calls the **same activity type** at the
  **same k** with **different arguments**.
- **Failure sequence:** `replay(k, "activity:ChargePayment")` matches the
  recorded signature, `e.resolved` is true, and the **old** result is returned.
  The workflow proceeds believing the new call happened. No divergence is
  raised, no event is written, nothing is logged.
- **Impact:** silent incorrect reuse of a result across versions. In saga terms,
  a compensation registered on the strength of a result that describes a
  different action. Strictly worse than F-03's loud failure because it is
  undetectable from the log.
- **Why the input was reasonably excluded:** `context.go:313-314` and
  `workflow_worker.go:338-341` both note, correctly, that re-marshalling a
  decoded value is not guaranteed to produce identical bytes. So the input
  cannot be compared *byte-wise* safely.
- **Fix:** include a canonical digest of the input in the signature — hash the
  scheduled event's stored `input` bytes **as stored**, never a re-marshalled
  value. The stored bytes are stable by construction (the same argument
  `workflow_worker.go:338-341` makes for the workflow input). Compare the digest
  of the newly requested input against the recorded digest and raise
  `NonDeterminismError` on a mismatch. Must land together with F-03's fix, or it
  converts silent corruption into terminal failure.
- **Regression test:** extend `engine/internal/workflow/dispatcher_test.go` with
  a same-type/different-input replay case asserting divergence. **NOT WRITTEN.**
- **Blocks launch: YES** (jointly with F-03).

### F-05 — A failed compensation is terminal and unrecoverable; `COMPENSATING` is dead code

- **Severity: High. Confidence: Confirmed.**
- **Location:** `engine/internal/core/ids.go:27` (`StatusCompensating`, assigned
  **nowhere** — verified by grep across the repository), `:33` (`IsTerminal`
  includes `StatusCompensatingFailed`); `engine/internal/saga/saga.go:140-172`;
  `engine/internal/runtime/engine.go:166-168` (`Signal` refuses any terminal
  run).
- **Failure sequence:** forward steps 1..3 succeed; step 4 fails; `Compensate`
  unwinds; the compensation for step 2 exhausts its durable retry budget →
  `CompensationFailedError` → `StatusCompensatingFailed` → `IsTerminal()` true.
  The compensations for steps 2 and 1 are still owed. `POST /signal` →
  `ErrWorkflowNotFound`. `POST /cancel` → same path, same refusal. A fresh
  `StartWorkflow` on the business key → `already_open`.
- **Impact:** real-world state is known-inconsistent, the engine correctly says
  so, and then offers no route back. Recovery requires hand-written SQL against
  `executions` and `events` — on the one table the design declares append-only.
  Separately, because `StatusCompensating` is never set, a workflow actively
  rolling back reports `RUNNING`, so an operator cannot see a rollback in
  progress at all.
- **Fix:** (1) set `StatusCompensating` when the saga begins unwinding, so the
  state is visible; (2) treat `COMPENSATING_FAILED` as **non-terminal for
  signals** — allow a reserved `__resume_compensation` signal to re-enqueue a
  workflow task, which is sound because `Compensate` already resumes at the
  failed step from the rebuilt stack. The machinery to recover exists; only the
  door is locked.
- **Regression test:** `engine/internal/runtime/compensation_resume_test.go`.
  **NOT WRITTEN.**
- **Blocks launch: YES** if sagas with irreversible steps are in scope. Launch
  with compensating workflows disabled is defensible.

### F-06 — An `ESCALATE` hold is unbounded and indistinguishable from healthy work

- **Severity: Medium. Confidence: Confirmed.**
- **Location:** `engine/internal/runtime/dedup.go:235-272` (`applyOutcome`
  removes the activity task from `d.NewTasks` while the
  `ActivityTaskScheduled` event is still written);
  `engine/internal/runtime/preface.go:112-184` (release requires an
  `__adjudicate` signal).
- **Failure sequence:** the semantic check returns `ESCALATE`; the activity is
  scheduled in history but **no task row is created**; the workflow parks. With
  no timer and no deadline, it waits forever. Status: `RUNNING`.
- **Impact:** a held payment is neither taken nor released, indefinitely, and
  looks exactly like a healthy running workflow. It also produces the same
  database signature as an F-01 orphan (`RUNNING`, no task, no timer), which
  means any orphan-detection query an operator writes will be noisy unless it
  excludes held activities.
- **Fix:** write a `timers` row alongside the `ESCALATE` verdict so the hold has
  a deadline and a default action (release or fail, declared per activity), and
  expose `dedup_escalations_held` as a gauge. This also makes the orphan probe
  for F-01 unambiguous.
- **Regression test:** `engine/internal/runtime/escalation_deadline_test.go`.
  **NOT WRITTEN.**
- **Blocks launch: NO** — but it must be documented as requiring a human on call.

### F-07 — The workflow worker never renews its task claim (the fixed defect's shape, left in place on the other path)

- **Severity: Medium. Confidence: Confirmed.**
- **Location:** `engine/internal/runtime/workflow_worker.go:74-263` — no
  `keepClaimed` call, compared with `activity_worker.go:119`.
- **Verification of the `9223122` fix first, since the brief asked:** the
  activity-path fix is **correct and complete**. `keepClaimed` renews at
  `lock/3` (`claimRenewalFraction`), floors the interval
  (`minClaimRenewal`), treats **only** `taskq.ErrLockLost` as definitive,
  cancels the activity's context when the claim is definitively gone so an
  in-flight HTTP call stops rather than returning a result the worker may not
  commit, and `settleClaim` waits on the goroutine so the heartbeat cannot
  outlive the activity. `Extend`'s SQL carries `AND locked_until > now()`
  (`queue.go:191`), so a lapsed claim is never silently resurrected. The nine
  tests in `internal/runtime/liveness_test.go` cover the real cases, including
  `TestTransientRenewalFailureDoesNotAbandonTheActivity`. I could not fault it.
- **The same shape remains on the workflow path.** A workflow task holds its
  claim for `TASK_LOCK` (30 s default) across `Events.Load`, replay,
  `resolveSemantic` — **which makes a network call to the embedder** — and
  `CommitDecision`, with nothing renewing it. The embedder timeout is 2 s
  (`semantic/embedder.go:30`) and replay is fast, so 30 s is not easily
  exceeded today; a slow database, a long history, or several escalation
  candidates would do it.
- **Why it is Medium and not High:** safety holds via I-2. Two workflow workers
  on the same task both replay and both commit; the events primary key makes one
  lose with `SeqRaceError` and roll back entirely. This is the material
  difference from the activity path, where a lapsed claim meant a genuine
  concurrent external call. Here it is wasted work and latency.
- **Fix:** call `keepClaimed` in `WorkflowWorker.process` as the activity worker
  does. The helper is already general.
- **Regression test:** `engine/internal/runtime/workflow_claim_renewal_test.go`,
  mirroring `TestLongActivityRenewsItsClaim`. **NOT WRITTEN.**
- **Blocks launch: NO.**

### F-08 — A fenced worker abandons its task without releasing it, adding up to `TASK_LOCK` to every handover

- **Severity: Medium. Confidence: Confirmed.**
- **Location:** `engine/internal/runtime/activity_worker.go:207-209` and
  `engine/internal/runtime/workflow_worker.go:246-248` — both `return
  errShardGone` with no `w.release(...)`. Contrast `:84-88` / `:74-78`, where the
  pre-flight `TokenFor` failure **does** release.
- **Impact:** after an ownership change, in-flight tasks stay locked for the
  balance of `TASK_LOCK` (30 s). Added to the deliberate `2 × TTL` (20 s)
  orphan-adoption wait, a shard's in-flight work can stall ~50 s.
  `docs/results/README.md` explains the ~30 s shard unavailability as orphan
  adoption against NFR-4's 10 s target; the unreleased-task component is a
  second, unaccounted contribution to the same number.
- **Fix:** release the task before returning `errShardGone`. Safe: `Release`
  requires the matching `lock_token`, so it cannot disturb a task another node
  has since claimed, and the fresh context in `release()` already handles the
  cancelled-context case.
- **Regression test:** assert `tasks.locked_until IS NULL` after a fenced commit.
  **NOT WRITTEN.**
- **Blocks launch: NO.**

### F-08b — Graceful shutdown cancels in-flight activities rather than draining them

- **Severity: Medium. Confidence: Confirmed.**
- **Location:** `engine/internal/runtime/pool.go:241-251` (`StopAll` cancels
  every shard context, then waits); `engine/cmd/engined/main.go:421`.
- **The ordering is right and deliberately so** — HTTP stop, then drain, then
  release leases (`main.go:423-426`), so no shard is handed over while this node
  is still writing. That part is correct and I verified it.
- **But `StopAll` is a cancel, not a drain.** `pool.go:239-240` claims "a worker
  that is mid-decision finishes its commit"; in fact `sw.cancel()` cancels the
  context the activity is running under, `Executor.Execute` returns at its
  `ctx.Err()` check (`executor.go:172`), and `releaseForRetry` hands the task
  back. Every in-flight activity is therefore **re-invoked** after the restart —
  on every rolling deployment, by design rather than by accident.
- **Impact:** at-least-once invocation is guaranteed to bite on each deploy, not
  merely on a crash. With F-02 (no activity forwards the dedup key) there is
  nothing downstream to absorb it.
- **Fix:** give `StopAll` a bounded drain: stop *polling* immediately, let
  in-flight activities run to completion under a shutdown budget shorter than
  `shutdownGrace`, and only then cancel. Correct the comment either way.
- **Regression test:** `engine/internal/runtime/shutdown_drains_test.go`.
  **NOT WRITTEN.**
- **Blocks launch: NO**, but it must be stated in the operational notes.

### F-09 — The fencing token is not checked on any path except `CommitDecision`, so a fenced node can still invoke activities

- **Severity: Medium. Confidence: Confirmed.**
- **Location:** no token parameter in `taskq.Queue.Poll/Extend/Release/Enqueue`
  (`queue.go:134/196/220/273`), `eventlog.Store.Create` (`store.go:112`), or
  `timers.Store.Delete` (`store.go:105`). The only in-process guard is
  `e.TokenFor(w.Shard)` at the top of `process()`, which reads the lease
  manager's cached state.
- **Failure sequence:** a node is fenced (paused VM, DB stall). For up to one
  `Tick` plus clock skew, `TokenFor` still answers. The worker polls a task,
  **invokes the activity (external effect)**, and only then has its commit
  refused. Meanwhile the new owner cannot take that task until the lock lapses.
- **Why it is not worse, stated honestly:** the `tasks` row lock lives in the
  same shared database, so it serialises execution across nodes **independently
  of the lease**. Two nodes cannot run the *same* task concurrently unless the
  lock lapses — which is the hole `9223122` closed. The lease is not what
  prevents concurrent activity execution; the task lock is. The design comment
  at `pool.go:42-45` says as much.
- **What is genuinely wrong:** `Executions.Create` has no token check and is the
  one unfenced path that *does* mutate durable state. That is F-01's other half.
- **Fix:** F-01's fix (move `Create` inside `CommitDecision`) closes the only
  consequential case. For the rest, document that `CommitDecision` is the sole
  fenced write boundary and that polling is deliberately unfenced — the current
  comments imply a stronger property than the code provides.
- **Blocks launch: NO** once F-01 is fixed.

### F-10 — The ring gives a 4/9/3 split; losing the heaviest node moves 9 of 16 shards

- **Severity: Medium. Confidence: Confirmed (recomputed and matched against the live cluster).**
- **Location:** `engine/internal/shard/ring.go:18` (`DefaultVNodes = 150`);
  `engine/cmd/engined/main.go:223`.
- **Measured this audit** (`engine/internal/shard/audit_ring_distribution_test.go`,
  added by me — see §8):

  | Cluster | Distribution | Shards moved |
  |---|---|---|
  | 3 nodes | node-a 4, node-b 9, node-c 3 | — |
  | 4 nodes | node-a 3, node-b 6, node-c 2, node-d 5 | **5 of 16** (shards 2, 3, 6, 8, 12) |
  | remove node-a | node-b 9, node-c 7 | 4 of 16 |
  | **remove node-b** | node-a 8, node-c 8 | **9 of 16** |
  | remove node-c | node-a 5, node-b 11 | 3 of 16 |

  The 3-node row matches the live cluster's `/healthz` exactly (4 / 9 / 3), so
  the ring is behaving as computed — no silent misconfiguration, which is worth
  confirming given this project's history of exactly that.
- **Impact:** 150 vnodes evens out the *ring space*, but 16 keys is far too few
  for the law of large numbers to apply; the result is binomial noise. node-b
  carries 56% of the workload. Losing it moves 9 of 16 shards and makes 56% of
  in-flight workflows unavailable for the orphan-adoption window. The published
  claim "5 of 16 move" is **true and is good minimal-movement evidence**; it is
  reported without the imbalance the same ring produces, and blast radius is a
  fault-tolerance property, not just a performance one.
- **Fix:** either raise the shard count well above the node count (the usual
  remedy — 64 or 256 fixed shards would bring the split close to even and cost
  only more rows), or assign shards by an explicit balanced partition with the
  ring used only as a tiebreaker. Not a code defect; a parameter choice whose
  consequence should be stated.
- **Blocks launch: NO.** The claim should be amended.

### F-10b — Cold start: the first node to boot adopts all 16 shards, then hands 12 back

- **Severity: Low. Confidence: Confirmed (live metrics).**
- **Location:** `engine/cmd/engined/main.go:239-250` (`Wants`' orphan-adoption
  clause `time.Since(l.ExpiresAt) > 2*cfg.LeaseTTL`).
- **Evidence:** node-a's `lease_transitions_total` shows
  `acquired=1` for **all 16 shards** and `released=1` for 12 of them. On a cold
  cluster every lease is long expired, so orphan adoption fires for every shard
  and the first node up takes everything, then converges via `Releases`.
- **Impact:** for the first few seconds after a full cluster start, one node
  runs the entire workload at 1/3 capacity; 28 lease transitions and 28 fencing
  token increments per cold start. Benign but it explains the token skew
  observed live (shards 0–3 at token 5, shards 7 and 14 at **214**) and would
  otherwise look like an ownership flap. **I checked for an ongoing flap and
  there is none** — the counters are stable at `acquired=1` per shard.
- **Fix:** none required. Worth noting so the token spread is not mistaken for
  instability.
- **Blocks launch: NO.**

### F-11 — `clear()` and `complete()` can regress `executions.last_seq`

- **Severity: Low. Confidence: Unverified (code permits it; zero instances in 11,807 live executions).**
- **Location:** `engine/internal/eventlog/commit.go:175-180`
  (`updateExecutionSQL` sets `last_seq = $4` **unconditionally**);
  `engine/internal/runtime/activity_worker.go:406-414` (`clear`) and
  `engine/internal/runtime/workflow_worker.go:307-315` (`complete`), both of
  which pass an `ex.LastSeq` read in an earlier, separate statement.
- **Failure sequence:** worker A reads `ex` (`last_seq = 3`); worker B on the
  same node and token advances history to seq 10; worker A calls `clear` and
  sets `last_seq = 3`. Most readers are protected because they take
  `highestSeq(ex.LastSeq, hist)` (`workflow_worker.go:364`) — a deliberate guard
  with a correct comment. **`Engine.Signal` is not**: it computes
  `seq := ex.LastSeq + 1` (`engine.go:177`) from the pointer alone. With a
  regressed pointer, every signal collides on the primary key and fails, and a
  workflow parked on `WaitForSignal` can never be woken — because only a
  workflow decision repairs `last_seq`, and no decision will run until a signal
  arrives.
- **Live probe (this audit):** zero executions where `last_seq < max(event seq)`,
  across 11,807. Reachable in principle, not observed.
- **Fix:** make the update monotone —
  `SET last_seq = GREATEST(executions.last_seq, $4)`. One word, closes it
  completely. Separately, have `Signal` derive its seq from history rather than
  the pointer.
- **Regression test:** `engine/internal/eventlog/last_seq_monotonic_test.go`.
  **NOT WRITTEN** (needs a database).
- **Blocks launch: NO.**

### F-12 — The test suite still writes into the live database

- **Severity: Low (for this remit). Confidence: Confirmed.**
- **Evidence:** 48 rows in the live `tasks` table with `shard_id` 900–911,
  `workflow_id = 'wf'`, no matching `executions` row, written 2026-10-19
  18:20:05. `tasks_task_id_seq` is at 194,771 and continuous, so the table was
  not truncated — this is residue, not a reset.
- **Impact:** harmless in itself (no node owns shards 900–911, so nothing polls
  them), but it is the **fifth** instance of the class this project has already
  documented twice in `green-but-wrong-findings.txt` (the suite deleting four
  shards' lease rows; `/healthz` reporting `owned_shards=16`). It also means any
  evidence file produced from a database the suite has touched carries unknown
  pollution.
- **Fix:** make integration tests refuse to run unless pointed at a DSN whose
  database name ends in `_test`, and fail loudly otherwise.
- **Blocks launch: NO**, but it undermines every measurement taken from this
  database, which matters for a research write-up.

### F-13 — One workflow is stuck `RUNNING` with complete work and no task row, and there is no recovery path for that state

- **Severity: Medium. Confidence: Confirmed as stuck; cause NOT determined.**
- **Evidence:** `real-agent-1792350128`, shard 14, type `TravelAgent`, created
  2026-10-18 19:02, `last_seq = 13`, 13 events, **no task row, no timer**. Its
  history is complete and successful: `k=4` returned `{"done": true, "answer":
  "Your flight is confirmed! Booking ID: bk_000001..."}`. Only the final
  `WorkflowExecutionCompleted` is missing. Three days stale.
- **Why I will not name a cause.** The engine's own paths cannot easily produce
  it: a workflow task row is created in the *same transaction* as the activity
  result that precedes it, and the only code that deletes one without writing an
  event is `complete()` (which requires a terminal status this run does not
  have) or `clear()` (which only ever holds an `ACTIVITY` task id). A lost
  failover transaction would have restored the task row along with the event, by
  the same atomicity. The most probable explanation is operator or test
  interference with the live database — F-12 proves that happens, and this is
  one case in ~11,800. **Asserting an engine defect here would not be supported
  by the evidence.**
- **What the finding actually is, and it stands regardless of cause:** the
  engine has **no recovery path for a `RUNNING` workflow that has lost its task
  row**. Whatever produces that state — a bug, a restore, an operator, a torn
  failover — it is permanent and is reported as healthy. This is the same
  missing component as F-01's weaker fix: a reconciler.
- **Fix:** the reconciler described in F-01, generalised — for each owned shard,
  periodically find `RUNNING` executions with no task row and no timer row,
  exclude those held at `ESCALATE` (see F-06), and enqueue a workflow task.
  Replay is idempotent, so re-driving such a workflow is safe by construction:
  it would simply write the missing `WorkflowExecutionCompleted`. **This single
  component would have recovered all 20 stuck workflows on the live cluster.**
- **Regression test:** `engine/internal/runtime/reconciler_test.go`. **NOT WRITTEN.**
- **Blocks launch: YES**, jointly with F-01 — they share one fix.

---

## 8. Evidence appendix

### Source files read in full
`internal/core/{ids,errors,event,command,idem,ports}.go`;
`internal/eventlog/{commit.go, store.go (ExecutionStore section)}`;
`internal/workflow/{dispatcher,coroutine,context,history,decision}.go`;
`internal/runtime/{activity_worker,workflow_worker,preface,pool}.go`,
`engine.go:90-250`, `dedup.go:235-295`, `metrics.go` (reason constants);
`internal/lease/{store,manager}.go`; `internal/taskq/queue.go`;
`internal/timers/{store,scanner}.go`; `internal/saga/saga.go`;
`internal/activity/{dedup,executor}.go`; `internal/api/handlers.go:136-345`;
`internal/shard/ring.go:1-120`; `cmd/engined/main.go:200-300, 380-520`;
`cmd/loadgen/duplicates.go`; `migrations/0001_init.up.sql`;
`deploy/assert_no_duplicates.sql`.

### Evidence files read
`docs/results/README.md` (in full); `docs/results/standby-promotion-run2.txt`;
`internal/runtime/liveness_test.go` (test inventory).

### Commands run

Read-only against the live cluster (`postgres-leader`), plus one `go test`:

```
docker compose -f deploy/docker-compose.yml ps
curl -s http://localhost:808{1,2,3}/healthz
curl -s http://localhost:8081/metrics | grep -iE 'lease|fencing'

psql: SELECT status, count(*) FROM executions GROUP BY status;
psql: -- RUNNING executions with no events        -> 19
psql: -- RUNNING executions with no task, no timer -> 20
psql: -- per-row detail of all 20, with event counts and created_at/updated_at
psql: SELECT seq, event_type, payload FROM events WHERE workflow_id='real-agent-1792350128'
psql: SELECT count(*) FROM tasks;  SELECT last_value FROM tasks_task_id_seq;
psql: SELECT * FROM shard_leases ORDER BY shard_id;
psql: -- last_seq regression probe (last_seq < max event seq) -> 0

go test ./internal/shard/ -run TestAuditRingDistributionAndMovement -v
```

### Key measurements taken

| Measurement | Result |
|---|---|
| executions, by status | COMPLETED 11,783 / RUNNING 20 / FAILED 4 |
| RUNNING with zero events (**F-01 orphans**) | **19** |
| RUNNING with no task and no timer | **20** |
| `last_seq` regressions (**F-11**) | **0** of 11,807 |
| events total | 141,490 across 11,788 workflows |
| live `tasks` rows (**F-12** pollution, shards 900–911) | 48 |
| shard ownership, live | node-a 4, node-b 9, node-c 3 |
| ring: shards moved adding a 4th node (**claim 4**) | **5 of 16** — shards 2, 3, 6, 8, 12 |
| ring: shards moved losing node-b | 9 of 16 |
| lease transitions, node-a | `acquired=1` × 16 shards, `released=1` × 12 — stable, no flap |

### Files I added

- `engine/internal/shard/audit_ring_distribution_test.go` — read-only, asserts
  nothing, prints the ring's distribution and movement. Added so claim 4 could
  be checked against the code rather than taken on trust. **Safe to delete.**

`engine/internal/runtime/audit03_start_orphan_test.go` is **not mine** — it was
written by a parallel auditor (Agent 3) during this session. I left it
untouched. Worth noting that it characterises the same start-path orphan as
F-01, which is independent corroboration reached without shared context.

No production code was modified. Nothing in `docs/results/` was touched. No
commit was made.

### Cluster state left behind

**Unchanged and healthy.** No node was killed, no fault injected, no row written
or deleted. All three nodes `healthy`, 16/16 shards owned (4/9/3), both
PostgreSQL instances up with replication intact. The 20 stuck workflows and the
48 polluted task rows were **already present** when the audit began and were
deliberately left in place as evidence — see F-01 and F-12. Starting a workflow
was permitted but proved unnecessary: the existing data already contained the
proof.

### What could not be checked, and why

| Not checked | Why | Marked |
|---|---|---|
| Whether any duplicate external effect ever occurred | Structurally unobservable (F-02); no invocation counter exists | **NOT MEASURABLE** |
| Database failover under a torn commit | Would require killing the primary on the student's demo cluster | **NOT RUN** |
| Whether the failover-truncated workflow resumed | The drill records event counts only; the data is discarded by the restore step | **NOT MEASURED BY THE EVIDENCE** |
| Every regression test proposed above | Designed, not written. A designed test is not a passing test | **NOT RUN** |
| Multi-day timers and long signal waits | No evidence runs longer than minutes | **NOT RUN** |
| The full test suite | F-12 shows it writes into the live database; running it risked the demo | **NOT RUN, deliberately** |
| Replay latency claim | Agent 2's remit | **OUT OF SCOPE** |
