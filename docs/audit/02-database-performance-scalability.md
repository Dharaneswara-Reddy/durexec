# Audit 02 — PostgreSQL, Go concurrency, performance and scalability

Agent 2 of 3. Remit: database, concurrency, performance, scalability.
Repo at commit `3052fa7` (clean tree). Audit date 2026-10-21.

Host: Linux 6.17.5, 12 logical CPUs, 15.7 GB RAM.
Background load average when measurement began: **2.01** (1-min), 1.39 (5-min).
Recorded because `docs/results/` shows load above ~4 invalidates a run.

Everything below marked *measured* was produced by a command in this audit and
the command is given. Everything marked *derived* is arithmetic over measured
inputs, with the inputs named. Everything marked **NOT RUN** was not executed.

---

## 1. Executive summary

**Verdict for this remit: CONDITIONAL GO — for a single-laptop demonstration and
a research write-up. NO-GO as a production durable-execution engine.**

The engine is correct-by-construction in the places that matter most (the
fencing check is inside the writing transaction; the task queue is a lease
queue; the race detector is clean). What it is not is horizontally scalable in
the sense the write-up implies, and two of the four headline numbers do not
survive inspection.

| # | Finding | Severity | Confidence |
|---|---|---|---|
| 1 | Every commit takes `FOR UPDATE` on the shard's single `shard_leases` row and holds it for 5–8 round trips plus an fsync. Per-shard commit throughput is therefore capped at ~1/(lock hold time), measured constant at **126–140 commits/s per shard** across 1, 2 and 4 shards. This, not the hardware, is what sets the scaling shape. | CRITICAL | High |
| 2 | The 4-shard point in `scaling-saturated.txt` has the **same generator-bound defect** the project already quarantined in `scaling-c16-GENERATOR-BOUND.csv`. At concurrency 64 the 4-shard figure is 32.10 wf/s and 64 ÷ p50 2.0081 s = 31.87 — the generator's own limit, to within 0.7%. The engine was idle-waiting. | CRITICAL | High |
| 3 | `tasks_pollable` does not include `locked_until`, and the `ORDER BY task_id` cannot be served from the index because `visible_at` is a range predicate ahead of it. Measured: the poll goes from **0.29 ms to 15.2 ms** when a shard's queue holds 12.5k rows, with 10,000 locked rows heap-fetched and discarded per poll. Cost grows with backlog while poll rate is fixed by worker count — a congestion-collapse mechanism. | HIGH | High |
| 4 | Replay has no snapshotting. Measured read amplification is **~8 event rows read per task execution, ~10.5 replays per workflow** for a 12-event workflow — total work is O(H²) in history length. Fine at H=12; 500,000 row reads per workflow at H=1000. | HIGH | High |
| 5 | `maxPoolConns` is a floor-raiser, not a cap: the code only ever *raises* `MaxConns`. Measured live: **exactly 25 connections per node × 3 = 75 of 100**. A 4th node takes the cluster to 100+ and reproduces the "too many clients" failure the comment says was fixed. | HIGH | High |
| 6 | Replication is **asynchronous** (`synchronous_standby_names` empty, confirmed `sync_state = async`). The 0–2 lost events on promotion are inherent to the configured durability, not a bug. | MEDIUM (correctly characterised) | High |
| 7 | `events` has no GC or archival. Measured **~422 bytes per event** on disk including index. At 50 wf/s and 12 events/wf that is **~22 GB/day, ~650 GB/month**. | HIGH | High |
| 8 | The depth sampler's `countSQL` omits `task_type`, so its own code comment about index coverage is wrong. Measured: **full Seq Scan, 200,000 rows, 28.65 ms**, every 5 s per node. | MEDIUM | High |

**The scaling ratio claim half-survives.** The 1→2 doubling is a real saturated
measurement. The 2→4 doubling is not — at 4 shards the engine had drained its
queue and latency had fallen to the workload's own floor. See §5.

### What is genuinely strong, and should be claimed harder

Two results in this remit are better than the write-up currently says:

- **Zero deadlocks in 45,461,945 committed transactions** (measured,
  `pg_stat_database`). The uniform lock order — lease row always first, and it is
  per-shard — makes a deadlock between two commits unreachable, and 45.5 M
  transactions is real evidence for it, not an argument. 94 rollbacks, 1 in
  484,000, are the expected seq races and fenced commits.
- **The per-shard commit rate is constant at 126–140/s across a 4× change in
  shard count** (derived from recorded throughput × measured commits per
  workflow). That is a *mechanism* for the scaling shape, and a mechanism is
  much stronger evidence than a ratio from three data points. It also predicts
  the ceiling, which the current write-up does not attempt.

One result is worse than it looks: the engine runs **~131 database transactions
per second while idle** (measured, 45.5 M transactions over 4 days on a mostly
idle cluster) purely from workers polling. See DB-14.

---

## 2. Schema and query analysis

Schema: `engine/migrations/0001_init.up.sql` (88 lines, six tables, frozen).

### 2.1 Live server configuration (measured)

`docker exec deploy-postgres-leader-1 psql -U durexec -d durexec -c "SELECT ... FROM pg_settings"`

| Setting | Value | Comment |
|---|---|---|
| `server_version` | 16.15 | pgvector/pgvector:pg16 |
| `max_connections` | **100** | default, not overridden in compose |
| `shared_buffers` | **128 MB** | default, in a 400 MB container |
| `work_mem` | 4 MB | default |
| `default_transaction_isolation` | **read committed** | no serializable anywhere |
| `synchronous_commit` | on | local fsync |
| `synchronous_standby_names` | **(empty)** | → async replication |
| `wal_level` | replica | |
| `max_wal_size` / `min_wal_size` | 1024 MB / 80 MB | default |
| `checkpoint_timeout` | 300 s | default |
| `wal_compression` | **off** | default; relevant to §7 |
| `wal_keep_size` | **0 MB** | standby depends solely on the replication slot |
| `autovacuum_vacuum_scale_factor` | 0.2 | default |
| `deadlock_timeout` | 1000 ms | default |

Nothing in `deploy/docker-compose.yml` tunes PostgreSQL. Every figure above is
a stock default. That is a legitimate choice for a demo and worth stating
plainly in the write-up: **no measured number here reflects a tuned database.**

### 2.2 The transaction that sets the ceiling

`engine/internal/eventlog/commit.go:210` `CommitDecision`. Order, from the
function's own comment and verified against the code:

```
a. SELECT fencing_token FROM shard_leases WHERE shard_id = $1 FOR UPDATE   (commit.go:23)
b. INSERT INTO events ...                                        (batch)   (commit.go:149)
c. INSERT INTO tasks ... / INSERT INTO timers ...                (batch)   (commit.go:153,162)
d. INSERT INTO activity_intents ...                              (batch)   (commit.go:167)
e. UPDATE executions SET status=..., last_seq=..., updated_at=now()        (commit.go:175)
f. DELETE FROM tasks WHERE shard_id=$1 AND task_id=$2                      (commit.go:186)
   COMMIT  (fsync, synchronous_commit=on)
```

Step (a) is the design's entire safety argument and it is correct — the lock on
the lease row is what serialises the ownership question against the writes. It
is also, unavoidably as written, **a cluster-wide mutex per shard held across
every remaining round trip in the transaction**.

Round trips held under that lock, counting pgx behaviour (`SendBatch` pipelines
to one round trip; `Exec` is one each; `Commit` is one plus the fsync):

| Step | Round trips |
|---|---|
| (a) verify token | 1 |
| (b) events | 1 (batched) |
| (c) tasks, timers | 1–2 |
| (d) intents | 0–1 |
| (e) execution update | 1 |
| (f) task delete | 1 |
| COMMIT + fsync | 1 |
| **Total under the lease lock** | **6–8** |

`Begin` is one more round trip, outside the lock.

**This is the first bottleneck and it is quantifiable from the project's own
recorded results.** Measured from the live database:

```
events.n_tup_ins        = 141,775      executions.n_tup_upd = 129,922
distinct runs in events = 11,873 workflows   avg events/workflow = 12.00 (p50 12, max 18)
```

`CommitDecision` always runs exactly one `UPDATE executions`, so commits ≈
129,922 for 11,873 workflows = **10.9 commits per workflow** (measured).

Applying that to the recorded throughput in `docs/results/scaling-saturated.txt`
(concurrency 128):

| Shards | wf/s (recorded) | × 10.9 commits | ÷ shards = commits/s **per shard** | implied lease-lock hold |
|---|---|---|---|---|
| 1 | 12.80 | 139.5 | **139.5** | 7.2 ms |
| 2 | 23.15 | 252.3 | **126.2** | 7.9 ms |
| 4 | 46.30 | 504.7 | **126.2** | 7.9 ms |

