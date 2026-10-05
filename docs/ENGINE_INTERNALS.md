# Engine Internals — How This Thing Actually Works, Level by Level

A mechanism-level walkthrough of the durable execution engine, from "user clicks Place Order" down to the individual rows written to Postgres, the goroutine handshakes inside a worker, and the exact SQL that makes failover safe.

This is a companion to `Problem_Statement.md`. That document says *what* is being built and why. This one says *how it works*, at the level you need to write the code and to survive a viva.

---

## 0. The one idea everything else hangs off

> **Workflow code is a pure function of its event history.**

Given the same list of events, running the workflow function produces the same sequence of decisions, every time, on any machine. That single property is what buys you everything else:

- **Crash recovery** — you don't need to snapshot memory, threads, or stack frames. You re-run the function against the saved history and the stack rebuilds itself.
- **Migration** — a workflow can resume on a completely different machine, because nothing machine-local was ever part of its state.
- **Debuggability** — history *is* the trace. There is no separate log to correlate.

Everything in the rest of this document is either (a) machinery to keep that property true, or (b) machinery to keep the history durable and available.

Two terms, used precisely throughout:

| Term | Meaning | Determinism requirement |
|---|---|---|
| **Workflow** | The orchestration code. Decides *what* happens next. | Must be deterministic. Runs many times (once per replay). |
| **Activity** | A single step with a real-world side effect — charge a card, call an API, send an email. | May be non-deterministic. Runs at-least-once, ideally once. |

The workflow never does I/O. The activity never makes orchestration decisions. If you blur that line the engine breaks, and every confusing bug you will hit for the next four months traces back to blurring it.

---

## 1. Level 1 — What is actually on disk

Five tables per shard. Every mechanism in this engine is these five tables plus the code that manipulates them. Postgres, one database per shard.

### 1.1 `events` — the event log

```sql
CREATE TABLE events (
    shard_id    INT         NOT NULL,
    workflow_id TEXT        NOT NULL,
    run_id      UUID        NOT NULL,
    seq         BIGINT      NOT NULL,   -- 1, 2, 3, ... per (workflow_id, run_id)
    event_type  TEXT        NOT NULL,
    payload     JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (shard_id, workflow_id, run_id, seq)
);
```

Three things to notice, because they are load-bearing:

1. **The primary key gives you append-only for free.** Two workers both trying to append `seq = 47` — one gets a unique-violation and rolls back. You do not need a lock to protect the log; the PK *is* the concurrency control. This is your cheapest correctness guarantee, take it.
2. **`run_id` exists so a `workflow_id` can be re-run.** `order-8f3a` might be retried as a fresh execution after a terminal failure. `workflow_id` is the business key (stable, user-facing); `run_id` is one attempt at it.
3. **There is no `UPDATE` on this table, ever.** If you find yourself writing one, something has gone conceptually wrong.

### 1.2 `executions` — the mutable pointer

The log is the truth, but you can't scan the whole log to answer "is this order still running?". So one mutable row per workflow, derived from the log:

```sql
CREATE TABLE executions (
    shard_id       INT    NOT NULL,
    workflow_id    TEXT   NOT NULL,
    run_id         UUID   NOT NULL,
    workflow_type  TEXT   NOT NULL,
    status         TEXT   NOT NULL,   -- RUNNING | COMPLETED | FAILED | COMPENSATING
    last_seq       BIGINT NOT NULL,
    PRIMARY KEY (shard_id, workflow_id)
);
```

`PRIMARY KEY (shard_id, workflow_id)` — not including `run_id` — is deliberate. It means **at most one open run per business key**, which is how `StartWorkflow` becomes idempotent: a duplicate start request hits a unique violation and returns the existing execution instead of creating a second one. Free deduplication at the front door.

### 1.3 `tasks` — the durable queue

```sql
CREATE TABLE tasks (
    shard_id            INT         NOT NULL,
    task_id             BIGSERIAL   NOT NULL,
    task_type           TEXT        NOT NULL,   -- WORKFLOW | ACTIVITY
    workflow_id         TEXT        NOT NULL,
    run_id              UUID        NOT NULL,
    scheduled_event_seq BIGINT,                 -- which event created this task
    visible_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until        TIMESTAMPTZ,
    lock_token          UUID,
    attempt             INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (shard_id, task_id)
);
CREATE INDEX ON tasks (shard_id, task_type, visible_at)
    WHERE locked_until IS NULL OR locked_until < now();
```

This is a *lease queue*, not a delete-on-read queue. A worker doesn't remove the row; it takes a time-boxed lock on it. If the worker dies, the lock expires and the task becomes visible again. That's the whole crash-recovery story for in-flight tasks.

The poll query — and the `SKIP LOCKED` here is not optional, it's the difference between a queue that scales to N workers and one that serializes them all behind a single row lock:

```sql
UPDATE tasks
SET locked_until = now() + interval '30 seconds',
    lock_token   = gen_random_uuid(),
    attempt      = attempt + 1
WHERE (shard_id, task_id) IN (
    SELECT shard_id, task_id FROM tasks
    WHERE shard_id = $1
      AND task_type = $2
      AND visible_at <= now()
      AND (locked_until IS NULL OR locked_until < now())
    ORDER BY task_id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;
```

