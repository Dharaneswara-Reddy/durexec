# Measured results

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
