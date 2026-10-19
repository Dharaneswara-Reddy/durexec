# Five defects, fixed: before and after

**Answer first: every published figure still stands.** Safety is unchanged and
throughput is unchanged. Nothing in `docs/results/` was overwritten; the
"after" files sit beside the originals.

Two liveness defects were the brief. Three more turned up while fixing them: a
comment that described a database layout the system does not use, a test that
deleted four shards' lease rows from the live cluster and left them deleted,
and a health endpoint that reported the cluster's shard count under the name of
the owned set.

## What was wrong

None of the five was found by a test failing. Every one of them was live while
`go test ./...`, `make verify` and all three `/healthz` endpoints were green.
The first two are **liveness only** — nothing was corrupted and no side effect
ever happened twice.

| | What happened | Why it survived every test |
|---|---|---|
| **1. Workers retired on a sequence race** | Two writers racing for the same event sequence number: the loser's unique-violation was reported as *lost shard ownership*, and a worker told it lost the shard exits for good. Four races retired all four of a shard's workflow workers, and `WorkerPool.Start` would not replace them because the shard was still in its running set. The shard then made no progress while the node reported healthy and kept the lease. | The losing transaction rolls back, so the log stays perfect. The symptom is a stalled shard, which no safety assertion looks for. |
| **2. A slow activity ran twice** | A task claim lasts `TASK_LOCK` (30 s) and nothing renewed it. An activity that ran longer — including its in-process retries — had its claim lapse, a second worker claimed the row, and the same activity ran **concurrently on a healthy node**. `Queue.Extend` existed for exactly this and nothing outside the tests called it. | The dedup key keeps the world consistent, and the second run's commit loses a sequence race — which then triggered defect 1. Every demonstration activity finishes in under half a second, so it never fired. |
| **3. A wrong comment** | `migrations/0001_init.up.sql` said "One database per shard", and `docs/ENGINE_INTERNALS.md` said the same plus "five tables". There is one shared database, sharded by a `shard_id` column, and six tables. | Nothing executes a comment. |
| **4. The test suite killed four shards** | `TestEnsureRowsIsIdempotent` deleted `shard_leases` rows 0–3 **and deleted them again in its cleanup**, so running the suite against the live database — which the Makefile tells you to do — left four shards with no lease row. `Acquire` is an `UPDATE`, so nothing recreates a missing row while a node is up: those shards stayed dead until a restart, refusing about half of all start requests. | A missing lease row is reported as lost ownership, exactly as designed. Every layer behaved correctly while the cluster did no work. |
| **5. `/healthz` always said `owned_shards=16`** | It served `len(OwnedShards)`, and `engined` sets that to **every** shard in the cluster, once, at startup. So all three nodes claimed all sixteen shards — including while four of them were dead. | It is a constant, so it is never inconsistent with itself. |

Defects 4 and 5 were found **while** re-running the fixes for 1 and 2, and they
are the more valuable pair: they are the fourth and fifth time this project has
caught a health signal lying about a broken cluster. Full write-up in
`green-but-wrong-findings.txt`.

## What the re-runs show