### 1.4 `timers` — durable sleep

```sql
CREATE TABLE timers (
    shard_id    INT         NOT NULL,
    fire_at     TIMESTAMPTZ NOT NULL,
    workflow_id TEXT        NOT NULL,
    run_id      UUID        NOT NULL,
    timer_seq   BIGINT      NOT NULL,   -- the TimerStarted event's seq
    PRIMARY KEY (shard_id, workflow_id, run_id, timer_seq)
);
CREATE INDEX ON timers (shard_id, fire_at);
```

### 1.5 `shard_leases` — ownership

```sql
CREATE TABLE shard_leases (
    shard_id      INT         PRIMARY KEY,
    owner_node    TEXT,
    fencing_token BIGINT      NOT NULL DEFAULT 0,
    expires_at    TIMESTAMPTZ NOT NULL
);
```

Covered in depth in Level 9. It is four columns and it replaces ZooKeeper.

---

## 2. Level 2 — The workflow task loop (the heart of the engine)

Everything else is plumbing around this loop. Read it three times.

A **workflow task** means: *"this workflow has new history; run its code forward until it blocks again."*

```
1.  Poll a WORKFLOW task from `tasks`.
2.  Load full history: SELECT * FROM events WHERE workflow_id=..., run_id=... ORDER BY seq.
3.  Construct a fresh WorkflowContext:
        seqCounter  = 0
        history     = the events
        cursor      = 0
        commands    = []
4.  Start the workflow function on a coroutine. Let it run.
5.  Every time the code calls ctx.ExecuteActivity / ctx.Sleep / ctx.WaitForSignal:
        k := seqCounter++            // program order → stable id
        if history contains an event for k:
              REPLAY MODE  — resolve from history, emit no command
        else:
              NEW MODE     — append a command, return an unresolved Future
6.  Run until the coroutine blocks on an unresolved Future (or returns).
7.  Collect `commands`.
8.  ONE Postgres transaction:
        - verify our fencing token (Level 9)
        - INSERT the events implied by the commands
        - INSERT the corresponding rows into `tasks` / `timers`
        - UPDATE executions.last_seq
        - DELETE this workflow task row
    COMMIT.
9.  Throw the in-memory workflow state away.
```

Step 9 is not a mistake. Correctness never depends on keeping the workflow in memory. Caching it between tasks (Temporal calls this *sticky execution*) is a pure latency optimization you can add in October and remove without changing behaviour.

Step 8 being a single transaction is what makes the whole system safe. Either the decision and all its consequences land together, or none of them do. There is no partial state to reconcile, ever.

### 2.1 The join key: program order

This is the mechanism most people miss.

```go
func (w *FoodDeliveryWorkflow) Execute(ctx WorkflowContext) error {
    if err := ctx.ExecuteActivity("ChargePayment", w.order).Get(); err != nil {   // k = 0
        return err
    }
    if err := ctx.ExecuteActivity("NotifyRestaurant", w.order).Get(); err != nil { // k = 1
        return ctx.ExecuteActivity("RefundPayment", w.order).Get()                // k = 2
    }
    ...
}
```

`k` is assigned by a counter that increments **once per call, in the order the code makes the calls**. It is never stored in the source. It is recomputed identically on every replay because the code takes the same branches, because the code is deterministic, because it only ever reads from history.

`k` is written into the `ActivityTaskScheduled` payload. On replay, `k` is how the engine matches "the call the code is making right now" to "the event that recorded that call last time."

If the code takes a different branch on replay, `k` shifts, the match fails, and you get a **non-determinism error**. The engine cannot repair this — it can only refuse to continue, because it has no idea which version of the code told the truth. Detect it and fail loudly:

```go
if recorded.ActivityType != requestedType {
    return NonDeterminismError{
        Seq: k, Recorded: recorded.ActivityType, Requested: requestedType,
    }
}
```

### 2.2 How the coroutine actually blocks (Go specifics)

`future.Get()` has to suspend the workflow *without* blocking the OS thread and *without* handing control to the Go scheduler in a way you can't predict. You need cooperative, deterministic scheduling. The handshake:

```go
type coroutine struct {
    unblock chan struct{}   // dispatcher → workflow: "keep going"
    yielded chan struct{}   // workflow → dispatcher: "I'm blocked"
}

// Called from workflow code, on the workflow goroutine.
func (f *future) Get() (any, error) {
    for !f.ready {
        f.co.yielded <- struct{}{}   // hand control to dispatcher
        <-f.co.unblock               // wait to be resumed
    }
    return f.value, f.err
}

// Dispatcher, on the engine goroutine.
func (d *dispatcher) runUntilBlocked() {
    d.co.unblock <- struct{}{}   // let the workflow run
    <-d.co.yielded               // it ran until it blocked or finished
}
```

Exactly one goroutine is runnable at any instant. The dispatcher and the workflow ping-pong. There is no true parallelism inside a workflow, and that is the point — parallelism would be non-determinism.

