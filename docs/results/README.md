# Measured results

**The cloud campaign that would fill the NOT RUN rows below is designed in
[`../cloud-deployment/CLOUD-TEST-CAMPAIGN.md`](../cloud-deployment/CLOUD-TEST-CAMPAIGN.md).**

**Local exploratory shard/concurrency sweep (2026-10-29), NOT study results:**
[`local-shard-sweep/FINDINGS.md`](local-shard-sweep/FINDINGS.md) — refutes this
plan's own shard-scaling prediction and shows the fixed-concurrency sweep was
offered-load-limited by 2.74x.

**AWS campaign (2026-10-30): prepared, NOT RUN** — evidence inventory, matrix and
harness fixes in [`../cloud-campaign/`](../cloud-campaign/README.md). No cloud
result exists; `cloud-campaign/results.csv` appears on the first run.

**For a one-page roll-up of the best valid run of every metric, read
[`../RESULTS_SUMMARY.md`](../RESULTS_SUMMARY.md).** This file remains the
per-run index, including the runs that are invalid and why they are kept.

Every file here is output from a run against the live three-node cluster or the
real `all-MiniLM-L6-v2` encoder. Nothing is estimated.

| File | What it measures | Status |
|---|---|---|
| `scaling-saturated.txt` | NFR-5 throughput at 1, 2 and 4 shards, concurrency 64 and 128 | **Valid.** 1→2 shards = 1.81×, 2→4 = 2.00× at c=128 |
| `scaling-c16-GENERATOR-BOUND.csv` | The same test at concurrency 16 | **Invalid as scaling evidence.** Kept deliberately — see below |
| `duplicates-under-failure.txt` | NFR-1, NFR-3, NFR-4 — 1,000 workflows while two nodes were killed mid-run | **Valid.** 0 duplicated steps in 3,512 completions; 0 accepted work lost; 122 refused at admission |
| `standby-promotion-run2.txt`, `standby-promotion-run3.txt` | FR-9 failover: leader SIGKILLed under load, standby promoted, nodes repointed (`make promote-drill`) | **Valid.** Promotion 0.1 s; engine works on the promoted DB; **not lossless: 2 and 1 committed events lost** — see below |
| `duplicates-undisturbed.txt` | NFR-3 on a cluster nobody disturbed | **Weak evidence** — see below |
| `acceptance-matrix.txt` / `.csv` | Every FR and NFR checked against the live cluster by `cmd/acceptance -inject` | 0 FAIL. MANUAL rows are ones the tool cannot verify from outside — see the other files for those |
| `semantic-experiment-minilm.html` | Precision–recall of semantic idempotency, real encoder | Labels generated, not hand-assigned |
| `phase6-sweep.csv` | **Phase 6.** Open-loop offered-load sweep on the 2-vCPU Kubernetes lab: generator view (offered, completed/s, p50/p95/p99, lag, in-flight slope, saturation verdict) joined to the infrastructure view for the same window (PostgreSQL txn/WAL/buffers/connections, container CPU and memory, engine pool pressure). 32 rows, 6 tags | **Valid, but NOT a capacity estimate.** 1 node = 12.98 wf/s, 2 nodes = 25.78 wf/s (1.99×). Rows tagged `baseline` at rates ≥16 are **GENERATOR_BOUND and invalid as engine evidence** — kept deliberately; superseded by tag `aw600` |
| `phase6-storage.txt` | **Phase 6.** Heap vs index size per relation, and cost per event and per workflow, read from `pg_total_relation_size` on the live database | **Valid.** 11.99 events/workflow, 418 B/event, 5,548 B/workflow. Also records `tasks` index bloat: 4,472 kB of indexes over 24 kB of heap |
| `phase6-crash-boundaries.txt` | **Phase 6 (F9-2).** Durable state after each armed crash boundary: workflow creation, decision commit (mid-run), and after-activity-journaled | **Valid for the blocks headed "RE-RUN AFTER THE F9-2 WIRING FIX".** The three blocks ABOVE that heading are **invalid** — they ran against a binary whose crash seams were dead, and are kept to document the defect |
| `phase7-extsim-matrix.txt` | **Phase 7.** External-effect failure matrix: seven scenarios (ambiguous response, transient pre-effect failure, journaled-then-crash, crash-before-invocation, saga-after-effect, claim expiry mid-flight, node-level SIGKILL mid-flight). Each reads PostgreSQL and, independently, an effect ledger held in a separate pod that survives the engine being killed | **Valid.** Every scenario: invocations ≥ effects, exactly one effect per dedup key, 0 duplicate `(run_id,seq)`, 0 pending tasks. Proves at-least-once invocation with **effectively-once effect, conditional on the recipient honouring the idempotency key** — NOT exactly-once. Scenario E records a real gap: a failed saga leaves the external effect standing with 0 compensations. The `(ledger unreadable)` lines are a harness defect in the per-key reader, preserved; the `counters` line carries the decisive numbers |
| `phase8-boundaries.txt` | **Phase 8.** The two boundaries Phase 7 left open: (H) external effect confirmed then a REAL self-`SIGKILL` before the result journal, via the new `BeforeResultJournal` seam; (I) shard ownership moved by pausing the owner while the effect was in flight, then thawing the stale worker | **Valid for the LAST H block only** — the first three are preserved failures: one armed the killing seam while an unrelated workflow was on a timer (both nodes died, start got HTTP 000), one used a broken quiescence query that matched every row, one read the seam log after the disarm had replaced the pod. H: `Requests=2 Performed=1 Replayed=1 Injected=0`, one journal completion. I: fencing token 63→64 mid-flight, one effect — but the stale worker was stopped by the **task-claim check, not fencing** |
| `phase8-restore-drill.txt` | **Phase 8.** Backup usability drill: `pg_dump -Fc`, `pg_restore -l` integrity listing, restore into a fresh database, then row-count and invariant verification | **Valid as a dump-usability result only.** 247 MB → 24.6 MB in 2.0 s; restore 4.3 s; all counts match; 0 duplicate journal records. It restored into a second database in the **same instance**, so recovery from losing the instance, volume or node is **NOT TESTED**. `archive_mode=off`, so there is no PITR at all |
| `cloud-tier1.csv` | **Phase 9.** Tier 1 cloud acceptance results, one row per case, appended and flushed as each finishes. Written by `engine/scripts/aws/acceptance_tier1.sh`; resumable, with the resume command in the file's own header | **Does not exist yet — NOT RUN.** Phase 9 was preparation only: no AWS resource was created, so there is no cloud result to report. The file appears on the first authorized deployment. Treating an absent file as a pass would be exactly the fabrication this project forbids |
| `cloud/<timestamp>-<label>/` | **Phase 9.** Evidence bundles from `collect_evidence.sh`: container logs, compose state, `docker stats`, host memory and disk, the four journal invariants, all nine PostgreSQL durability settings, the external-effect ledger when the simulator is running, and raw `/metrics` from each node | **Empty — NOT RUN.** Collected *before* any disarm or restart, because Phase 7 and Phase 8 each destroyed their own evidence by doing it the other way round |


