# engine

> The repository root has its own README with the project's big picture and
> where everything lives (design docs, papers, the presentation deck). This
> file is only about the code in this directory. Start at the
> [top-level README](../README.md) if you have not already.

## What this is

Think of a food delivery order: take the payment, reserve the food, tell the
kitchen, send a driver. Each of those steps has to happen, in order, and if
any of them fails partway through, the ones that already happened need to be
undone (refund the payment, release the reservation). That whole sequence is
called a "workflow" in this project.

The hard part is that a workflow can take a long time — minutes, sometimes
much longer — and the computer running it can crash in the middle. This
engine's job is to make sure a crash never loses or repeats a step. It does
that by writing down every decision to a database *before* acting on it, so
if the machine running a workflow dies, a different machine can read those
notes back and pick up exactly where the first one left off, without redoing
anything or skipping anything.

To handle more workflows than one machine can, the work is split into a fixed
number of partitions called "shards" (16 by default). Which shard a given
workflow belongs to never changes. Which machine currently *owns* that shard
can change at any time — ownership is a lease, a row in the database with an
expiry time, so if the owning machine dies the lease simply expires and
another machine picks the shard up. Every node in the cluster is given the
same list of peers, and each one runs the same consistent-hash calculation
over that list to agree, without asking anyone, on who should own what.
There is no separate coordination service (no ZooKeeper, no etcd) — the
database is doing that job too.

## Run it in under a minute

```sh
cd engine
make up      # builds and starts 3 engine nodes, Postgres (leader + a
             # streaming replica), Prometheus and Grafana, and waits for
             # every healthcheck to pass
make demo    # places one food-delivery order and prints its status
             # until it finishes
```

Then open **http://localhost:8081/** — the built-in web dashboard. It also
answers on 8082 and 8083 (one port per node); any of the three shows the
whole cluster. From there you can place orders, watch them run, read a
workflow's event history, see which node owns which shard and how many each
one holds, look at the semantic-deduplication audit trail, and arm a
deliberate failure.

Grafana is at **http://localhost:3000** (`admin` / `admin`, or continue as a
viewer) with a dashboard already wired to Prometheus, which is at
**http://localhost:9090** if you want to run a query by hand.

When you are done: `make down` (data is kept; add `-v` — see the Makefile —
if you want a completely clean slate).

## Layout of `internal/` and `app/`

| Package | What it does |
|---|---|
| `internal/core` | Every shared type: ids, events, commands, the storage interfaces. Frozen — nothing else may change it. |
| `internal/eventlog` | Reads and writes the append-only event log in PostgreSQL; runs the schema migration. |
| `internal/lease` | The shard-ownership lease: acquire, renew, and detect when a lease has expired or been lost. |
| `internal/shard` | Maps a workflow's business key onto a shard, and separately tracks which shards this node currently owns via the consistent hash ring. |
| `internal/runtime` | The integration layer: wires the dispatcher, the task queue, the committer and the semantic checker into one running node. |
| `internal/workflow` | The deterministic context a workflow function runs against (`ExecuteActivity`, `Sleep`, `WaitForSignal`, …) — the part that makes replay safe. |
| `internal/activity` | Decides how an activity should run: the registry, the retry policy, the dedup key. Runs no I/O itself. |
| `internal/saga` | Compensation bookkeeping for workflows that need to undo completed steps when a later step fails. |
| `internal/taskq` | The lease-based work queue that hands tasks to workers and makes a crashed worker's task visible again. |
| `internal/timers` | Durable timers (`Sleep`), stored as database rows rather than in-process timers, so they survive a restart. |
| `internal/idem` | Idempotency, as two strategies under one umbrella: `idem/exact` (byte-identical arguments) and `idem/semantic` (a model decides whether two differently-worded calls mean the same thing). |
| `internal/faultinj` | The deliberate-failure switch used by the demo and by CI's fault-injection stage. Inert unless `FAULT_INJECTION=true`. |
| `internal/api` | The HTTP front door: routing, request/response shapes, error mapping. |
| `internal/config` | Loads and validates settings from the environment (see the table below). |
| `internal/obs` | Prometheus metrics and structured logging. |
| `internal/webui` | The single-file dashboard described above, embedded into the binary with `go:embed`. |
| `internal/bench` | The in-memory performance harness for the design document's two latency requirements (replay time, added dedup latency). No database or network involved. |
| `app/delivery` | The first demonstration workflow: an ordinary food-delivery saga, where every step has a natural exact idempotency key. |
| `app/agent` | The second demonstration workflow: a travel-booking agent whose tool calls come from a language model, so arguments are English sentences — the case that needs semantic deduplication, not just exact matching. |