This also tells you why `go func(){}`, `time.Sleep`, `sync.Mutex`, and raw channels are banned inside workflow code: they hand scheduling to the Go runtime, which does not consult your history.

---

## 3. Level 3 — A worked trace, with real rows

`workflow_id = order-8f3a`, `run_id = 4c1e...`, hashes to shard 2.

### Task 1 — the workflow starts

Gateway writes, in one transaction:

| seq | event_type | payload |
|---|---|---|
| 1 | `WorkflowExecutionStarted` | `{"type":"FoodDelivery","input":{"order_id":"8f3a","amount":450,"restaurant":"R42"}}` |
| 2 | `WorkflowTaskScheduled` | `{}` |

…plus `tasks(task_type=WORKFLOW, scheduled_event_seq=2)` and `executions(status=RUNNING, last_seq=2)`.

A workflow worker polls that task. History is `[1, 2]`. It runs the code:

- `ctx.ExecuteActivity("ChargePayment", …)` → `k=0`. No event for `k=0` in history → **NEW MODE**. Emit command, return unresolved Future.
- Code calls `.Get()` → blocks. Dispatcher regains control. No more history to feed. Task is done.

Commit:

| seq | event_type | payload |
|---|---|---|
| 3 | `WorkflowTaskStarted` | `{"worker":"node-a"}` |
| 4 | `WorkflowTaskCompleted` | `{}` |
| 5 | `ActivityTaskScheduled` | `{"k":0,"activity":"ChargePayment","input":{...},"dedup_key":"8f3a:0"}` |

…plus `tasks(task_type=ACTIVITY, scheduled_event_seq=5)`, and the workflow task row is deleted.

**Note what just happened: the intent to charge was durably recorded *before* anything was charged.** That ordering is the entire basis of the engine's safety.

### Task 2 — the activity runs

An activity worker polls the ACTIVITY task, reads event 5 for its input, and calls the payment API. It succeeds: `txn_id = ch_991`. The worker appends:

| seq | event_type | payload |
|---|---|---|
| 6 | `ActivityTaskStarted` | `{"worker":"node-b","attempt":1}` |
| 7 | `ActivityTaskCompleted` | `{"k":0,"result":{"txn_id":"ch_991"}}` |
| 8 | `WorkflowTaskScheduled` | `{}` |

…deletes the activity task, and inserts a new WORKFLOW task. The result is now in history. It will never be recomputed.

### Task 3 — replay, then forward

Worker polls the workflow task. History is `[1..8]`. **It runs the workflow function from the very first line again:**

- `ctx.ExecuteActivity("ChargePayment", …)` → `k=0`. History has `ActivityTaskScheduled(k=0)` and `ActivityTaskCompleted(k=0)` → **REPLAY MODE**. The Future resolves *immediately* with `{"txn_id":"ch_991"}`. **No API call is made.** The code never knows the difference.
- `.Get()` returns instantly. Execution continues past the point it previously reached.
- `ctx.ExecuteActivity("NotifyRestaurant", …)` → `k=1`. Nothing in history → **NEW MODE**. Emit command. Block.

Commit events 9–11, schedule the next activity task. And so on.

That is the complete engine. Everything below is making this survive worse and worse failures.

---

## 4. Level 4 — Determinism, concretely

Banned inside workflow code, and the replacement:

| Banned | Why | Use instead |
|---|---|---|
| `time.Now()` | Different on every replay | `ctx.Now()` — returns the timestamp recorded in `WorkflowTaskStarted` |
| `rand`, `uuid.New()` | Different every replay | `ctx.SideEffect(func() any { … })` |
| `time.Sleep` | Blocks a real thread; wall-clock dependent | `ctx.Sleep(d)` — a durable timer |
| `go func(){}` | Non-deterministic interleaving | `ctx.Go()` if you implement it, on the same cooperative scheduler |
| Map iteration | Go randomizes it *deliberately* | Sort keys before iterating |
| Reading env vars, files, network | Environment differs across machines | Do it in an activity |
| Global mutable state | Shared across replays | Pass through workflow input |

### `SideEffect` — the escape hatch, and how it works

```go
orderRef := ctx.SideEffect(func() any { return uuid.New().String() }).(string)
```

- **New mode:** run the closure, get `"a91f-…"`, append `MarkerRecorded{k, value:"a91f-…"}` to history, return it.
- **Replay mode:** do *not* run the closure. Read the value out of `MarkerRecorded` and return it.

Non-determinism is neutralised by journaling its result. Hold onto this idea — **Level 12 uses exactly this trick to make the semantic idempotency check safe**, and it's the piece that makes that contribution technically defensible rather than hand-wavy.

Map iteration deserves a specific warning: Go randomizes map ordering on purpose, so a `for k, v := range m` that schedules activities will schedule them in a different order on replay and blow up with a non-determinism error — intermittently, maybe once in twenty runs. You will lose an evening to this. Sort the keys.

---

## 5. Level 5 — Retries, and the honest truth about "exactly once"

Say this to your guide before they say it to you:

> **The engine provides at-least-once *execution* of activities and exactly-once *observation* of their results.** True exactly-once execution across a process boundary is impossible; the engine's job is to make the boundary narrow and to hand the application the tool to close it.