| `semantic-experiment-minilm.csv` | Per-arm numbers behind the report | As above |
| `semantic-experiment-minilm.txt` | Full terminal output of that run | As above |
| `LIVENESS_FIX_BEFORE_AFTER.md` | The one-page before/after for the two liveness fixes — read this first, the four files below are its evidence | **Published figures still stand.** Safety unchanged, throughput unchanged |
| `scaling-saturated-after-liveness-fix.txt` | NFR-5 re-run after the fixes, same commands, c=64 and c=128, six sweeps in all | **Valid, and it reproduces the published run twice on two different builds**: 12.80 / 24.10 / 47.15 and 12.80 / 24.50 / 47.40 wf/s at c=128, against the published 12.80 / 23.15 / 46.30. The other sweeps are much lower — that is this desktop's background load, which the file records beside each sweep. Ignore any sweep whose load figure is above about 4 |
| `scaling-after-liveness-fix-15s-window.txt` | The first attempt at the above | **Not comparable.** loadgen's window is (duration − warmup), so `-duration 20s` gave 15s windows where the published file has 20s. Kept as a second data point |
| `duplicates-under-failure-after-liveness-fix.txt` | NFR-3 re-run after the fixes: 1,000 workflows, node-b killed at 8 s and node-c 12 s later | **Valid.** 0 duplicated steps in 3,052 completions across 763 runs. More admission failures than the published run (237 vs 122), which is kill timing, not safety |
| `acceptance-matrix-final-image.txt` / `.csv` | The matrix again on the image that also carries the `/healthz` and lease-test fixes, because `cmd/acceptance` reads `/healthz` and that endpoint changed | 0 FAIL, **all 36 rows the same outcome as the published run** |
| `acceptance-matrix-after-liveness-fix.txt` / `.csv` | The whole FR/NFR matrix re-run after the fixes | 0 FAIL, and **every one of the 36 rows has the same outcome as before the change** |
| `acceptance-matrix-after-liveness-fix-pre-load.txt` / `.csv` | The same matrix run before the load tests, when the event log held only 7 runs | **Weak.** Kept only because it is the run that revealed the log had been emptied; the row above is the one to read |
| `green-but-wrong-findings.txt` | Two more defects found while re-running the above: the test suite deleting four shards' lease rows from the live cluster, and `/healthz` reporting `owned_shards=16` on every node whatever it held | **Valid, both fixed.** Same class as the hash ring and the three different builds — green by every signal, silently wrong |
| `liveness-fix-demonstrations.txt` | The targeted tests for each defect, including the same tests failing against the old code | **Valid.** Both defects reproduce on demand and no longer happen |
| `semantic-experiment-hybrid-minilm.txt` / `.csv` / `.html` | The same sweep with the **opt-in hybrid comparator** added: deciding fields (route, date, daypart, passengers) compared exactly, similarity reserved for the free text. Same 213 pairs, same encoder, same cached vectors | **Does not replace the file above.** The hybrid arms score 1.000 on this corpus; the gate answered every pair and the model was never consulted, so those rows measure a hand-written field extractor and not semantic similarity — see below |