The per-shard commit rate is **constant to within 10% across a 4× change in
shard count**. That is the signature of a per-shard serialization point, and it
is the real explanation of the near-linear scaling: the system is not scaling
with nodes or CPUs, it is scaling with **the number of independent row locks**.
7.2–7.9 ms for 6–8 localhost round trips plus one fsync is entirely plausible.

Consequence: **the 16-shard ceiling on any hardware with this code is about
16 × 12.8 ≈ 205 workflows/s**, and adding nodes beyond the point where every
shard has an owner (i.e. beyond 16 nodes, and in practice beyond 3 by §4.1)
adds nothing at all.

The fix is cheap and does not weaken the safety argument: steps (b)–(f) are
independent of each other and could be issued as **one** `SendBatch`, cutting
the lock hold from 6–8 round trips to 2. That is a 3–4× per-shard throughput
improvement available without changing the schema or the fencing design.

### 2.3 `shard_leases` is also the hottest row in the database (measured)

```
relname       n_live_tup  n_tup_upd  autovacuum_count  total_size
shard_leases          16    722,412             2,208       80 kB
events            141,490          0                 9       57 MB
```

722,412 updates on 16 rows — **45,150 updates per row** — and 2,208 autovacuum
cycles on an 80 kB table, against 9 on the 57 MB `events` table. Those updates
are lease renewals: `LEASE_RENEW=3s` × 16 shards × continuous operation.

Two consequences:

1. The renewal `UPDATE` (`lease/store.go:126` `renewSQL`) contends for the same
   row that every commit holds `FOR UPDATE`. A slow commit transaction delays
   that shard's renewal. With `LEASE_TTL=10s` and `LEASE_RENEW=3s` there are
   two spare renewal attempts, so a single slow commit is absorbed — but a
   sustained period where commits hold the row for hundreds of milliseconds
   would expire the lease and trigger a spurious failover **while the node is
   perfectly healthy**. This is the plausible mechanism behind the recorded
   `0.00 wf/s at 1 shard` under background load 5.4.
2. It is the reason `shard_leases` needs constant vacuuming. At 80 kB it is
   free; the row-lock contention is the cost, not the bloat.

Observed in the live cluster during this audit: shard 4 was at
`fencing_token = 80` while most shards were at 5 — i.e. that shard has changed
hands ~80 times. Consistent with renewal/takeover churn, not with 5 planned
restarts.

### 2.4 The poll query — the CTE fix holds, the index does not support it

`engine/internal/taskq/queue.go:118` `pollSQL`. The CTE rewrite described in the
comment is present and is the correct shape: the `FOR UPDATE SKIP LOCKED` and
the `LIMIT 1` happen once, inside the CTE, so the EvalPlanQual re-run that
caused "1 of 4 pollers got all the work, 12 rounds of 12" cannot recur. **The
fix holds.** `engine/internal/taskq/concurrency_test.go` is the regression test
and it is DSN-gated (see §4.3).

The index does **not** support it. `tasks_pollable` is
`(shard_id, task_type, visible_at, task_id)` (migration line 54).

- `locked_until` is absent, so every row whose lock is still live matches the
  index condition and must be heap-fetched and discarded by the Filter.
- `ORDER BY task_id` cannot be served in index order, because `visible_at` sits
  between the equality columns and `task_id` and is used as a range.

Measured on the live server, 48-row production table:

```
Execution Time: 0.290 ms   — Seq Scan + Sort (table too small for the index)
```

Measured on a session-local `TEMP` table with the identical definition and
index, 200,000 tasks across 16 shards, 80% currently locked:

```
Limit  (actual time=15.199..15.201 rows=1)
  Buffers: local read=3288
  ->  Sort  Sort Key: task_id   Sort Method: quicksort  Memory: 214kB
      ->  Bitmap Heap Scan on t_tasks  (actual time=1.986..14.774 rows=2500)
            Filter: ((locked_until IS NULL) OR (locked_until < now()))
            Rows Removed by Filter: 10000
            Heap Blocks: exact=3125
Execution Time: 15.231 ms
```

**0.29 ms → 15.2 ms, a 52× degradation, for one poll.** 10,000 locked rows read
and thrown away; 2,500 rows sorted to find the lowest `task_id`.

The temp table was created inside a single `psql` session and vanished with it;
no production table was touched.

Why this matters more than the absolute number: the poll *rate* is set by the
worker count, not by the queue depth. With work available, `NextIdle` resets to
`IdleMin = 10 ms` (`taskq/poller.go:21`), so each of the 12 workers per shard
polls as fast as polls complete. 12 workers × 1/15.2 ms = **790 poll
statements/s/shard**, each costing 15.2 ms of server time — 12 CPU-seconds of
database work per second, per shard. The database cannot supply that, polls
queue, the backlog deepens, polls get slower. **Deeper queue → slower poll →
less drain → deeper queue.** That is congestion collapse, and it is the most
likely explanation for `0.00 wf/s` completing in a 20 s window at one shard.

Fix: `CREATE INDEX tasks_pollable2 ON tasks (shard_id, task_type, task_id)
WHERE locked_until IS NULL` plus a second partial index for the expired-lock
case, or simply add `locked_until` to the existing index and accept the Sort.
The cleanest is to drop `ORDER BY task_id` — nothing in the design requires FIFO
— which removes the Sort entirely.

### 2.5 Replay read amplification (measured)

`engine/internal/eventlog/store.go:49` `selectEventsSQL` reads a run's whole
history, ordered by seq, with no bound and no snapshot. `store.go:71` scans each
row and `append`s a copy of the payload — one allocation per event per replay.

Per task execution the worker does three reads before it can decide:
`Executions.Get` (1), `Events.Load` (1), and the poll itself (1).

Measured from `pg_stat_user_indexes` on the live database:

```
events_pkey       idx_scan = 124,679   idx_tup_read = 1,007,810
executions_pkey   idx_scan = 839,732
tasks_pkey        idx_scan = 530,150   idx_tup_read = 4,681,046
```

- `events_pkey`: 1,007,810 ÷ 124,679 = **8.08 event rows read per replay**
- 124,679 replays ÷ 11,873 workflows = **10.5 replays per workflow**
- total event rows read per 12-event workflow ≈ 85, i.e. **~7× read
  amplification** over the 12 rows written.

That is the O(H²) signature: a workflow of H events performs ~H replays, the
k-th reading k events, total H²/2. For H = 12 the prediction is 72; measured 85
(the excess is API status reads). **Confirmed quadratic in history length.**

| History length H | Event rows read per workflow (derived, H²/2) |
|---|---|
| 12 (measured workload) | 72 |
| 100 | 5,000 |
| 500 | 125,000 |
| 1,000 | 500,000 |

This is the single biggest risk to workload profile B (§6). The engine has no
snapshot, no continue-as-new, and no history cap.

### 2.6 Write amplification per workflow step (derived from the SQL)

One workflow step (= one `CommitDecision`) writes, at minimum:

| Table | Rows | Note |
|---|---|---|
| `events` | 1–2 | measured 12.00 events over 10.9 commits = 1.1/commit |
| `tasks` | +1 insert, −1 delete | the woken successor, and the completed row |
| `executions` | 1 update | always |
| `timers` | 0–1 | only on a durable sleep |
| `activity_intents` | 0–1 | semantic-mode activities only |

Measured whole-table totals confirm it: `tasks` has 118,758 inserts, 118,908
updates and 118,687 deletes for 48 live rows — insert ≈ update ≈ delete, i.e.
**each task is polled almost exactly once** (118,908 ÷ 118,758 = 1.001). Retry
rate in the recorded workload is effectively zero, which means every recorded
number is a best case for contention.

### 2.7 Index bloat (measured)

```
tasks         n_live_tup = 48    heap = 16 kB    total = 2456 kB
tasks_pollable                    2120 kB
tasks_pkey                         232 kB
```

**2.4 MB of index on 48 live rows.** The queue churns 118k rows through a
`BIGSERIAL` key; B-tree pages left of the current insert point are emptied by
the deletes but are not reclaimed, because `BIGSERIAL` never revisits them.
Autovacuum has run 27 times on `tasks` and cannot fix this — only `REINDEX` can.
Harmless at this size; at a sustained 50 tasks/s the `tasks_pollable` index
grows without bound and nothing in the repo reindexes it. There is no
`REINDEX`, no `pg_repack`, and no scheduled maintenance anywhere in
`deploy/` or the `Makefile`.

### 2.8 Lock ordering and deadlock exposure