Here is the proof, as a catalogue of every place a crash can land during one activity.

| # | Crash point | State on disk | What happens on recovery | Safe? |
|---|---|---|---|---|
| A | After `ActivityTaskScheduled` (5), before any worker polls | Task row exists, unlocked | Another worker polls it. Activity runs once. | Safe |
| B | Worker polled the task, crashed before calling the payment API | Task row locked, lock expiring | `locked_until` passes, task redelivered, activity runs once. | Safe |
| C | **Payment API succeeded, worker crashed before writing event 7** | Task locked; money moved; history says nothing | Lock expires, task redelivered, **worker calls the payment API a second time** | **Not safe — engine alone cannot fix this** |
| D | Event 7 written, crash before the workflow task is processed | Workflow task row exists | Redelivered, replay resumes, activity is not re-run | Safe |

Window C is irreducible. The side effect and the record of it live in two different systems, and there is no distributed transaction between your Postgres and Stripe. What the engine does about it:

1. It passes a stable **dedup key** — `"8f3a:0"`, derived from `workflow_id` and the deterministic `k` — to the activity. The same key on every retry, because `k` is stable across replays.
2. The activity forwards it as the downstream service's idempotency key (`Idempotency-Key: 8f3a:0` for Stripe, or a `UNIQUE` column for your own DB).
3. The downstream service does the actual deduplication.

So: the engine's contribution is *generating a stable key across crashes and replays*, which the application could not do on its own. The final dedup happens where it must — at the resource. Being explicit about this is a strength in a viva, not a weakness. Vagueness here is what gets you asked follow-up questions you can't answer.

### Retry policy

Lives on the activity, evaluated by the workflow worker when it sees `ActivityTaskFailed`:

```go
type RetryPolicy struct {
    InitialInterval    time.Duration  // 1s
    BackoffCoefficient float64        // 2.0
    MaximumInterval    time.Duration  // 100s
    MaximumAttempts    int            // 5, 0 = unlimited
    NonRetryableErrors []string       // "InvalidCardError" — never retry
}
```

A retry is just another row in `tasks` with `visible_at = now() + backoff`. Nothing special; the queue already handles delay.

---

## 6. Level 6 — Durable timers

`ctx.Sleep(5 * time.Minute)` at `k=3`:

1. **New mode:** append `TimerStarted{k:3, fire_at: ctx.Now()+5m}`, insert into `timers`, return unresolved Future. Workflow blocks. Worker moves on to other work. **Nothing is holding a thread, a connection, or memory for those five minutes.**
2. A background loop per shard, every second:
   ```sql
   SELECT * FROM timers WHERE shard_id = $1 AND fire_at <= now() LIMIT 100;
   ```
   For each: append `TimerFired{k:3}`, delete the timer row, insert a WORKFLOW task. One transaction.
3. The workflow task replays from seq 1; at `k=3` it finds `TimerStarted` **and** `TimerFired`, resolves the Future immediately, continues.

A five-minute sleep and a five-day sleep cost exactly the same. Cluster restarts during the wait are invisible — the timer is a row, not a thread. This is the cheapest impressive demo you have: start a workflow with a 2-minute sleep, `docker compose down`, wait, `docker compose up`, watch it wake up on schedule.

**Timer + activity race:** "wait up to 5 min for the restaurant" is a select over two futures:

```go
sel := ctx.NewSelector()
sel.AddFuture(restaurantReply, func(f Future) { accepted = true })
sel.AddFuture(ctx.Sleep(5*time.Minute), func(f Future) { timedOut = true })
sel.Select()   // blocks until one is ready
```

The selector must resolve **in history order**, not in whichever-fires-first order — otherwise the branch taken differs between the live run and the replay, and you get a non-determinism error. Resolve by scanning history for whichever of the two completion events has the lower `seq`.

---

## 7. Level 7 — Signals and human-in-the-loop

A signal is an external event injected into a running workflow: the restaurant taps Accept, a human approves the agent's plan.

`POST /workflows/order-8f3a/signal/RestaurantResponse {"accepted":true}` →

1. Route to shard 2, verify the fencing token.
2. Append `SignalReceived{name:"RestaurantResponse", payload:{...}}`.
3. Insert a WORKFLOW task.
4. Commit.

The signal is durable **before** any workflow code runs. If the whole cluster dies one millisecond later, the signal is not lost. On the next workflow task, `ctx.WaitForSignal("RestaurantResponse")` at `k=4` scans history for a matching `SignalReceived` and resolves.

Signals arriving before the workflow reaches the wait must still be delivered — buffer them in history and match on name, not on arrival order relative to `k`. This is the classic bug in a hand-rolled implementation.

---

## 8. Level 8 — Sharding

Two mappings, and keeping them separate matters:

```
workflow_id  --(hash, fixed N)-->  shard_id  --(consistent hash ring)-->  node
```

- `shard_id = fnv32(workflow_id) % N`, with **N fixed at deploy time** (say 16). A workflow never changes shard. This is what makes its log a stable, single-writer append stream.
- Shards are then assigned to nodes via a consistent hash ring with ~150 virtual nodes each. Adding or removing a node moves a minority of shards; every other shard is untouched.