## Why the concurrency-16 run is kept

At concurrency 16 throughput was 8.5 wf/s whether the cluster had 2 or 4 shards,
which reads as "adding shards does nothing". It was the load generator, not the
engine. A closed-loop generator's throughput is its concurrency divided by
per-workflow latency — 16 ÷ 1.9 s ≈ 8.4 wf/s, matching the plateau almost
exactly. It measured itself. Raising concurrency until the engine was saturated
produced the valid numbers above. The file stays as a record of the trap.

## What the semantic numbers mean

They are real measurements of this mechanism with this model, over a corpus whose
labels were generated by construction. They are not a publishable accuracy result
until a person labels real retry pairs. Read them as a finding about the
approach, not as a score.

## What the hybrid run adds, and what it does not

`semantic-experiment-hybrid-minilm.*` is the stated fix from the baseline's own
diagnosis, implemented and measured. The pure-semantic path is unchanged and is
still what the engine runs by default (`SEMANTIC_HYBRID` is off); the semantic
rows in the hybrid file reproduce the baseline file exactly, which is what makes
the two comparable.

At the shipped thresholds, on the same 213 pairs:

| | detect (recall) | FPR | hard FPR | precision | F1 |
|---|---|---|---|---|---|
| exact (baseline) | 0.143 | 0.000 | 0.000 | 1.000 | 0.250 |
| semantic, band [0.85,0.90) | 0.571 | 0.939 | 1.000 | 0.341 | 0.427 |
| hybrid, band [0.85,0.90) | 1.000 | 0.000 | 0.000 | 1.000 | 1.000 |

The double-booking fixture is caught at every threshold in the sweep, including
the ones where the pure-semantic path escalated or missed it.

Three reasons that 1.000 is not a result to quote:

1. **The model was never consulted.** Once the deciding fields are removed from
   these intents, no free text is left, so the similarity half of the comparator
   contributed nothing. The figure measures exact matching over values scraped
   out of prose.
2. **The scraper's vocabulary was written by reading this corpus.** Its coverage
   of the corpus is part of what the accuracy measures — selection on the test
   set, and not fixable by writing the rules more carefully.
3. **The labels are still generated by construction**, exactly as in the
   baseline run.