## Configuration

Every environment variable below is read in `internal/config/config.go`; that
file is the source of truth if this table and the code ever disagree.

| Variable | Default | What changing it does |
|---|---|---|
| `NODE_ID` | the container hostname | The name this node claims shard leases under. Two nodes sharing one name would each believe they own the other's shards, so it must be unique per node. |
| `CLUSTER_NODES` | just this node | Comma-separated list of every node id in the cluster, including this one. The consistent hash ring is built from this exact list, so **every node must be given the same list** — a node left off it is never assigned work. |
| `LISTEN_ADDR` | `:8080` | The address the HTTP API (and the dashboard) listens on. |
| `DATABASE_URL` | *(required)* | The Postgres connection the event log, leases and task queue all live in. The process refuses to start without it. |
| `SHARD_COUNT` | `16` | The fixed number of partitions the whole keyspace is split into. Changing it re-maps every workflow to a different shard, so it should not change after data exists. |
| `LEASE_TTL` | `10s` | How long a shard lease survives without renewal — also the worst-case time a shard sits unowned after its node dies. |
| `LEASE_RENEW` | `3s` | How often the owner refreshes its lease. Must be at most a third of `LEASE_TTL`, or a single missed renewal can hand the shard away while work is still in flight. |
| `TASK_LOCK` | `30s` | How long a polled task stays invisible to other workers before it is considered abandoned and re-offered. |
| `WORKFLOW_WORKERS` | `4` | How many goroutines this node runs to process workflow tasks concurrently. |
| `ACTIVITY_WORKERS` | `8` | How many goroutines this node runs to execute activities concurrently. |
| `SYNC_COMMIT` | `true` | Maps to PostgreSQL `synchronous_commit`. Turning it off trades away the durability guarantee this engine exists to provide, in exchange for throughput — only for benchmarking. |
| `TAU_HIGH` | `0.90` | The similarity score above which the semantic dedup check calls two calls a duplicate. |
| `TAU_LOW` | `0.85` | The similarity score below which the check calls two calls genuinely distinct. Between `TAU_LOW` and `TAU_HIGH` the verdict is "uncertain" (see `ESCALATION_BLOCKS`). |
| `EMBEDDER_URL` | *(unset)* | Address of a real embedding service. Left unset, the node falls back to a built-in deterministic stand-in encoder — see Known limitations. |
| `ESCALATION_BLOCKS` | `false` | What happens on an uncertain verdict. `false`: the activity runs anyway, and the verdict is still journaled. `true`: the activity is held and the run waits for a person to decide. |
| `FAULT_INJECTION` | `false` | Arms the ability to deliberately fail a step on command. Must be set explicitly in the environment — it can never be turned on by code, so a real deployment cannot have it left on by accident. |

## Reproducing the fault-injection demonstration

This is the same sequence `.github/workflows/ci.yml` runs on every push, done
by hand so you can watch it happen.

1. **Start the cluster and confirm it's healthy.**
   ```sh
   make up
   curl -s http://localhost:8081/healthz
   ```

2. **Send it some work.** Run `make demo` a few times, or open
   http://localhost:8081 and place a few orders from the form. Give at least
   one order a longer prep time (the "Prep time" field) so it is still
   running when you do the next step.