If you instead hashed `workflow_id` straight to *nodes*, then adding a node would move individual workflows between logs mid-flight. Don't. The fixed indirection through `shard_id` is why rebalancing is a cheap ownership handoff rather than a data migration.

```go
type Ring struct {
    points  []uint32          // sorted hash positions
    nodeFor map[uint32]string
}

func (r *Ring) Lookup(key string) string {
    h := fnv32(key)
    i := sort.Search(len(r.points), func(i int) bool { return r.points[i] >= h })
    if i == len(r.points) { i = 0 }        // wrap around the ring
    return r.nodeFor[r.points[i]]
}
```

The gateway is stateless: hash → node → forward. If it forwards to a node that no longer owns the shard, that node replies `NotOwner{current_owner}` and the gateway refreshes and retries. Routing is allowed to be briefly wrong; correctness is enforced at the write, not at the route. Say that out loud in your defence — it's the difference between a design that needs global consensus on routing and one that doesn't.

---

## 9. Level 9 — Leases and fencing (the part that impresses)

The claim in your contributions list is "persistence-layer fencing, no external lock service." Here is the actual mechanism, and the argument for why it is safe.

### Acquire

```sql
UPDATE shard_leases
SET owner_node    = $me,
    fencing_token = fencing_token + 1,
    expires_at    = now() + interval '10 seconds'
WHERE shard_id = $shard
  AND (owner_node = $me OR expires_at < now())
RETURNING fencing_token;
```

Zero rows returned → someone else holds a live lease. One row → you own the shard, and you now hold token `T`. The token increments on **every** ownership change and never goes backwards.

### Renew (every 3s, on a 10s expiry)

```sql
UPDATE shard_leases
SET expires_at = now() + interval '10 seconds'
WHERE shard_id = $shard AND owner_node = $me AND fencing_token = $T;
```

Token unchanged. If this returns zero rows, you have been fenced out — stop all work on this shard immediately.

### The part that actually provides safety

The lease is **not** what makes this safe. This is:

```go
tx, _ := db.Begin()
var tok int64
tx.QueryRow(`SELECT fencing_token FROM shard_leases
             WHERE shard_id=$1 AND owner_node=$2 AND expires_at > now()
             FOR UPDATE`, shard, me).Scan(&tok)
if tok != myToken {
    tx.Rollback()
    return ErrLostOwnership
}
// ... all event appends, task inserts, timer inserts ...
tx.Commit()
```

**Every mutating transaction re-checks the token, inside the same transaction as the writes, with `FOR UPDATE`.**

Why this specific shape? Consider the failure that breaks naive lease systems:

1. Node A owns shard 2, token 7. It polls a task and starts working.
2. Node A's process freezes — GC pause, disk stall, VM suspend. It has no idea time is passing.
3. Ten seconds pass. The lease expires. Node B acquires it and gets **token 8**.
4. Node B replays the workflow and starts executing.
5. Node A wakes up mid-transaction, still believing it holds the lease, and tries to commit.

Without fencing, both nodes write and you have split-brain. With the check above, A's transaction reads token 8, sees `8 != 7`, and rolls back. A cannot commit *anything* — not one event, not one task row — after being fenced. `FOR UPDATE` is what makes it airtight: it serializes the ownership check against the write, so there is no window between "check" and "write" for ownership to change underneath you.

The property is: **a node that has lost ownership cannot mutate shard state, regardless of what it believes.** This is Kleppmann's fencing token argument, implemented in a table instead of ZooKeeper, and it is *exactly* what Temporal does with its RangeID. That last sentence is your answer when the guide asks why you didn't use ZooKeeper.

Two things to be honest about:

- **Availability gap.** Between the old owner dying and a new one acquiring, the shard is unavailable for up to `lease_duration`. 10s expiry ≈ up to 10s of unavailability for 1/N of workflows. That's a tunable, and it's a real trade-off — shorter expiry means faster failover and more risk of spurious fencing under load. Measure it and put the number in your results.
- **This is not consensus.** It relies on Postgres as a single, linearizable source of truth for the ownership record. That's a legitimate design (it's the DBOS argument), but say it plainly: you've moved the coordination problem into the database rather than eliminated it.

---

## 10. Level 10 — Replication and failover

Each shard's Postgres has a follower via streaming replication.

**Async replication** (`synchronous_commit = off`) is fast but has a loss window: a leader that dies with unreplicated commits loses them. **Sync replication** (`synchronous_commit = remote_apply`) has no loss window but adds a network round-trip to every commit and stalls entirely if the follower is down.

Recommendation: run sync, and make the cost a *result* rather than a caveat. "Throughput with async vs sync replication, and the measured loss window under kill-9 with async" is a genuine experimental finding, and it costs you one config flag to produce. That table will do more for your evaluation section than another feature will.

Failover sequence when node A (shard 2 owner) is killed:

```
t+0.0s  docker kill node-a
t+0.0s  In-flight tasks stay locked; locks are already ticking down
t+3.0s  Node A misses its lease renewal
t+10.0s Lease expires
t+10.1s Node B's acquire loop wins the UPDATE → token 8
t+10.1s Postgres follower promoted (or B connects to the same DB, if you
        keep replication at the DB layer only)
t+10.2s B scans for tasks whose locks have expired
t+10.3s B polls a workflow task, replays history from seq 1, resumes
t+40.0s Any activity tasks A held expire and are redelivered
```

Nothing is lost, because nothing was ever only in A's memory. Some work is *repeated* — activities in window C — which is why the dedup keys from Level 5 exist. Recovery time is dominated by the lease expiry, and it is the headline number in your Checkpoint 2 demo. Quote it as a distribution (p50/p99 over 20 kills), not a single lucky run.

---

## 11. Level 11 — Sagas and compensation

Compensation is not a special engine subsystem. It's a workflow pattern that the engine makes durable, and understanding that is the point.

```go
func (w *FoodDeliveryWorkflow) Execute(ctx WorkflowContext) error {
    saga := NewSaga(ctx)
    defer saga.Cleanup()

    if err := ctx.ExecuteActivity("ChargePayment", w.order).Get(); err != nil {
        return err                                   // nothing done yet, nothing to undo
    }
    saga.AddCompensation("RefundPayment", w.order)   // registered AFTER success

    if err := ctx.ExecuteActivity("ReserveInventory", w.order).Get(); err != nil {
        return saga.Compensate()                     // → RefundPayment
    }
    saga.AddCompensation("ReleaseInventory", w.order)

    if err := ctx.ExecuteActivity("NotifyRestaurant", w.order).Get(); err != nil {
        return saga.Compensate()                     // → ReleaseInventory, then RefundPayment
    }
    return nil
}
```

Mechanically:

- `AddCompensation` pushes onto an in-memory stack. **It is not an event.** It doesn't need to be — the stack is rebuilt identically on every replay, because the code is deterministic and the successes it depends on are in history. If you find yourself wanting to journal the compensation stack, re-read Level 0; you're about to duplicate state that replay already gives you.
- `Compensate()` pops in **LIFO order** and runs each compensation as a normal activity — journaled, retried, idempotent, crash-safe like anything else.
- Registration happens *after* the forward step succeeds. Register before, and a failed charge triggers a refund of money that was never taken.

The genuinely hard cases, which you should raise before the guide does:

- **A compensation itself fails.** It retries with backoff like any activity. If it exhausts retries, the workflow enters `COMPENSATING_FAILED` and requires human intervention. There is no automatic resolution — Garcia-Molina's original 1987 paper says as much ("you cannot un-fire a missile"). Surfacing this state in the admin panel is a better demo than pretending it can't happen.
- **Non-compensatable steps.** A sent email cannot be unsent. The saga's guarantee is *semantic* rollback (a cancellation email), not *physical* rollback.
- **No isolation.** Between step 2 and its compensation, other workflows can observe the intermediate state. Sagas are ACD, not ACID. This is exactly the gap the Daraghmi 2022 paper in your reading list attacks with a quota cache — cite it, and state plainly that you're accepting the limitation rather than solving it.

---

## 12. Level 12 — Semantic idempotency (the contribution)

### 12.1 First, a framing correction — read this carefully

Your current one-paragraph pitch says: *"a crash causes the agent to retry with reworded but equivalent output."* That is loose in a way a sharp examiner will catch, and it's worth fixing now rather than in the viva.

When the **engine** retries a scheduled activity, the arguments are already frozen in the `ActivityTaskScheduled` event. Byte-identical. Exact matching handles it. So an engine-level retry is *never* the source of a reworded duplicate.

The real source is **agent re-planning**. Here is the crash story that actually holds up, and it's the one to demo:

```
Plan(v1)          → journaled → decides: book_flight("BLR→DEL Fri morning")
ToolCall A        → journaled → FLIGHT ACTUALLY BOOKED, result in history
Plan(v2)          → LLM call starts...
                  ✗ CRASH before ActivityTaskCompleted for Plan(v2) is written
--- recovery ---
replay seq 1..N   → Plan(v1) and ToolCall A resolve from history, not re-run
Plan(v2) re-runs  → temperature > 0 → returns a DIFFERENT plan, which
                    re-issues: book_flight("Bangalore to Delhi, Friday AM")
                  → exact-match hash ≠ any prior key → EXECUTES
                  → DOUBLE BOOKING
```

That is a genuine, crash-induced, semantically-duplicate side effect that exact-match idempotency provably cannot catch, and it's reproducible on demand for your demo. The non-crash variant — a ReAct loop re-issuing an equivalent call after a timeout — is the same phenomenon and is documented as a measured failure mode in *Why Do Multi-Agent LLM Systems Fail?* (step repetition). Cite that paper for "this is real, not hypothetical."

Pitch it as: **"agent re-planning, whether triggered by a crash or by a tool failure, produces semantically-duplicate tool calls. The workflow's event history is the natural place to detect them, and no durable execution engine looks."**

### 12.2 Where the check must live — and why this is the technically interesting part

Naive placement: inside the activity worker, right before it calls the tool.

**That is wrong, and understanding why is the strongest thing you can demonstrate about this design.**