Lock acquisition order in `CommitDecision` is fixed and uniform: `shard_leases`
row first, then `events`/`tasks`/`timers`/`activity_intents`/`executions`. Because
every writer takes the same single row first and that row is per-shard, two
commits on the same shard are fully serialised and two commits on different
shards share no lock. **A deadlock between two `CommitDecision` calls is not
reachable.** This is a genuine strength of the design and worth saying so.

Residual exposure:

- `Queue.Enqueue` (`queue.go:266`) and the timer scanner write `tasks` **without**
  taking the lease row. They are inserts of fresh `BIGSERIAL` rows, so they
  cannot deadlock against a commit either.
- `isolation = read committed` throughout, and no `SELECT ... FOR UPDATE` other
  than the lease row and the poll CTE. **No serialization failures are
  possible**, because nothing runs at `REPEATABLE READ` or above.
- The correctness that `SERIALIZABLE` would buy is instead bought by the
  `events` primary key, which is the right trade for this design.

**Measured, and it confirms the analysis:**

```
pg_stat_database (datname=durexec, since 2026-10-17):
  xact_commit = 45,461,945   xact_rollback = 94
  deadlocks   = 0            conflicts = 0   temp_files = 19
```

**Zero deadlocks in 45.5 million committed transactions.** That is strong
empirical support for the uniform-lock-order argument above, and it is a result
worth putting in the paper — it is the kind of number that is hard to argue
with. The 94 rollbacks (1 in 484,000) are the seq races and fenced commits the
design expects.

One incidental finding from the same counter. 45,461,945 transactions over
~4 days (345,600 s) is **131 transactions/s sustained**, on a cluster that was
mostly idle. That is the idle polling floor: with ~16 shards × 12 workers = 192
pollers backing off to `IdleMax = 1 s` (`taskq/poller.go:22`), the predicted
idle rate is ~192/s and the measured average is 131/s. **The engine performs
over 100 database transactions per second while doing no work at all**, which is
the cost of polling rather than listening. `LISTEN`/`NOTIFY` on task insert
would remove nearly all of it, and is the obvious improvement for a deployment
with many mostly-idle shards (profile B).

---

## 3. Durability and replication

### 3.1 What is actually configured (measured)

```
synchronous_commit        = on       (local WAL fsync before COMMIT returns)
synchronous_standby_names = (empty)  (no standby is required to confirm)
wal_keep_size             = 0 MB     (standby relies on the replication slot)
```

```
pg_stat_replication: application_name=walreceiver  state=streaming
                     sync_state=async
                     write_lag=129 µs  flush_lag=800 µs  replay_lag=862 µs
```

### 3.2 Verdict on the promotion drill

`docs/results/standby-promotion-run2.txt` / `run3.txt` record promotion ≈0.13 s,
service back in 9–15 s, and **0–2 committed events lost** across three runs.

**That loss is inherent to the configured durability settings and is not a
bug.** With `sync_state = async`, `COMMIT` returns as soon as the *leader* has
fsynced; the standby may be behind by up to one network round trip. Measured
flush lag was 800 µs on an idle cluster. At the measured 126–140 commits/s per
shard, 800 µs of lag is 0.1 commits in flight per shard — and 1–2 across 16
shards under load is exactly the right order of magnitude for the observed
0–2 lost events. The arithmetic and the setting agree.

Making the loss zero requires `synchronous_standby_names = 'ANY 1 (...)'` and
`synchronous_commit = remote_apply` (or `on` with a sync standby), which costs
one network round trip inside the lease-row lock — i.e. it would come directly
off the 126–140 commits/s per shard in §2.2. **State this trade in the paper
rather than describing the current setting as a defect.** The honest sentence is:
"the engine chose asynchronous replication, so promotion loses up to the
replication lag; we measured 0–2 events."

The 9–15 s return-to-service is separate and is dominated by `LEASE_TTL=10s` —
the new leader cannot be written to until each shard's lease expires and is
re-acquired. That is a configuration choice, not a database limit, and the
comparison to AMBROSIA's 1.8 s should say so.

### 3.3 Checkpoint and WAL behaviour (measured)

```
pg_stat_bgwriter since 2026-10-17: checkpoints_timed=651  checkpoints_req=4
                                   buffers_checkpoint=14,811  buffers_backend=9,295
pg_current_wal_lsn since init: 586 MB of WAL generated
```

- 651 timed vs 4 requested: **checkpoints are time-driven, not pressure-driven.**
  `max_wal_size = 1 GB` is not being hit. No checkpoint pressure at this scale.
- `buffers_backend` is 9,295 against `buffers_checkpoint` 14,811 — **39% of dirty
  page writes are done by backends themselves**, meaning 128 MB of
  `shared_buffers` is already too small for the working set and backends are
  evicting dirty pages inline. This directly taxes the lease-lock hold time in
  §2.2. Raising `shared_buffers` is the cheapest available throughput win and it
  is not done.

---

## 4. Concurrency findings

### 4.1 Connection pool arithmetic — the cap is not a cap

`engine/cmd/engined/main.go:449`:

```go
const maxPoolConns int32 = 25
...
needed := int32(cfg.ShardCount*(cfg.WorkflowWorkers+cfg.ActivityWorkers)) + 8
if needed > maxPoolConns { needed = maxPoolConns }
if needed > pcfg.MaxConns { pcfg.MaxConns = needed }     // ← only ever RAISES
```

With `SHARD_COUNT=16`, `WORKFLOW_WORKERS=4`, `ACTIVITY_WORKERS=8`
(`config.go:136–141`): `needed = 16×12 + 8 = 200`, clamped to 25.

Measured live:

```
application_name   state   count
durexec:node-c     idle       25
durexec:node-a     idle       25
durexec:node-b     idle       25
(background)                   5
walreceiver        active      1
psql               active      1
                            ---
                             82   of max_connections = 100
```

**The arithmetic is correct for three nodes and the fix works. Two defects
remain.**

1. **18 connections of headroom.** A fourth engine node needs 25 and the
   cluster hits 107 > 100. `make up` with a `node-d` fails with "sorry, too many
   clients already" — the exact failure the comment says was fixed. Nothing in
   the repo guards this: `max_connections` is not set in
   `deploy/docker-compose.yml` and no startup check compares the cluster's
   demand against the server's limit. **Node count is capped at 3 by the
   database, not by the design.**
2. **`maxPoolConns` is a floor, not a ceiling.** `pcfg.MaxConns` arrives from
   `pgxpool.ParseConfig` defaulted to `max(4, runtime.NumCPU())`. The code only
   raises it. On a host with more than 25 cores — and the container has **no
   CPU limit in the compose file**, so it sees all of the host's — each node
   would take `NumCPU` connections. On a 34-core machine: 3 × 34 = 102 > 100,
   and the regression returns on better hardware. The name and the comment both
   say "cap"; the code does not implement one. One-line fix:
   `if pcfg.MaxConns > maxPoolConns { pcfg.MaxConns = maxPoolConns }`.

`engine/cmd/loadgen/db.go:169` uses bare `pgxpool.New`, so the load generator
takes a further `max(4, NumCPU)` = 12 connections on this host while a benchmark
runs. 82 + 12 = 94 of 100 during a measurement run. **Six connections of
headroom while benchmarking**, and `psql`, `make verify` or a second agent would
exhaust it. This is a plausible contributor to the "same binary, 0.00 wf/s"
anomaly and it is worth checking for `too many clients` in the node logs of that
run before attributing it to load alone.

The reasoning in the comment — that a worker holds a connection only for the
duration of a query, so a small pool back-pressures rather than deadlocks — is
correct, with one exception in §4.4.

### 4.2 Race detector (measured)

```
$ cd engine && go test -race -count=1 -p 4 ./...
```
Full output: `/tmp/claude-1000/.../scratchpad/race.log`
Go 1.25.10. Load 2.69 before, 2.72 after. Duration ~11 s.

**30 packages, all `ok`, EXIT=0, zero `DATA RACE` reports.**

```
ok  github.com/dharaneswar339/durexec/internal/taskq     1.031s
ok  github.com/dharaneswar339/durexec/internal/runtime   1.571s
ok  github.com/dharaneswar339/durexec/internal/lease     1.023s
ok  github.com/dharaneswar339/durexec/internal/eventlog  1.012s
ok  github.com/dharaneswar339/durexec/internal/shard     1.687s
...  (3 packages have no test files: cmd/engined, cmd/fieldeval, internal/webui)
```