The file also contains a separate 8-pair **free-text probe** (not in the 213 and
not in any figure above) that puts the decision back where similarity has to
make it. It scored 3 of 8: all four reworded repeats were missed, because what
survives the gate is two or three words, and `"refundable fare"` against
`"non refundable fare"` scored 0.9346 and would be suppressed. The baseline's
failure mode survives the fix; it has moved into the text the gate does not
cover.

## Why the undisturbed duplicate audit is weak evidence

On a cluster nobody disturbs, a duplicate has no way to occur, so a count of zero
proves nothing. The meaningful run is `duplicates-under-failure.txt`, in which
node-b was killed 8 seconds in and node-c 12 seconds later, before both were
restarted.

## What the failure run actually showed

- **No accepted work was lost.** Exactly 878 executions exist for that batch, all
  COMPLETED — none stuck RUNNING, none FAILED.
- **No step ran twice.** Zero duplicated (shard, workflow, run, k) across 3,512
  activity completions.
- **122 requests were refused, not lost.** They received `409 NotOwner, shard
  currently unowned` and the client gave up after 6 attempts. With two owners
  dead, their shards had no owner, and orphan adoption deliberately waits two
  lease TTLs so that a brief pause does not cause shards to change hands.

That delay is a trade-off with a measured cost: the shard is unavailable for new
work for roughly 30 seconds, against NFR-4's target of at most one TTL (10 s).
Shortening it to one TTL would meet NFR-4 at the price of more churn when a node
merely stalls.

## What the standby-promotion drill showed

`make promote-drill` kills the leader with SIGKILL 15 seconds into a 4-shard,
32-concurrency load, calls `pg_promote()` on the standby, recreates the three
nodes with `DATABASE_URL` pointed at it, starts a new workflow, reads a
pre-failure workflow back, and then compares against the dead leader's own data
directory (started again afterwards, read only) to count what was lost.

| | Run 1 (not kept) | Run 2 | Run 3 |
|---|---|---|---|
| `pg_promote()` call | 0.13 s | 0.14 s | 0.13 s |
| kill to promoted and writable | 0.87 s | 0.65 s | 0.82 s |
| kill to first new workflow accepted | 8.90 s | 15.39 s | 8.87 s |
| kill to that workflow COMPLETED | 13.09 s | 19.51 s | 14.02 s |
| committed events missing on promoted | 0 | **2** | **1** |
| workflows missing on promoted | 0 | 0 | 0 |
| workflows with a shortened history | not measured correctly | 1 | 1 |
| duplicate-event assertion on promoted DB | clean | clean | clean |

- **Data was lost, a little.** Replication is asynchronous (`sync_state = async`),
  so a crash loses whatever WAL had not yet reached the standby. Here that was
  1 to 2 events out of about 3,000 per run, belonging to one workflow whose
  history on the promoted database stops a step short. No workflow disappeared
  entirely. This is the cost of async replication, not a bug in the drill;
  zero loss would need `synchronous_commit` with a synchronous standby, which
  costs write latency and blocks writes while the standby is down. Run 1 happened
  to lose nothing, which shows loss is a matter of timing, not a guarantee.
- **The "WAL the standby never received" byte figure overstates the loss.** It
  includes records the old leader wrote itself while recovering after the kill.
  Read the event counts, which are compared row for row.
- **Failover is manual.** Nothing detects the dead leader or promotes. The drill
  scripts three steps a person would have to do: kill and stop restarts (the
  failure itself), `pg_promote()`, and recreating the nodes against the new
  address (the engine has no way to switch databases without a restart).
- **Run 2's 15 s to first accepted workflow is slower than runs 1 and 3 (about
  9 s)**: it spent longer in the node restart. Treat the figure as roughly 9 to
  15 s of unavailability, dominated by container recreation, not by promotion.
- **Run 1 is not kept.** Its terminal output was overwritten when the script's
  loss counter was fixed: it compared per-workflow positions after the nodes had
  already resumed work, so it reported 26 shortened histories that were just
  in-flight work moving on. The headline numbers above are from the terminal
  output of that run; its event and workflow counts were taken before the nodes
  restarted and were not affected.
- **The workflow data from each drill is discarded** by the restore step, which
  re-clones the standby from the old leader. The old leader keeps its own copy
  of everything it committed, including the events the promoted standby lacked.