3. **Watch who owns shard 2.** Either read it straight off the dashboard's
   "Shard ownership" card, or:
   ```sh
   curl -s http://localhost:8081/v1/shards | jq '.shards[] | select(.shard_id==2)'
   ```
   Note the `owner` field and its `fencing_token`.

4. **Kill that node.**
   ```sh
   make kill-owner
   ```
   This asks any node for the current owner of shard 2 and runs
   `docker kill` on it — a hard, unclean stop, on purpose. The killed
   container is *not* auto-restarted, so the failover is real and visible
   rather than the same node quietly coming back.

5. **Watch ownership move and the fencing token climb.** `LEASE_TTL` is 10
   seconds, so within roughly that long another node takes the shard over.
   The dashboard's shard table flashes the row when the token changes; from
   the command line:
   ```sh
   watch -n1 'curl -s http://localhost:8082/v1/shards | jq ".shards[] | select(.shard_id==2)"'
   ```
   The `fencing_token` must be strictly higher than the value you noted in
   step 3 — that increase is the proof that ownership actually moved, rather
   than two nodes both believing they hold the shard. In Grafana, the
   `shard_recovery_seconds` and `lease_transitions_total` panels show the
   same event.

6. **Confirm the in-flight workflow still finished.** Check its status on the
   dashboard or with `curl .../v1/workflows/<id>`; it should reach
   `COMPLETED` (or a deliberate `FAILED`/`COMPENSATING_FAILED` if you armed a
   non-retryable failure) even though the node that started it is gone.

7. **Prove nothing was written twice.** This is the actual safety claim, and
   it is checkable, not just observable:
   ```sh
   make verify
   ```
   `make verify` runs `deploy/assert_no_duplicates.sh`, which queries the
   event log for any `(shard_id, workflow_id, run_id, seq)` tuple that was
   ever written more than once, and fails if it finds one. A clean run means
   the workflow that was in flight when its owning node died picked up on
   the new owner without repeating or losing a step.

8. **Tear down.** `make down`.

## Known limitations

- **The event log never shrinks.** There is no garbage collection or
  archiving of old events; every append lives in Postgres forever. A
  long-running deployment needs an archival strategy this codebase does not
  provide.
- **One database, not one per shard.** Shards are a logical partitioning
  inside a single PostgreSQL instance (leader plus a streaming replica), not
  physically separate databases. The isolation between shards is enforced by
  the fencing-token check on every write, not by the storage layer.
- **The built-in encoder is a stand-in, not a model.** Without `EMBEDDER_URL`
  pointing at a real embedding service, semantic dedup runs on a
  deterministic hash-based fake. It is enough to exercise the plumbing —
  the verdict pipeline, the audit trail, the escalation band — but it cannot
  actually recognise that two differently-worded requests mean the same
  thing. Trust the verdicts you see with the fake encoder as evidence the
  system works, not as evidence about language understanding.
- **This is at-least-once execution with strong deduplication, not
  exactly-once.** A crash between performing a real-world effect (charging a
  card, calling an external API) and recording that it happened is
  indistinguishable, from outside the process, from a crash before the
  effect ran at all. The engine's answer is to make that gap vanishingly
  unlikely to be observed twice — the fencing token and the event log
  together stop a *second* node from repeating the effect — but it cannot
  promise the effect itself happens exactly once, only that it is recorded
  at most once and resumed correctly either way.

## Integrator note

`internal/webui` builds the dashboard as a plain `http.Handler`:

```go
import "github.com/dharaneswar339/durexec/internal/webui"

mux.Handle("/", webui.Handler())
```

It only answers `/` and `/index.html`; mounting it at the root of the same
mux as the API's `/v1/*`, `/healthz` and `/metrics` routes is safe and is
what the dashboard's own JavaScript assumes (it calls the API with relative
paths, so it must be served from the same origin as the API).