The similarity check is non-deterministic — it depends on an embedding model, a float threshold, and a candidate set. If the verdict were computed inside the activity worker, then on replay it would either be recomputed (possibly differently, diverging history) or skipped entirely (so the workflow's decision isn't reproducible). Either way, Level 0's invariant breaks.

**Correct placement: the verdict is computed once, on the workflow path, and journaled as an event.** It is `SideEffect` from Level 4, applied to a dedup decision.

```
ctx.ExecuteActivity("book_flight", args)  with mode = Semantic, at k = 7

NEW MODE:
  1. intent    := canonicalize("book_flight", args)
  2. candidates:= prior Semantic activities of the SAME type in THIS run's
                  history that COMPLETED successfully
  3. vec       := embed(intent)                      -- network call, non-deterministic
  4. best, sim := argmax cosine(vec, candidate.vec)
  5. verdict   := DUPLICATE if sim >= τ_high
                  DISTINCT  if sim <  τ_low
                  ESCALATE  otherwise
  6. APPEND:  SemanticDedupEvaluated{
                 k: 7, verdict: "DUPLICATE",
                 matched_k: 3, similarity: 0.94,
                 threshold: 0.90, model: "all-MiniLM-L6-v2"
              }
  7. if DUPLICATE → resolve the Future with the recorded result of k=3.
                    NO activity is scheduled. NO side effect occurs.
     if DISTINCT  → emit ScheduleActivity as normal.
     if ESCALATE  → emit a human-approval signal wait, or execute (configurable).

REPLAY MODE:
  1. Read SemanticDedupEvaluated{k:7} from history.
  2. NO embedding call. NO similarity computation. NO network.
  3. Apply the recorded verdict. Identical outcome, every replay, forever.
```

The consequences are worth stating explicitly in the write-up:

- Replay is **fast and offline** — no embedding service needed to recover a workflow. If your embedding provider is down, in-flight workflows still recover. A design that called the model on every replay would have coupled recovery availability to a third-party API.
- The verdict is **auditable**. `similarity: 0.94, threshold: 0.90, matched_k: 3` is in the permanent log. When someone asks "why didn't this flight get booked?", the answer is a row, not a guess. For a mechanism that skips real-world actions, that audit trail is not a nice-to-have — it's what makes the feature deployable at all.
- It composes with the whole existing determinism story instead of fighting it.

### 12.3 Storage

```sql
CREATE TABLE activity_intents (
    shard_id      INT     NOT NULL,
    workflow_id   TEXT    NOT NULL,
    run_id        UUID    NOT NULL,
    scheduled_seq BIGINT  NOT NULL,
    activity_type TEXT    NOT NULL,
    intent_text   TEXT    NOT NULL,
    embedding     VECTOR(384) NOT NULL,   -- pgvector, all-MiniLM-L6-v2
    PRIMARY KEY (shard_id, workflow_id, run_id, scheduled_seq)
);
```

Candidate sets are per-workflow-run and small — tens of rows. **Brute-force cosine over that is microseconds; you do not need an ANN index.** Say so explicitly; it pre-empts "how does this scale" by pointing out the search is bounded by workflow history length, not corpus size. That's a real architectural advantage over semantic caching systems, which search a global corpus.

`intent_text` construction matters more than the model choice. Canonicalize: `activity_type + "(" + sorted, normalized JSON args + ")"`. For `search_flights({"q":"BLR to DEL next Friday"})` the argument *is* natural language and embeds meaningfully. For `charge_payment({"amount":450})` it isn't — which is precisely why `IdempotencyMode` is **opt-in per activity** and defaults to `Exact`.

### 12.4 Failure modes — lead with these

| | Meaning | Cost | Mitigation |
|---|---|---|---|
| **False positive** | Two genuinely different calls judged duplicate → a real action is silently skipped | **Severe and asymmetric.** A flight the user wanted is never booked, and nothing errors. | Opt-in per activity; high τ; ESCALATE band; every verdict journaled and visible in the admin panel |
| **False negative** | A true duplicate not caught → executes twice | Equal to the exact-match baseline. No regression. | — |

The asymmetry is the honest core of this contribution and you should present it as a designed-for constraint rather than let it be discovered: **the mechanism is strictly better than baseline on false negatives and strictly worse on false positives, so it is opt-in, thresholded conservatively, and fully audited.** That is a real engineering position. Claiming it's free is what gets picked apart.

The two-threshold (τ_high / τ_low / ESCALATE) design comes straight from *Closing the Calibration Gap in Semantic Caching* — cite it, and note that a single threshold forces a binary decision at exactly the point where the model is least confident.

### 12.5 The experiment

- **Setup:** AI-agent workflow, temperature 0.8, crash injected after a tool call is journaled but before the *next* plan completes (the Level 12.1 scenario). 100–200 trials.
- **Arms:** (1) exact-match only, (2) semantic, τ ∈ {0.85, 0.90, 0.95}, (3) semantic with escalation band.
- **Ground truth:** hand-label each retry pair as truly-duplicate or truly-distinct. With 200 trials this is a few hours of work and it's what makes the numbers meaningful. Don't skip it and don't let a model self-label.
- **Metrics:** duplicate-side-effect rate, false-positive rate, precision/recall of the dedup verdict, added latency per check (p50/p99), embedding cost per workflow.
- **The headline result:** a precision/recall curve over τ, with the baseline as a single point at (recall=some low number, precision=1.0). That single chart is your contribution's evidence.