## The two liveness defects, and why the numbers here did not move

Two defects were found by reading the source, not by any test failing, and both
were liveness only — nothing was corrupted and no side effect ever happened
twice:

1. A lost race for an event sequence number was reported as lost shard
   ownership, and a worker that is told it lost the shard exits for good. Four
   such races retired all four of a shard's workflow workers, and the pool
   would not replace them, so that shard stopped making progress while the node
   went on reporting healthy and holding the lease.
2. A task claim lasts `TASK_LOCK` (30 s) and nothing renewed it. An activity
   that ran longer — including its in-process retries — had its claim lapse and
   a second worker ran it concurrently, on a healthy node. `Queue.Extend`
   existed for exactly this and nothing outside the tests called it.

Both are fixed, with tests that fail against the old code
(`liveness-fix-demonstrations.txt`). The safety numbers here are unchanged, as
they should be: neither defect could produce a duplicate side effect, because
the losing writer's transaction rolls back and the dedup key covers the
activity. The throughput numbers are unchanged too — see
`LIVENESS_FIX_BEFORE_AFTER.md` for why that answers the "the published figures
may have been understated" worry rather than dodging it.

Neither defect was observed firing on the live cluster during these re-runs,
across about 8,900 workflow runs and two node kills: `seq_race`,
`task_lock_lost` and the "replacing exited shard workers" log line all stayed
at zero. That is expected rather than reassuring, and
`liveness-fix-demonstrations.txt` says why.

Two further defects turned up while doing the re-runs, and they are worth more
than the ones they were found alongside, because they are the fourth and fifth
time this project has caught a signal lying about a broken cluster. Running the
test suite the way the Makefile tells you to deleted four shards' lease rows
from the live database and left them deleted, which stopped those shards
permanently; and `/healthz` answered `owned_shards=16` on all three nodes
throughout, because it was reporting the cluster's shard count under the name
of the owned set. Both are fixed and written up in
`green-but-wrong-findings.txt`.

## Phase 9 — AWS deployment preparation (no cloud results)

`docs/cloud-testing/10-PHASE9-AWS-PREP.md` is the report. It has **no measured
cloud numbers, deliberately**: nothing was deployed, so every Tier 1 and Tier 2
row in `docs/cloud-deployment/ACCEPTANCE-MATRIX.md` reads NOT RUN rather than
PASS.

What it does contain that is real and measured locally: `terraform validate`
Success and a `plan` audited to **18 resources with exactly one ingress rule**
(tcp/22 from one `/32`, no `0.0.0.0/0`); both container images built for
`linux/amd64`; the external-effect simulator re-verified honouring an
idempotency key in a local container; `gofmt` and `go vet` clean; and
`make test-integration` at **1220 passed, 0 failed, 0 skipped, 2 blocked**.

Three findings in it are worth reading even if the deployment never happens.

**The external-effect simulator had no Dockerfile.** `durexec-extsimd:phase7`,
the image behind every Phase 7 and Phase 8 external-effect result, was built by
hand and never committed. The committed `Dockerfile.extsimd` now produces
**8.05 MB** from the same source where that image was 27.8 MB. Behaviour should
be identical, and the local re-verification is consistent with that, but the
exact artifact those phases ran is gone. The evidence stands; its
reproducibility did not, and now does.

**One of the two BLOCKED tests is no longer blocked by missing plumbing.**
`TestAcceptNoDuplicateExternalEffectBlocked` waits on "the external-effect
simulator ... and a crash between the effect and its journal write". Phase 7
built the first and Phase 8's `BeforeResultJournal` is the second. The suite
still reports a limitation the engine no longer has. It was **not** implemented
in Phase 9, because adding capability to turn an acceptance matrix green is the
opposite of measuring one.

**Two of my own checks were wrong in ways that would have looked like cloud
failures.** The smoke script queried `tasks.completed_at`, which does not exist
— a finished task is deleted, so any remaining row is work owed — and it
treated `/healthz`'s `owned_shards` as a list when it is an integer count.
Both were caught by reading the engine's code rather than trusting the shape I
expected, and both are the same class of defect as the five "green but wrong"
findings already recorded above.