**Important limitation, and it is a real gap.** `DUREXEC_TEST_DSN` was not set,
so every `integration_test.go` **skipped itself** — including
`internal/taskq/concurrency_test.go`, which is the regression test for the
poller-starvation defect, and the lease, eventlog and timers integration tests.
The race detector therefore covered the in-process concurrency and **none of the
database concurrency**. Running `DUREXEC_TEST_DSN=... go test -race ./...`
against the demo cluster's own database was judged unsafe during a live demo
with two other agents operating the cluster: **NOT RUN**. It belongs in §9 as a
one-line addition against a throwaway database.

`cmd/engined` having no test files means the pool-sizing arithmetic in §4.1 is
**untested**.

### 4.3 Depth sampler — `engine/internal/taskq/depth.go`

**No leak.** `Run` (line 102) uses `time.NewTicker` with `defer t.Stop()`,
selects on `ctx.Done()`, and returns nil on cancellation. The `reported` map is
guarded by `s.mu` (lines 228–236, 245–250), and `forget` deliberately calls the
observer **outside** the lock (line 239 comment) — correct, that is the right
call. `Sample` returns early when `Observer` or `Shards` is nil, so a node with
no registry starts nothing. Nothing survives cancellation: there is no
goroutine, only the caller's.

**One real defect, in the SQL.** `countSQL` (line 182):

```sql
SELECT shard_id, task_type, count(*) FROM tasks
 WHERE shard_id = ANY($1) AND visible_at <= now()
   AND (locked_until IS NULL OR locked_until < now())
 GROUP BY shard_id, task_type
```

The comment at line 180 claims "The `tasks_pollable` index covers the
shard_id/task_type/visible_at leg of the predicate." **It does not** — the query
has no `task_type` predicate at all, so the index's second column is skipped and
PostgreSQL 16 has no skip-scan. Measured on the 200k-row temp table with all 16
shards owned:

```
HashAggregate (actual time=28.629..28.632 rows=16)
  ->  Seq Scan on t_tasks  (actual time=0.013..23.698 rows=40000)
        Rows Removed by Filter: 160000
        Buffers: local read=3125
Execution Time: 28.654 ms
```

**Full sequential scan, 200,000 rows examined, 28.65 ms, 3,125 pages (24 MB)
read.** Every 5 s, on every node. At this scale the duty cycle is 0.6% and it is
harmless. At a 1M-row backlog it is ~150 ms and ~120 MB per pass — which would
evict the whole 128 MB `shared_buffers` every five seconds, on three nodes
independently, precisely when the cluster is already in trouble. **The
observability query degrades fastest exactly when you most need it.** The fix is
to count per task type so the index applies, or to accept an estimate.

### 4.4 Claim-renewal heartbeat — `engine/internal/runtime/activity_worker.go:273`

**No leak, and the shape is right.** `keepClaimed` returns
`(actx, settleClaim)`. The goroutine `defer close(done)`s, tickers are stopped
with `defer t.Stop()`, and `settleClaim` does `cancel(); <-done; return
lost.Load()`. The `<-done` is what makes it leak-free — there is no path on
which the heartbeat outlives the activity, and the function's own comment says
so correctly. The no-token early return (line 275) avoids starting a goroutine
that could only fail. `lost` is an `atomic.Bool`, correctly. Only
`taskq.ErrLockLost` is treated as definitive (line 299); transient errors are
logged and retried, which is the right call given renewal at a third of the lock
duration leaves room for two failures.

Three issues, none of them a leak:

1. **`settleClaim()` has no timeout.** `<-done` blocks until the goroutine
   returns, which requires the in-flight `Extend` to return. `Extend` honours
   `actx` and pgx cancels on context, so in practice this resolves — but on
   shutdown the worker goroutine is unconditionally blocked on it. Severity LOW;
   worth a bounded wait.
2. **Renewal competes for the shared pool, and that is a load-amplifier.**
   `Extend` (`queue.go:189`) goes through the same 25-connection pool as every
   poll and commit. Under pool saturation the renewal queues. If it queues past
   the remaining lock lifetime, the claim lapses, `settleClaim()` returns true,
   and **the completed activity's result is thrown away** (line 125–138) — the
   work is discarded and the task is re-polled and re-run. Overload therefore
   *destroys* completed work and *adds* load. This is the second
   positive-feedback path into collapse, alongside §2.4. The §4.1 reasoning that
   "a smaller pool just back-pressures" does not hold for this one caller,
   because this caller has a deadline that the back-pressure can blow.
3. **Renewal is not fenced.** `extendSQL` matches on `lock_token` and
   `locked_until > now()` but does not consult `shard_leases`. A node fenced out
   of a shard goes on renewing its activity claims until the activity finishes
   and `CommitDecision` refuses it. Not a safety hole — the commit is refused and
   the dedup key covers the side effect — but it does mean a fenced-out node can
   hold a task row away from the new owner for up to the activity's duration,
   which lengthens failover. Severity MEDIUM for recovery time, not for
   correctness.

### 4.5 Shutdown

`main.go` drains in the correct order and the comment explains why: stop
accepting work, `pool2.StopAll()` to let in-flight tasks finish, **then**
`cancelBackground()` and release the leases — so no shard is handed on while
this node is still writing to it. It waits on `backgroundLoops` completions with
a `shutdownCtx` timeout and logs a warning if it expires. This is right.

### 4.6 Memory against the 300 MB cap

Unbounded or unchecked allocations found:

| Site | Growth | Bound |
|---|---|---|
| `eventlog/store.go:64–76` `Load` | one `[]core.Event` + one payload copy per event, per replay | **history length — unbounded** |
| `commit.go:271–287` `insertEvents` | one `pgx.Batch` entry per event | decision size |
| `commit.go:362` `VectorLiteral` | `2 + 384×12` ≈ 4.6 kB string per intent | fixed |
| `taskq/depth.go:202` | map sized `shards × 2` | 32 entries |
| `lease/store.go:207` `List` | `append` with no pre-size, 16 rows | fixed |

Only the first is a real risk, and it is the same O(H²) problem as §2.5 seen
from the heap: a 1,000-event history with 1 kB payloads allocates ~1 MB per
replay, 12 workers deep, on a node capped at 300 MB. **No history length limit
exists anywhere in the code.** A single pathological workflow can OOM a node,
and because `restart: "no"` is set on the engine services
(`docker-compose.yml:73`), that node stays down.

Container memory headroom — **NOT RUN** (`docker stats` under load was not
taken, to avoid loading the demo cluster).

---

## 5. Do the recorded claims survive?

### 5.1 The generator-bound defect is present in the published run

`engine/cmd/loadgen/throughput.go:141` describes the generator: "in-flight
workflows, each replaced as soon as it finishes, for a fixed time." That is a
**closed-loop** generator, so Little's Law holds exactly:

> throughput = concurrency ÷ mean latency

A closed-loop generator cannot measure saturation throughput. It measures
`concurrency ÷ latency` and nothing else. The only way to tell a saturated
system from an idle one is whether latency has risen **above the workload's own
service floor**.

The floor is knowable from the code. `DefaultLatency()`
(`app/delivery/activities.go:41`) for the happy path:
ReserveInventory 40 ms + ChargePayment 220 ms + NotifyRestaurant 120 ms +
DispatchDriver 180 ms = **560 ms**, plus the forced 50 ms prep timer, plus the
timer scanner's **1 s** tick granularity (`timers/scanner.go:37`), plus up to one
100 ms loadgen poll. That lands at **≈2.0 s**, and the recorded p50 bottoms out
at 2.0070–2.0152 s in four independent runs. **The floor is ~2.01 s and it is
set by sleeps and tick granularity, not by the database.**

Now apply the test to every recorded point:

| Run | Shards | Recorded wf/s | Recorded p50 | concurrency ÷ p50 | p50 above 2.01 s floor? | Verdict |
|---|---|---|---|---|---|---|
| c=64 | 1 | 13.50 | 5.0054 s | 12.79 | +3.0 s | **saturated — valid** |
| c=64 | 2 | 26.50 | 2.5093 s | 25.50 | +0.5 s | marginal |
| c=64 | 4 | **32.10** | **2.0081 s** | **31.87** | **+0.00 s** | **GENERATOR-BOUND** |
| c=128 | 1 | 12.80 | 9.8039 s | 13.06 | +7.8 s | **saturated — valid** |
| c=128 | 2 | 23.15 | 5.4579 s | 23.45 | +3.4 s | **saturated — valid** |
| c=128 | 4 | 46.30 | 2.0152 s | 63.52 | +0.01 s | **GENERATOR-BOUND** |
| c=16 (quarantined) | 1 | 5.76 | 2.8048 s | 5.70 | +0.8 s | generator-bound |
| c=16 (quarantined) | 2 | 8.68 | 2.0079 s | 7.97 | +0.00 s | generator-bound |
| c=16 (quarantined) | 4 | 8.48 | 2.0070 s | 7.97 | +0.00 s | generator-bound |