---

## 13. Level 13 — What actually runs on a machine

One Go binary, one process per node, these goroutines:

```
node-a (process)
├── HTTP server                          — gateway API, /health, /metrics
├── lease manager        × 1             — renew owned shards every 3s;
│                                          try to acquire unowned ones every 5s
├── per owned shard:
│   ├── timer scanner    × 1             — poll `timers` every 1s
│   ├── workflow workers × 4             — poll WORKFLOW tasks
│   └── activity workers × 8             — poll ACTIVITY tasks
└── metrics collector    × 1
```

Each worker goroutine is a plain loop: poll, do work, commit, repeat. Backoff when the poll returns nothing (start at 10ms, cap at 1s) so an idle cluster doesn't hammer Postgres.

Every worker checks the fencing token inside its commit. If a shard is lost mid-flight, its workers see `ErrLostOwnership`, abandon the transaction, and exit. No coordination needed to shut them down — the database tells them.

`docker compose` for the demo: 3 engine nodes, 2 Postgres per shard (leader + follower), Prometheus, Grafana, the food-delivery frontend. Comfortably under 3 GB.

---

## 14. Build order (and what "done" means at each step)

Your timeline is right; this is the same list with a testable exit condition attached, because "done" is the part that slips.

| # | Build | Done when |
|---|---|---|
| 1 | `events` table + append + read | You can append and read back an ordered history |
| 2 | `WorkflowContext`, Future, dispatcher coroutine | A workflow with 3 activities runs start to finish, single process, no DB |
| 3 | Replay | Kill the process mid-workflow; restart; it resumes without re-running completed activities. **This is the moment the project becomes real.** |
| 4 | `tasks` table + poll loop with `SKIP LOCKED` | Workers are separate processes from the engine |
| 5 | Retries + dedup keys | An activity that fails 3 times then succeeds; the log shows exactly one success |
| 6 | Timers | 2-minute sleep survives a full `docker compose down`/`up` |
| 7 | Sharding | 16 shards across 3 nodes; gateway routes correctly; `NotOwner` redirect works |
| 8 | Leases + fencing | `docker kill` the owner; another node takes over; **no duplicate event is ever written** |
| 9 | Replication | Follower promotion; measure recovery time over 20 kills |
| 10 | Saga | Restaurant rejects → refund fires → history shows LIFO order |
| 11 | Website | Order form, live status via polling, admin panel with a "fail this step" button |
| 12 | Agent workflow + semantic idempotency | Both workflows run on the **same unmodified engine binary** |
| 13 | Experiment + dashboards | The precision/recall curve exists |

Step 3 is the milestone. Everything before it is setup; everything after it is making it survive worse failures. If step 3 lands by early August you are on schedule regardless of how the rest looks.

---

## 15. Questions your guide will ask, and the answers

**"How is this different from a message queue with a database?"**
A queue gives you at-least-once delivery of individual messages. It does not give you *the state of the multi-step process*. With a queue, the orchestration state lives in your application code and database rows, and you write the recovery logic by hand for every workflow. Here, orchestration state is the event log, and recovery is one generic mechanism — replay — that works for every workflow ever written on the engine.

**"Why not just use Temporal?"**
The project is the implementation. Using Temporal would be using the artifact, not building it. And the semantic idempotency contribution requires modifying the engine's decision path — it cannot be built as a Temporal user.

**"Is this really exactly-once?"**
No, and no system that crosses a process boundary is. At-least-once execution, exactly-once observation of results. See Level 5, window C. The engine's job is to generate a stable dedup key across crashes and replays so the downstream resource can close the gap — which the application could not do on its own.

**"Why no ZooKeeper?"**
Because Postgres already provides the linearizable compare-and-swap that shard ownership needs. A fencing token in a table, re-checked inside every write transaction, gives the same safety property as a lock service without a second cluster to operate. Temporal does exactly this with RangeID. The honest cost: the database becomes the coordination bottleneck and the availability floor.

**"What if the embedding model is wrong?"**
False positives skip real actions, which is the severe direction, so the mode is opt-in per activity, thresholded conservatively, has an escalation band that defers to a human, and journals every verdict with its similarity score for audit. The experiment measures the false-positive rate directly rather than assuming it's small.

**"Where's the AI?"**
In the idempotency decision path — the engine embeds what an agent is trying to do and compares it against what this workflow already did, to catch semantically-duplicate side effects that exact matching provably cannot. Not as a feature bolted on top, but inside the engine's core execution decision.

**"What's the novelty over semantic caching (GPTCache, MeanCache)?"**
They decide whether to skip *recomputing an answer* — a cache miss costs latency. This decides whether to skip *re-executing a side effect* — a wrong decision costs a real-world action. Different consequence structure, therefore different threshold calibration, different failure analysis, and a hard requirement that the verdict be deterministic under replay and auditable after the fact. None of the caching work has to solve that.