| Measurement | Before (published) | After | Verdict |
|---|---|---|---|
| `go build` / `go vet` / `go test ./...` | pass | pass, also under `-race` and with `DUREXEC_TEST_DSN` set | unchanged |
| `make verify` (no duplicate event tuple) | OK | OK | unchanged |
| Acceptance matrix, all 36 rows | 0 FAIL — 16 PASS, 17 MANUAL, 3 BLOCKED | 0 FAIL — 16 PASS, 17 MANUAL, 3 BLOCKED, **every row the same outcome**, re-checked twice (after the liveness fixes, and again on the final image) | unchanged |
| C-2, the real duplicate-step check | 0 steps twice in 6,770 runs / 81,252 events | **0 steps twice in 9,545 runs / 114,565 events** | unchanged, on 41% more runs |
| Duplicates under two node kills | 0 duplicates in 3,512 completions / 878 runs | **0 duplicates in 3,052 completions / 763 runs** | unchanged |
| …admission failures in that run | 122 of 1,000 | 237 of 1,000 | worse, and it is kill timing, not the engine — see below |
| Throughput, c=128, 1 / 2 / 4 shards | 12.80 / 23.15 / 46.30 wf/s | **12.80 / 24.10 / 47.15 wf/s** on the final image, and 12.80 / 24.50 / 47.40 on the image before it | unchanged — the 1-shard figure is identical to three significant figures in both runs |
| …scaling 1→2 and 2→4 | 1.81× / 2.00× | **1.88× / 1.96×** (and 1.91× / 1.94×) | unchanged, NFR-5 still MET |
| …p50 latency at 1 / 2 / 4 shards | 9.80 / 5.46 / 2.02 s | 9.52 / 5.39 / 2.01 s | unchanged |
| New in production: `worker_task_failures_total{reason="seq_race"}`, `{reason="task_lock_lost"}`, log line "replacing exited shard workers" | — | all **zero** over ~8,900 runs and two node kills | neither defect fired live |
| Lease rows surviving a full `go test ./...` against the live database | 12 of 16, four shards dead until a restart | **16 of 16**, re-owned within one lease tick, 10 of 10 starts accepted | fixed |
| `/healthz` across the three nodes | `16` + `16` + `16` on a 16-shard cluster | **`4` + `9` + `3` = 16**, with `cluster_shards: 16` beside it | fixed |

## Three things that weaken this, stated plainly

**Throughput is only re-measurable when the desktop is quiet.** The same
unchanged binary produced wildly different absolute numbers over two hours,
tracking the host's load average, not anything in the engine:

| 1-min load before the sweep | c=128, 1 / 2 / 4 shards |
|---|---|
| **1.27** (final image) | **12.80 / 24.10 / 47.15** ← matches published |
| **3.11** | **12.80 / 24.50 / 47.40** ← matches published |
| 3.9 (15 s windows) | 15.40 / 20.00 / 45.20 |
| 5.4 | 0.00 / 8.00 / 14.75 |
| 5.4 | 0.00 / 5.55 / 16.85 |

At load 5.4 nothing at all completed inside a 20 s window at one shard. The
host was running two browsers and two unrelated containers. Both quiet-host
runs reproduce the published figures, independently and on two different
builds, so the published numbers are confirmed — but only a quiet host can
confirm them, and a reader of this file should discard any row above whose
load column is over about 4. The load figure is taken *before* each sweep
starts, so it is background load and not the test's own.
This also means **the worry that the published throughput was understated is
answered as "no"** — the published numbers were already what this engine does
when the machine is quiet — rather than being dodged.

**Neither defect was seen firing on the live cluster**, so the evidence that
they are fixed is the tests, not the deployment. That is not an accident: fixing
defect 2 removes the commonest *cause* of defect 1, and the heartbeat does
nothing until an activity has run for a third of `TASK_LOCK`, which is 10 s.
Every test in `liveness-fix-demonstrations.txt` was run against the old code
first and does fail there; the real-PostgreSQL test reproduces defect 2 on
demand against the live leader.

**The duplicate run refused more work at admission** (237 of 1,000 against 122).
Those are `409 NotOwner, shard currently unowned` — the window after a kill
before a lease expires and is re-taken. It depends entirely on when the kills
land relative to the 10 s lease TTL, and it is an availability number, not a
safety one. The safety number, zero duplicated steps, is identical.

## Was the worker pool given the ability to recover a shard?

Yes, and deliberately narrowly. `WorkerPool.Start` now tops a shard back up to
full strength instead of returning early. It cannot fight the lease manager,
because its only caller is `Supervise`, which iterates the manager's own owned
set — a shard this node has legitimately lost is not in that set and is never
restarted — and because replacement workers run under the shard's **existing**
context, so a `Stop` landing a moment later cancels them exactly as it cancels
the originals. The reasoning is written out in full in the comment above
`Start`.