**The answer to "check whether any other recorded run has the same defect" is
YES.** At concurrency 64, 4 shards, the recorded 32.10 wf/s equals the
generator's own limit of 31.87 to within 0.7%, and p50 sits exactly on the
2.01 s floor — the identical signature by which
`scaling-c16-GENERATOR-BOUND.csv` was correctly quarantined. The same applies to
the concurrency-128 4-shard point: p50 is on the floor, so the engine had
drained its queue and was waiting for work.

`docs/results/scaling-saturated.txt` should be annotated: **its 4-shard points at
both concurrency levels are lower bounds on capacity, not measurements of it.**
The file already carries a related honesty note — "fewer owners than shards, so
this point measures one machine more than once and is NOT evidence of
horizontal scaling" — which is to the project's credit; this is a second,
independent reason the same point is not evidence.

### 5.2 Claim-by-claim

| Claim | Verdict |
|---|---|
| 12.80 / 24.10 / 47.15 wf/s at 1 / 2 / 4 shards, c=128 | 1- and 2-shard figures **stand** as saturated measurements of *this host under this workload*. The 4-shard figure is a **lower bound**, not a measurement. (The file at `3052fa7` records 12.80 / 23.15 / 46.30; the brief's 24.10 / 47.15 are a mean over two runs.) |
| 1.88× then 1.96× per doubling vs a 1.7× target | **1→2 survives** (23.15/12.80 = 1.81×, both points saturated). **2→4 does not** (46.30/23.15 = 2.00×, but the 4-shard point was not saturated — a ratio whose numerator is a lower bound is not a ratio). |
| p50 9.52 / 5.39 / 2.01 s | All measured, all real — but the 2.01 s figure is **the workload's own floor** (§5.1) and cannot go lower. The fall from 9.8 s to 2.0 s is queue drain, and it bottoms out at a number set by activity sleeps and a 1 s timer tick. Presenting 2.01 s as a performance result overstates it. |
| state rebuilt from 200 events in 0.9 ms p99 | **Stands as stated, and the code says so honestly.** `bench/replay_test.go` is pure in-process CPU replay; the test's own log line reads "the remaining time is the database read, which this harness does not measure". Any end-to-end claim must add the `selectEventsSQL` fetch of 200 rows (~62 kB at the measured 311 bytes/row). The write-up must keep the qualifier. |

### 5.3 How much is any absolute number worth?

The recorded anomaly — the same unchanged binary producing 12.80/24.50/47.40 on
a quiet host and 0.00/8.00/14.75 at background load 5.4, with **nothing at all
completing at one shard in a 20 s window** — is decisive. A 100% swing from host
load alone means **no absolute throughput figure from this setup is
transferable.** The right claim in the paper is the *shape*, and the shape now
has a mechanism behind it (§2.2: constant 126–140 commits/s per shard), which
is far stronger evidence than the ratio on its own.

The 0.00 result also deserves its own explanation rather than being filed under
"the host was busy". Two mechanisms in this audit produce exactly that outcome:
congestion collapse in the poll path (§2.4) and lease expiry from renewal
starvation (§2.3). Both are testable — see §9.

---

## 6. Workload profiles and capacity estimates

### Assumptions common to all three (stated explicitly)

| Assumption | Value | Source |
|---|---|---|
| Events per workflow | 12.00 | **measured**, `events` grouped by run |
| Commits per workflow | 10.9 | **measured**, `executions.n_tup_upd` ÷ runs |
| Per-shard commit ceiling | 126–140/s | **derived**, §2.2 |
| Bytes per event on disk | 422 (311 heap + 111 index) | **measured**, §7 |
| Lease-lock hold | 7.2–7.9 ms | **derived** from the above |
| Shards | 16 fixed | `SHARD_COUNT=16` |
| Workers per shard | 4 workflow + 8 activity | `config.go:140–141` |
| Connections | 25/node, 3 nodes max | **measured**, §4.1 |
| Latency floor | ~2.01 s | **derived**, §5.1 |
| Hardware | one 12-core laptop, 128 MB shared_buffers, no CPU limits | **measured** |

All capacity figures below are **derived**, not measured, except where a
measured value is cited. They are order-of-magnitude, and they are bounded by
the first constraint to bind, which is stated each time.

### Profile A — lightweight (short workflows, high creation rate)

| Driver | Effect |
|---|---|
| Arrival rate ↑ | Binds at **16 × 12.8 ≈ 205 wf/s** (lease-row serialization, §2.2). Beyond that, queue depth grows and §2.4 takes over. |
| Events per workflow ↑ | Linear in commits → linear loss of throughput. Halving events/workflow roughly doubles wf/s. |
| Concurrent active ↑ | Cheap — a waiting workflow is one `tasks` row. Bounded by the poll cost in §2.4, not by memory. |
| Node count ↑ | **Helps only up to 3** (§4.1 connection limit), and only to 16 (shard count). Past that, nothing. |
| Shard count ↑ | **The only lever that works.** Throughput is ~linear in shards until the database CPU saturates. |
| Task concurrency ↑ | Beyond ~12/shard this *hurts*: more pollers, more locked rows for each poll to skip (§2.4). |
| DB latency ↑ | Brutal — multiplied by 6–8 inside the lease lock. +1 ms RTT costs ~7 ms of hold time, i.e. ~45% of per-shard throughput. |
| Retry rate ↑ | Each retry is a full replay + commit. |

**Estimated sustainable: ~150–200 wf/s, ~1,800 events/s, p50 ~2 s (floor-bound),
p99 unmeasured.** First bottleneck: lease-row serialization.

### Profile B — long-running business workflows (hours to days)

This is the profile the project is named for and **it is the one the design is
least ready for.**

| Driver | Effect |
|---|---|
| History length ↑ | **O(H²) row reads (§2.5), measured.** The dominant risk. No snapshot, no continue-as-new, no cap. |
| Concurrent waiting ↑ | Cheap in `timers` (`timers_due` index is 72 kB for 11,874 lifetime timers), but every pollable `tasks` row taxes every poll (§2.4). |
| Durable timers ↑ | Scanner runs at 1 Hz per shard (**measured** `timers_due.idx_scan` = 1,395,988). Timer resolution is 1 s, so sub-second timers are impossible — a correctness-of-expectation issue for a business workflow. |
| Retries ↑ | Each retry replays the whole history. On a long history this is where the O(H²) bites. |
| Arrival rate ↑ | Binds far below profile A, because each step costs more. |
| DB outage ↑ | Workflows survive (that is the point) but every task retried after it replays a full history → **recovery is a thundering herd of full replays.** |

**Estimated sustainable: strongly dependent on H. At H=12, as profile A. At
H=100, ~70× more event-row reads per workflow → tens of wf/s. At H≥500 a
single node risks the 300 MB cap (§4.6).** First bottleneck: **replay read
amplification**, ahead of the lease row.

### Profile C — high contention (hot shards, bursty recovery)

| Driver | Effect |
|---|---|
| Hot shard | **No relief available.** A hot shard is one lease row; all its commits serialise at 126–140/s and no number of nodes or workers changes that. Workflow-ID distribution across shards is the only mitigation. |
| Workers competing ↑ | SKIP LOCKED means no lock queueing (good), but every locked row is scanned and discarded by every poll (§2.4) — contention shows up as **poll cost**, not as lock waits. |
| Retry rate ↑ | Jittered backoff (`backoff.go:24`, base 100 ms, cap 60 s, full jitter) is correctly designed and prevents lockstep retry. Good. |
| Large histories | Compounds with retries as in profile B. |
| Bursty recovery | **Worst case in the system.** After a node loss, every task of every shard it owned becomes pollable at once, each needing a full replay, while `LEASE_TTL=10s` gates when work can restart. Add the §4.4(2) path where pool saturation destroys completed activity results and re-queues them. |
| DB outage duration ↑ | Longer outage → more tasks due simultaneously on return → larger herd. No admission control, no rate limit on recovery anywhere in the code. |

**Estimated sustainable: well below profile A, and the regime is unstable rather
than merely slow** — §2.4 and §4.4(2) are both positive feedback. First
bottleneck: **poll-cost growth under backlog**, then lease-row serialization.

### Recovery time (from recorded evidence, not re-measured)

| Event | Time | Source |
|---|---|---|
| Lose one node | ~10 s to re-own shards | `LEASE_TTL=10s`, bounded below by expiry |
| Lose the database leader | 9–15 s to service | `standby-promotion-run2/run3.txt` (**recorded, not re-run**) |
| Events lost on promotion | 0–2 | recorded; inherent to async (§3.2) |
| Lose two of three nodes | ~10 s, but the survivor then owns 16 shards and needs 16×12=192 workers against 25 connections | derived |

---

## 7. Storage growth

`events` is append-only. Confirmed: `n_tup_upd = 0`, and there is **no DELETE,
TRUNCATE, archival, retention or partitioning statement for `events` anywhere in
the repository** (migration, Makefile and `deploy/` all checked).

### Measured unit cost

```
events: n_live_tup = 141,490   heap = 42 MB   total = 57 MB   events_pkey = 15 MB
```

- heap: 42 MB ÷ 141,490 = **311 bytes per event row**
- index: 15 MB ÷ 141,490 = **111 bytes per event row**
- **total 422 bytes per event**, measured, for this workload's payloads

WAL: 586 MB generated since init for 141,775 event inserts, 129,922 execution
updates, 722,412 lease updates and 118k task churn. `wal_compression = off` and
`full_page_writes = on`, so a checkpoint-adjacent write carries a full 8 kB page
image. Attributing WAL to events alone overstates it; **a conservative
~1 kB of WAL per event** is the right planning figure.

### Projection (derived from the measured 422 bytes/event, 12 events/workflow)

| Arrival rate | Events/s | Table+index per day | Per month (30 d) | Per year |
|---|---|---|---|---|
| 1 wf/s | 12 | 437 MB | 13 GB | 160 GB |
| 10 wf/s | 120 | 4.3 GB | 131 GB | 1.6 TB |
| 50 wf/s | 600 | **21.9 GB** | **656 GB** | 7.8 TB |
| 205 wf/s (the §2.2 ceiling) | 2,460 | 89.7 GB | 2.7 TB | 32 TB |

WAL, at ~1 kB/event, adds roughly 2.4× those figures in transient volume, and
the standby holds a full second copy.

**Assessment.** At 50 wf/s — comfortably within the engine's own derived ceiling
— the single shared database grows by 656 GB per month, forever, with no
mechanism in the codebase to stop it. That is launch-blocking for any
production use and must be stated in the paper. The architectural answer is not
a cron job: it is that `events` has an obvious partition key (`shard_id`, or
`created_at` for retention) and is never updated, which makes it a textbook
candidate for `PARTITION BY RANGE (created_at)` with detach-and-archive. The
single-table choice also means autovacuum must eventually freeze a table that
only grows — and at 57 MB today there is no evidence either way about how it
behaves at 600 GB. **NOT RUN.**

---

## 8. Findings register

Severity: CRITICAL / HIGH / MEDIUM / LOW. Confidence: High / Medium / Low.

---

**DB-01 — Every commit serialises on one `shard_leases` row**
Severity CRITICAL · Confidence High · **Launch-blocking: yes (for production)**
- **Location** `engine/internal/eventlog/commit.go:23` (`verifyTokenSQL`), used at `commit.go:220`; held until `commit.go:261`.
- **Preconditions** Any two concurrent commits on the same shard.
- **Failure sequence** Commit A takes `FOR UPDATE` on `shard_leases[N]` → performs 5–7 more round trips and an fsync → commits. Commit B waited for all of it. Per-shard commit rate = 1/(hold time).
- **Impact** Per-shard ceiling measured constant at 126–140 commits/s ≈ 12.8 wf/s across 1, 2 and 4 shards (§2.2). Cluster ceiling ~205 wf/s at 16 shards. Adding nodes or workers to a hot shard has **zero** effect.
- **Reproduction** Recompute the §2.2 table from `docs/results/scaling-saturated.txt` and the live `executions.n_tup_upd` ÷ distinct runs. Directly: §9 test S1 at 1 shard with worker count varied 4→16; throughput will not move.
- **Fix** Collapse steps (b)–(f) into one `pgx.Batch` so the lock is held for 2 round trips instead of 6–8. Expected 3–4× per-shard gain. Does not weaken the fencing argument: the token check stays first and in-transaction.
- **Regression test** Assert per-shard commits/s rises above 300 after the change, at 1 shard, concurrency 128.

---

**DB-02 — The published 4-shard scaling points are generator-bound**
Severity CRITICAL · Confidence High · **Launch-blocking: yes (for the paper)**
- **Location** `docs/results/scaling-saturated.txt` (c=64 4-shard, c=128 4-shard); generator at `engine/cmd/loadgen/throughput.go:141,167`.
- **Preconditions** Closed-loop generator whose concurrency ÷ service time exceeds the engine's capacity at that shard count.
- **Failure sequence** Engine drains the queue → latency falls to the ~2.01 s workload floor → throughput becomes `concurrency ÷ 2.01`, a property of the generator → recorded as engine capacity.
- **Impact** c=64/4-shard records 32.10 wf/s against a generator limit of 31.87 (0.7% apart) with p50 on the floor. The 2→4 scaling ratio is a ratio whose numerator is a lower bound. Same signature as the already-quarantined `scaling-c16-GENERATOR-BOUND.csv`.
- **Reproduction** The §5.1 table; arithmetic only, no run needed.
- **Fix** Annotate the file (do not delete it — `docs/results/` is evidence). Add a saturation gate to loadgen: refuse to report a point as saturated unless p50 exceeds a declared service floor by some margin, exactly as the c16 file was judged by hand.
- **Regression test** A loadgen unit test asserting the gate fires on the recorded c16 numbers and not on the c=128 1-shard numbers.

---

**DB-03 — `tasks_pollable` does not support the poll query**
Severity HIGH · Confidence High · **Launch-blocking: yes**
- **Location** `engine/migrations/0001_init.up.sql:54`; query at `engine/internal/taskq/queue.go:118`.
- **Preconditions** A shard whose `tasks` queue holds more than a few hundred rows, or many concurrently locked rows.
- **Failure sequence** `locked_until` is not in the index → every live-locked row is heap-fetched and Filtered. `ORDER BY task_id` is unreachable in index order because `visible_at` is a range ahead of it → Sort over all pollable rows. Poll cost rises with depth; poll rate is fixed by worker count; the product exceeds the server.
- **Impact** **Measured 0.29 ms → 15.2 ms** (52×) going from 48 rows to 12.5k rows/shard, 10,000 rows discarded per poll. 12 workers/shard × 1/15.2 ms ≈ 790 polls/s/shard × 15.2 ms = 12 CPU-s/s of database work per shard. Congestion collapse; the likely mechanism behind the recorded 0.00 wf/s.
- **Reproduction** The §2.4 temp-table `EXPLAIN (ANALYZE, BUFFERS)`, reproduced verbatim in §9 test B2.
- **Fix** Drop `ORDER BY task_id` (nothing requires FIFO) and add a partial index `ON tasks (shard_id, task_type, task_id) WHERE locked_until IS NULL`, plus one covering the expired-lock case.
- **Regression test** Assert the poll plan contains no `Sort` node and `Rows Removed by Filter` stays below 10 at a seeded depth of 10k rows/shard.

---

**DB-04 — Replay is O(H²) in history length, with no snapshot**
Severity HIGH · Confidence High · **Launch-blocking: yes for profile B**
- **Location** `engine/internal/eventlog/store.go:49` (`selectEventsSQL`, unbounded), called per task by `activity_worker.go` and `workflow_worker.go`.
- **Preconditions** A workflow whose history grows. No cap exists.
- **Failure sequence** Each of ~H task executions loads the whole history to date. Total row reads ≈ H²/2. Each read allocates a fresh `[]core.Event` plus a payload copy.
- **Impact** **Measured: 8.08 event rows per replay, 10.5 replays per workflow, ~85 rows read per 12-row workflow (≈7× read amplification).** Predicted 500,000 row reads for a 1,000-event workflow, and ~1 MB allocated per replay against a 300 MB container cap with `restart: "no"`.
- **Reproduction** `SELECT idx_scan, idx_tup_read FROM pg_stat_user_indexes WHERE indexrelname='events_pkey'`, divided by distinct runs. §9 test L1 varies H and measures directly.
- **Fix** Periodic state snapshot keyed by `(shard_id, workflow_id, run_id, seq)`; load the latest snapshot plus the tail. Or continue-as-new above a history threshold. Either makes replay O(H).
- **Regression test** Assert rows read per task execution stays bounded as H goes 10 → 100 → 1000.

---

**DB-05 — `maxPoolConns` is a floor, not a cap; 3 nodes is the hard limit**
Severity HIGH · Confidence High · **Launch-blocking: yes**
- **Location** `engine/cmd/engined/main.go:449` (`const maxPoolConns int32 = 25`), lines 487–490.
- **Preconditions** (a) a fourth node, or (b) a host where `runtime.NumCPU() > 25` — and the compose file sets no CPU limit, so containers see the whole host.
- **Failure sequence** `if needed > pcfg.MaxConns { pcfg.MaxConns = needed }` only raises. **Verified in the pgx source**, `pgxpool/pool.go:381–382`: `config.MaxConns = defaultMaxConns` (4), then `if numCPU := int32(runtime.NumCPU()); numCPU > config.MaxConns` — i.e. the default is `max(4, NumCPU)`, and the doc comment at `pool.go:175` says so. On a 34-core host each node takes 34 → 3 × 34 = 102 > `max_connections` 100 → `FATAL: sorry, too many clients already`, the exact regression the comment claims to have fixed.
- **Impact** **Measured 25 + 25 + 25 + 5 + 2 = 82 of 100**, i.e. 18 spare. loadgen takes a further 12 (`cmd/loadgen/db.go:169`, bare `pgxpool.New`) → 94 of 100 during any benchmark. Six spare while measuring. Node count is capped at 3 by the database.
- **Reproduction** `SELECT application_name, state, count(*) FROM pg_stat_activity GROUP BY 1,2;` (output in §4.1). For the NumCPU path, run a node on a >25-core host.
- **Fix** `if pcfg.MaxConns > maxPoolConns { pcfg.MaxConns = maxPoolConns }`. Set `max_connections` explicitly in `docker-compose.yml`. Add a startup check: `nodes × maxPoolConns + headroom <= max_connections`, logged loudly.
- **Regression test** A unit test over `newPool`'s arithmetic for NumCPU = 4, 12, 64 asserting the result never exceeds 25. `cmd/engined` currently has **no test files at all**.

---

**DB-06 — `events` has no retention, archival or partitioning**
Severity HIGH · Confidence High · **Launch-blocking: yes**
- **Location** `engine/migrations/0001_init.up.sql:13–22`; absence confirmed across `migrations/`, `Makefile`, `deploy/`.
- **Impact** **Measured 422 bytes/event.** At 50 wf/s: **21.9 GB/day, 656 GB/month, 7.8 TB/year**, in one unpartitioned table on one database, plus a full standby copy and ~2.4× that in transient WAL.
- **Reproduction** §7 arithmetic over the measured `pg_total_relation_size`.
- **Fix** `PARTITION BY RANGE (created_at)` with detach-and-archive. `events` is never updated, which makes this unusually clean.
- **Regression test** Not a test — an operational runbook and a monitored growth alert.

---

**DB-07 — Depth sampler does a full table scan; its own comment is wrong**
Severity MEDIUM · Confidence High · Launch-blocking: no
- **Location** `engine/internal/taskq/depth.go:182` (`countSQL`); the incorrect claim is the comment at `depth.go:180`.
- **Failure sequence** The query has no `task_type` predicate, so `tasks_pollable`'s second column is skipped and PostgreSQL 16 has no skip-scan.
- **Impact** **Measured Seq Scan, 200,000 rows, 160,000 Filtered, 28.65 ms, 3,125 pages**, every 5 s per node. At a 1M-row backlog, ~150 ms and ~120 MB per pass — enough to evict all 128 MB of `shared_buffers` every 5 s on three nodes at once, exactly when the cluster is in trouble.
- **Reproduction** §4.3 `EXPLAIN (ANALYZE, BUFFERS)`.
- **Fix** Add `task_type = ANY(...)` so the index applies, or use a cheaper estimate.
- **Regression test** Assert the plan is an index scan at a seeded depth of 100k rows.

---

**DB-08 — Pool saturation destroys completed activity results**
Severity MEDIUM · Confidence Medium · Launch-blocking: no
- **Location** `engine/internal/runtime/activity_worker.go:273` `keepClaimed`, renewal via `taskq/queue.go:189` `extendSQL` on the shared pool; discard path at `activity_worker.go:125–138`.
- **Preconditions** Activity longer than `lockDuration/3` (10 s by default) **and** a saturated 25-connection pool.
- **Failure sequence** Renewal `Extend` queues for a connection → queues past the remaining lock → `ErrLockLost` → `actx` cancelled → activity aborted or its result discarded → task re-polled and the activity re-run. Overload destroys finished work and adds load.
- **Impact** Positive feedback into collapse, alongside DB-03. Not a safety hole: the dedup key covers the repeated side effect.
- **Reproduction** §9 test R2 — saturate the pool while a >10 s activity runs; expect `activity task claim lost while running` in the node log.
- **Fix** A small dedicated pool, or a reserved connection, for lease and claim renewal — control-plane traffic should not queue behind data-plane traffic.
- **Regression test** Assert no claim is lost when the pool is saturated by 100 concurrent pollers for 30 s.

---

**DB-09 — Claim renewal is not fenced, lengthening failover**
Severity MEDIUM · Confidence High · Launch-blocking: no
- **Location** `engine/internal/taskq/queue.go:189` `extendSQL` — matches `lock_token` and `locked_until`, never consults `shard_leases`.
- **Impact** A fenced-out node keeps renewing its activity claims until the activity ends, holding the task row away from the new owner for up to the activity's duration. Correctness is preserved (the commit is refused, the dedup key covers the effect); **recovery time is not**.
- **Fix** Pass the fencing token to `Extend` and join `shard_leases`.
- **Regression test** Fence a node mid-activity; assert the new owner can claim the task within one lock duration.

---

**DB-10 — No snapshot/limit on history means one workflow can OOM a node**
Severity MEDIUM · Confidence Medium · Launch-blocking: no (coupled to DB-04)
- **Location** `engine/internal/eventlog/store.go:64–76`; `mem_limit: 300m` and `restart: "no"` at `deploy/docker-compose.yml:68,73`.
- **Impact** 1,000 events × 1 kB payloads ≈ 1 MB per replay × 12 concurrent workers, with no cap anywhere. The node does not come back, by design, because auto-restart is deliberately off for the failover demo.
- **Fix** Cap history length (reject or continue-as-new); stream rather than materialise in `Load`.

---

**DB-11 — `tasks_pollable` index bloat is unmanaged**
Severity LOW · Confidence High · Launch-blocking: no
- **Location** `tasks_pollable`; no `REINDEX` or `pg_repack` anywhere in the repo.
- **Impact** **Measured 2.4 MB of index against 16 kB of heap and 48 live rows** after 118k rows of churn. A monotonic `BIGSERIAL` key means autovacuum cannot reuse the emptied left-hand pages.
- **Fix** Scheduled `REINDEX CONCURRENTLY`, or partition the queue by time.

---

**DB-12 — `settleClaim()` waits unbounded**
Severity LOW · Confidence High · Launch-blocking: no
- **Location** `engine/internal/runtime/activity_worker.go:314–318` — `cancel(); <-done`.
- **Impact** The worker goroutine blocks until the in-flight `Extend` returns. pgx honours context cancellation so this resolves in practice; a wedged socket would stall shutdown.
- **Fix** `select { case <-done: case <-time.After(5*time.Second): }` with a logged warning.

---

**DB-13 — Database concurrency is never exercised under the race detector**
Severity MEDIUM · Confidence High · Launch-blocking: no
- **Location** `internal/taskq/concurrency_test.go:17`, plus the `integration_test.go` files in `taskq`, `lease`, `eventlog`, `timers` — all gated on `DUREXEC_TEST_DSN`.
- **Impact** `go test -race ./...` passes 30/30 (§4.2) but covers **no** database concurrency, including the regression test for the poller-starvation defect the CTE rewrite fixed. `make test` does not set the DSN, so CI never runs them either.
- **Fix** A `make test-integration` target that starts a throwaway PostgreSQL, applies the migration, and runs `DUREXEC_TEST_DSN=... go test -race ./...`.

---

**DB-14 — The engine runs ~131 transactions/s while completely idle**
Severity MEDIUM · Confidence High · Launch-blocking: no
- **Location** `engine/internal/taskq/poller.go:21–22` (`IdleMin = 10 ms`, `IdleMax = 1 s`); one `pollSQL` per worker per tick, each its own implicit transaction.
- **Preconditions** Any running cluster, including one with no work at all.
- **Failure sequence** Each of ~16 shards × 12 workers = 192 pollers backs off to a 1 s ceiling and then polls forever at 1 Hz. There is no `LISTEN`/`NOTIFY`, so an idle shard is indistinguishable from a busy one in cost per tick.
- **Impact** **Measured 45,461,945 transactions over ~4 days = 131 tx/s sustained** on a near-idle cluster (predicted ~192/s). Each is a write transaction (`pollSQL` is an `UPDATE`), so each consumes an XID and generates WAL. This is the floor under every other cost, it scales with shard count × worker count rather than with work, and it is most wasteful in exactly the deployment the project is named for — profile B, where most workflows are asleep.
- **Reproduction** `SELECT xact_commit FROM pg_stat_database WHERE datname='durexec';` twice, 60 s apart, on an idle cluster.
- **Fix** `LISTEN`/`NOTIFY` on task insert, with the 1 Hz poll kept as a safety net. Removes nearly all idle traffic and shortens wake-up latency, which also eats into the ~2.01 s latency floor in §5.1.
- **Regression test** Assert idle transactions/s falls below 10 with no work queued.

---

## 9. Reproducible benchmark plan

Preconditions for every test, from the evidence in `docs/results/`:

1. **Record `uptime` before and after.** A run whose 1-minute load exceeded ~4 at any point is void. The 0.00 wf/s anomaly makes this non-negotiable.
2. Use a **throwaway** database, not the demo cluster's. Add `max_connections` explicitly and keep `3 × 25 + loadgen + psql + headroom` under it (DB-05).
3. Write every trial to CSV **flushed per row**, with the resume command in the file header, per the project's own rule. A run killed at trial 37 must leave 37 usable rows.
4. Run detached with `nohup`, own log file.
5. **Declare the service floor** for the workload before starting, and void any point whose p50 sits on it (DB-02). For `DeliveryOrder` with `-prep 50ms` the floor is ~2.01 s.
6. Snapshot `pg_stat_database`, `pg_stat_bgwriter`, `pg_stat_user_tables` and `pg_stat_activity` before and after each trial, and diff them. None of the existing runs do this, and it is where the bottleneck evidence lives.

| ID | Test | Method | Measures | Answers |
|---|---|---|---|---|
| **S1** | Sustained, open-loop | Replace the closed-loop generator with a **fixed arrival rate**; sweep 5→250 wf/s at 1, 2, 4, 8, 16 shards. Hold 10 min per point. | p50/p95/p99, completed/s, queue depth trend | The real saturation point. A closed-loop generator **cannot** answer this (DB-02). Highest-value missing experiment. |
| **S2** | Lease-lock isolation | At 1 shard, fix arrival rate at 80% of S1's knee; vary workers 4→8→16→32. | commits/s per shard | Proves DB-01: throughput must not move. |
| **S3** | Batched-commit A/B | Apply the DB-01 fix on a branch; re-run S2. | commits/s per shard | Quantifies the 3–4× prediction. |
| **B1** | Burst | 10× the S1 knee for 60 s, then back to 50%. | time to drain, p99 during and after, whether throughput recovers | Whether overload is recoverable or collapses (DB-03, DB-08). |
| **B2** | Poll cost vs depth | Seed `tasks` to 0 / 1k / 10k / 100k rows per shard with 80% locked; `EXPLAIN (ANALYZE, BUFFERS)` the exact `pollSQL`. | ms/poll, rows filtered, Sort presence | DB-03. **Partially run already** (§2.4: 0.29 ms at 48 rows, 15.2 ms at 12.5k/shard). Fill in the curve. |
| **L1** | History length | Workflows with H = 10 / 50 / 100 / 500 / 1000 events; 50 of each. | event rows read per task (from `pg_stat_user_indexes` diffs), p99 latency, container RSS | DB-04, DB-10. Confirms or refutes the O(H²) projection. |
| **L2** | Long duration | 24 h at 50% of the S1 knee. | `events` and index size per hour, `n_dead_tup`, autovacuum count, WAL generated, index bloat | DB-06, DB-11. Validates the 656 GB/month projection against reality. |
| **R1** | Recovery after overload | B1's burst, then `docker kill` one node mid-burst. | time to full throughput, duplicate activity count, lost claims | Profile C's worst case. |
| **R2** | Renewal under saturation | 100 concurrent pollers plus one 30 s activity. | count of `activity task claim lost while running` in the node log | DB-08. |
| **R3** | Lease starvation | Hold a `CommitDecision`-shaped transaction on one lease row for 4 s, 8 s, 12 s; watch that shard's ownership. | spurious failovers | Whether DB-01 can expire a lease and explain the 0.00 wf/s. |
| **C1** | Integration race | `DUREXEC_TEST_DSN=<throwaway> go test -race ./...` | race reports | DB-13. **One line, and it should be done first.** |
| **D1** | Synchronous replication cost | Set `synchronous_standby_names='ANY 1 (walreceiver)'`; re-run S1 at 1 shard; re-run the promotion drill. | commits/s, events lost | Prices the §3.2 trade and turns "0–2 events lost" into a deliberate, costed choice. |

Priority if time is short: **C1** (one line), **S1** (the only test that can
produce a defensible absolute throughput number), **B2** and **L1** (the two
scalability claims most at risk), then **D1** (strongest paper material: a
measured durability/throughput trade-off).

---

## 10. What I could not measure

| Item | Why |
|---|---|
| Saturation throughput at any shard count | The only generator in the repo is closed-loop, so it measures `concurrency ÷ latency`. Needs S1. |
| 8- and 16-shard scaling | No recorded run goes past 4 shards. The §2.2 ceiling of ~205 wf/s is **derived**, not measured. |
| p95/p99 under genuine saturation | Recorded p95/p99 come from closed-loop runs, several of them generator-bound. |
| Database concurrency under `-race` | `DUREXEC_TEST_DSN` unset by choice (live demo, two other agents on the cluster). **NOT RUN.** See C1. |
| Container RSS under load | Would have required loading the demo cluster. **NOT RUN.** |
| Deadlock and serialization-failure counters | `pg_stat_database` was not snapshotted before/after a load run. **NOT RUN.** |
| Autovacuum behaviour on a large `events` | The table is 57 MB. Nothing here predicts 600 GB. **NOT RUN.** |
| Promotion drill re-run | Trusted the recorded `standby-promotion-run2/run3.txt`; verified the *configuration* explains them (§3.2) rather than re-running and disturbing the demo. |
| Fourth-node connection exhaustion | Predicted from the measured 82/100 and the code at `main.go:487`. **NOT RUN** — would have broken the cluster. |

---

## 11. Cluster state left behind

No production code, migration, or file in `docs/results/` was modified, deleted
or overwritten. Nothing was committed.

Actions taken against the live cluster, all read-only or session-local:

- `SELECT`s against `pg_settings`, `pg_stat_activity`, `pg_stat_user_tables`,
  `pg_stat_user_indexes`, `pg_stat_bgwriter`, `pg_stat_replication`, and
  aggregate counts over `events`.
- One `EXPLAIN (ANALYZE)` of a `pollSQL`-shaped `UPDATE` against the real
  `tasks` table. It matched **0 rows** (`rows=0`, visible in the §2.4 plan) and
  so modified nothing.
- Two `CREATE TEMP TABLE` + `CREATE INDEX` + `INSERT 200000` sequences inside
  single `psql` sessions. Temp objects are dropped when the session ends; both
  sessions exited. No production table was touched.
- `go test -race ./...` in `engine/`, which builds into the Go cache and skips
  every database-backed test.

State verified at the end of the audit (23:40, load 1.50):

```
SELECT count(*) AS lease_rows, count(*) FILTER (WHERE expires_at > now()) AS live
  FROM shard_leases;          ->  16 | 16        (16 rows, all leases live)

curl -o /dev/null -w '%{http_code}' http://localhost:808{1,2,3}/healthz
                              ->  200, 200, 200  (all three nodes answering)

docker ps                     ->  node-a, node-b, node-c healthy
                                  deploy-postgres-leader-1 healthy
                                  deploy-postgres-standby-1 healthy (streaming)
                                  deploy-embedder-1 healthy (overlay applied)
                                  deploy-prometheus-1, deploy-grafana-1 healthy
```

`pg_stat_replication` still shows `state = streaming`. The embedder container
reports healthy under its own compose healthcheck; a probe of `/health` returns
404 because its route is `/embed`, not `/health` — that is the expected
response, not a fault.

**The cluster is healthy and in the state `make up-semantic` leaves it.**

One observation not caused by this audit: **`node-a`, `node-b` and `node-c` were
22 seconds old** when first inspected, while `postgres-leader`, `postgres-standby`,
`prometheus`, `grafana` and `embedder` had been up 7 hours. The engine services
carry `restart: "no"`, so they cannot have restarted themselves — something ran
`make up` or recreated them shortly before. Most likely a parallel audit agent.
Shard 4 was at `fencing_token = 80` against 5 on most shards, so that shard has
changed hands repeatedly. Flagged so it is not mistaken for a defect found here.
