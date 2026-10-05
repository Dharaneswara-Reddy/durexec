# durexec — a sharded durable execution engine

Long-running workflows that survive machine failure, split across shards, with no coordination
service — and a duplicate-action check that compares meaning rather than text.

Written in Go on PostgreSQL. No message broker, no ZooKeeper, no Raft cluster.

---

## The problem

A workflow is a business process made of several steps that must all happen: take payment, reserve
stock, notify the restaurant, dispatch a driver. It may run for seconds or for days, and its steps
touch the outside world — once a card is charged, that cannot be quietly undone.

If the machine running the workflow dies halfway through, only two things can happen and both are
wrong. The completed work is lost and the process silently stops, or the work is repeated on restart
and the customer is charged twice.

## The approach

Every decision a workflow makes is written to an append-only log in PostgreSQL **before** it is acted
on. When a machine disappears, another machine reads that log back, re-runs the workflow code against
it, and arrives at exactly the point the first machine reached. Steps that already completed return
their recorded result instead of running again.

Work is partitioned across a fixed number of shards so capacity grows with machines, and ownership of
each shard is held by a renewable lease in a database row, guarded by a fencing token. That is why
the system has exactly one dependency.

## Measured results

Every figure below came from a three-node cluster under induced failure, not from an estimate.

| Property | Target | Measured |
|---|---|---|
| Committed work lost when two nodes are killed mid-run | zero | **zero** — all 878 accepted workflows completed |
| Steps executed twice, across 1,000 workflows under failure | zero | **zero**, across 3,512 completed steps |
| Throughput gain, one shard to two | 1.7x | **1.81x** (and 2.00x from two to four) |
| Rebuilding workflow state from history, 200 events | under 50 ms | **0.9 ms** at the 99th percentile |
| Shard rebalance when a 4th node joins a 3-node cluster | a minority moves | **5 of 16 shards** move; 11 untouched |

Raw output is in [`docs/results/`](docs/results/), including the runs that turned out to be invalid
and why.

## Quick start

```sh
cd engine
make up        # 3 engine nodes, PostgreSQL leader + standby, Prometheus, Grafana
make demo      # drive a food-delivery workflow end to end
make kill-owner # kill the node owning a shard and watch another take over
make verify    # assert no duplicate events were written
```

The dashboard is at <http://localhost:8081/>.

## How it works

| Mechanism | Where |
|---|---|
| Append-only event log, atomic decision commit | `engine/internal/eventlog` |
| Deterministic replay, workflow context and futures | `engine/internal/workflow` |
| Consistent-hash ring and shard mapping | `engine/internal/shard` |
| Lease ownership, fencing tokens, handback | `engine/internal/lease` |
| Durable task queue (`FOR UPDATE SKIP LOCKED`) | `engine/internal/taskq` |
| Durable timers | `engine/internal/timers` |
| Saga compensation in LIFO order | `engine/internal/saga` |
| Exact and semantic idempotency | `engine/internal/idem` |
| Node wiring and worker loops | `engine/internal/runtime` |

Full architecture: [`docs/DESIGN_DOCUMENT.html`](docs/DESIGN_DOCUMENT.html).
Mechanism-by-mechanism walkthrough: [`docs/ENGINE_INTERNALS.md`](docs/ENGINE_INTERNALS.md).

## Honest limitations

- **Semantic idempotency does not work yet.** Measured against a general-purpose sentence encoder it
  raises duplicate detection from 14.3% to 57.1%, but wrongly blocks 93.9% of genuinely distinct
  actions. Topical embeddings cannot separate actions that differ only in a date or a passenger
  count. The fix is to compare those fields exactly and reserve similarity for free text.
- **Shards share one database**, separated by a `shard_id` column, rather than one database each.
  Every key already carries the shard id, so per-database deployment is a change of configuration
  rather than of schema.
- **The event log has no garbage collection** and grows without bound.
- **This is at-least-once execution with strong deduplication, not exactly-once.** A crash between
  performing an effect and recording it is indistinguishable from the outside.

## Status

Engine complete and demonstrated under failure. Semantic idempotency built and measured, but the
detection quality is not yet usable — see the limitation above.
