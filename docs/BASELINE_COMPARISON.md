# Baseline Comparison — Published Numbers Against Our Measured Results

**Purpose.** Every measured result in `docs/results/` is compared here against the best
*real, published* number that could be found and verified against the paper itself.

**Three rules were followed, without exception.**

1. **Every citation is a paper published 2020–2026.** Nothing older is cited, however
   canonical. Where a pre-2020 idea is relevant it is discussed in prose only.
2. **Every number quoted below was read out of the paper's own PDF or publisher page.**
   Nothing is from memory, and nothing is from a secondary summary. Where a paper could
   not be opened, the entry says **could not verify** and gives no number.
3. **Each section ends with a blunt verdict, not a flattering table.** A three-node Docker
   Compose cluster on one laptop is not a datacentre benchmark. Saying so is worth more at
   a review than a comparison that collapses under one question.

---

## New references to add to the bibliography

Formatted in the same IEEE style as the project's existing list. Ready to paste.
Peer-review status is stated for each, because two of these are preprints and an examiner
will ask.

```
[9]  Schroeder et al., "vCache: Verified semantic prompt caching",
     ICLR 2026.                                                        [peer-reviewed]

[10] Weller et al., "NevIR: Negation in neural information retrieval",
     EACL 2024, pp. 2274-2287.                                         [peer-reviewed]

[11] Nagpal, "A reproducible benchmark harness for comparing workflow
     orchestration engines: Temporal, Conductor, and Apache Airflow",
     SoftwareX 35, 102998, 2026.                                       [peer-reviewed,
                                                                        numbers unverified]

[12] Gill et al., "Advancing semantic caching for LLMs with domain-specific
     embeddings and synthetic data", arXiv:2504.02268, 2025.           [preprint]

[13] Li, "Where does exactly-once live? Model, harness, and tool-contract
     effects on duplicate side effects in LLM agents",
     arXiv:2609.29095, 2026.                                           [preprint]

[14] Mansoor et al., "Verified tool calls improve LLM agent reliability
     under non-atomic failures", arXiv:2608.02645, 2026.               [preprint]

[15] Wang, "Callability is not operability: Controlled interface
     interventions for LLM agents", arXiv:2608.23628, 2026.            [preprint]

[16] Wang et al., "Category-aware semantic caching for heterogeneous
     LLM workloads", arXiv:2510.26835, 2025.                           [preprint]
```

### Two corrections to the existing bibliography

Both of these would be noticed by an examiner who knows the papers.

**Reference [8] has the wrong first author.** The CIDR 2026 paper *"Consistency and
Correctness in Data-Oriented Workflow Systems"* is by **Michael Stonebraker** (MIT),
Xinjing Zhou (MIT), Peter Kraft (DBOS, Inc.) and Qian Li (DBOS, Inc.) — verified from the
paper's own title page at `vldb.org/cidrdb/papers/2026/p9-stonebraker.pdf`. It should read:

```
[8] Stonebraker et al., "Consistency and correctness in data-oriented
    workflow systems", CIDR 2026.
```

**Reference [1]'s year is defensible but contested in the paper itself.** The AMBROSIA PDF
self-cites as *"PVLDB, 13(5): 588-601, 2019"* on its own first page, while the VLDB
Endowment volume header and the ACM DOI (10.14778/3377369.3377370) both place it in
**January 2020**. Keeping 2020 is correct and keeps the paper inside the 2020–2026 window;
just be ready to say why if challenged.

---

# 1. Throughput and horizontal scaling

### (a) What we measured

| Shards | Completed wf/s @ c=128 | Median end-to-end latency |
|---|---|---|
| 1 | 12.80 | 9.80 s |
| 2 | 23.15 | 5.46 s |
| 4 | 46.30 | 2.02 s |

Scaling: **1.81× then 2.00×** per doubling. Measured on a 3-node Docker Compose cluster on
a single laptop, concurrency 128, 20-second windows, 0 failures. Roughly four activities
per workflow (3,512 activity completions over 878 runs in the failure run).
Source: `docs/results/scaling-saturated.txt`.

### (b) The published baseline

**Stonebraker et al., CIDR 2026 [8]** is the closest published number in existence: a
Postgres-only durable execution engine running a four-step workflow, reporting goodput in
completed-workflows-per-second. All figures below are quoted from Tables 1–3 of the paper.

**Table 1 — uniform workload**

| Workers | Approach | Goodput | P90 latency | Abort rate |
|---|---|---|---|---|
| 1 | Saga | 24.0 orders/sec | 51 ms | 0% |
| 1 | Physical backout | 43.7 orders/sec | 30 ms | 0% |
| 10 | Saga | 163.4 orders/sec | 58 ms | 0% |
| 10 | Physical backout | 208.6 orders/sec | 55 ms | 20.6% |
| 50 | Saga | 319.0 orders/sec | 110 ms | 0% |
| 50 | Physical backout | 397.9 orders/sec | 196 ms | 48.8% |

**Table 2 — hot-item contention, 50 workers**

| Skew | Approach | Goodput | P90 | Abort rate |
|---|---|---|---|---|
| Zipf θ=0.5 | Saga | 196.0 orders/sec | 181 ms | 0% |
| Zipf θ=0.5 | Physical backout | 43.4 orders/sec | 1,107 ms | 35.7% |
| Zipf θ=0.9 | Saga | 67.1 orders/sec | 245 ms | 0% |
| Zipf θ=0.9 | Physical backout | 24.8 orders/sec | 1,724 ms | 45.7% |

**Table 3 — 1-second delay injected into the payment step, 20 workers, Zipf θ=0.9**

| Approach | Goodput | P90 | Abort rate |
|---|---|---|---|
| Saga | 15.9 orders/sec | 1,100 ms | 0% |
| Physical backout | 2.0 orders/sec | 24,554 ms | 42.5% |

The paper's own summary: *"Under low contention, transactional workflows with physical
backout outperform sagas by up to 1.8x because they require only one commit versus four
commits for saga steps."*

### (c) The baseline's setup

- PostgreSQL 14, with an extended DBOS implementation written in Python.
- Workload: an e-commerce checkout workflow with four steps — reserve inventory, check
  credit, process payment, fulfil order.
- Data: 1,000 bicycle models; 10,000 customers with normally distributed credit balances
  (mean $5,000, standard deviation $1,000); 10,000 units of initial inventory per model.
  Each order buys one bicycle for $1,000.
- SERIALIZABLE isolation; each saga step in its own SERIALIZABLE transaction; automatic
  retry with exponential backoff, up to 20 retries capped at 50 ms.
- **Hardware is not disclosed anywhere in the paper.** There is no machine, no core count,
  no memory figure, no cloud instance type. This is the single most important fact about
  this baseline.

### (d) Comparability verdict — plain language

**This is a rough order-of-magnitude sanity check, not a fair comparison.** It is the
closest thing that exists, and it is still not close.

What makes it usable:
- Same substrate class — one PostgreSQL database doing durability, queueing and
  coordination, with no separate broker or coordination service.
- Same workflow shape — four steps, which is what our workflows have.
- Same unit — completed workflows per second, counting only successes.

What makes it unusable as a head-to-head:
- **Their hardware is secret.** Without it, "46.3 versus 319.0" is a meaningless ratio. It
  could be a laptop or a 96-core server; the paper does not say.
- **The scaling axes are different things.** They vary worker count (1, 10, 50) against one
  database, which measures vertical scaling. We vary shard count (1, 2, 4) across three
  nodes, which measures horizontal partitioning. Those are not the same experiment.
- Our latency includes HTTP admission and up to one 100 ms client poll interval; theirs is
  measured inside the system.
- Theirs is Python, ours is Go. Theirs reports abort rates up to 48.8%; ours reports zero
  failures, because the engines retry differently.

**What would make it comparable:** implement the same four-step e-commerce workload in
durexec, run it at worker counts 1, 10 and 50 on a machine whose specification is stated,
and report goodput, P90 latency and abort rate in the same three tables. That is a few days
of work and would turn this section from a sanity check into a real comparison.

### A second baseline, for the scaling *shape*

**Burckhardt et al., PVLDB 15(8), 2022 [3]** is the right paper for the scaling claim, and
it contains a fact worth knowing before anyone asks: **Netherite publishes no absolute
throughput number at all.** Figure 9's caption states the throughput is *"normalized to the
throughput of the original implementation on 1x1 (a single node and core)."* Its reported
gains over the production Durable Functions engine are ratios:

> *"Netherite improves the throughput by up to x12.2 for Hello5, x7.8 for Bank Application,
> x18.6 for Word Count, and x2.35 for Collision Search applications."*

Its setup: Azure Elastic Premium plans (EP1, EP2 or EP3), a fixed number of hosts (1, 4, 8
or 12) with 1, 2 or 4 cores per host, written NxC. **The partition count is always 12**,
regardless of host count. Throughput is computed by dividing benchmark size by total time.

**Why this matters to us, positively.** Netherite is a PVLDB paper from Microsoft Research
that reports scaling as a ratio and never as an absolute rate, because the absolute rate
depends on hardware nobody else has. Our 1.81× and 2.00× figures are the same kind of
number, reported the same way. That is a precedent worth citing.

**Why it is not a baseline.** Netherite holds partitions constant at 12 and adds hosts; we
hold hosts constant at 3 and add shards. And because there is no absolute figure, there is
nothing to put next to 46.30 wf/s.

**AMBROSIA [1]** cannot be used here either: its throughput is reported in **MB/s** against
message payload size (Figure 4), not in workflows or requests per second.

**The one peer-reviewed harness that does measure Temporal:** Nagpal, SoftwareX 35, 102998,
2026 [11] — *"A reproducible benchmark harness for comparing workflow orchestration engines:
Temporal, Conductor, and Apache Airflow"*. Existence, author, journal, volume, page and DOI
(10.1016/j.softx.2026.102998) were all verified through Crossref. **Its numbers could not be
verified** — the Elsevier full text returned HTTP 403 and the API was rate-limited. Cite the
paper as the right place to look; do not quote a number from it.

### (e) One sentence to say out loud

> "The only published Postgres-only durable execution engine that reports goodput on a
> four-step workflow is the CIDR 2026 DBOS paper, which gets 319 to 398 workflows a second
> at 50 workers — but it never says what hardware that was on, so I treat it as an
> order-of-magnitude check rather than a comparison, and I report my scaling as a ratio for
> the same reason Netherite does."

---

# 2. Fault tolerance and duplicate suppression

### (a) What we measured

1,000 workflows driven while **2 of 3 nodes were SIGKILLed** mid-run (node-b at 8 s, node-c
12 s later), then restarted:

- **878 accepted, all reached COMPLETED.** None stuck RUNNING, none FAILED.
- **0 duplicated `(shard, workflow, run, k)` tuples across 3,512 audited activity
  completions.** `k` is the step's position in the workflow's own program order, so a
  duplicate here is what a repeated side effect looks like in the journal.
- **122 refused at admission** with `409 NotOwner, shard currently unowned` after 6 routing
  attempts — refused, not lost.
- 0 undecodable payloads.

Source: `docs/results/duplicates-under-failure.txt`.

### (b) The published baseline

**Li, arXiv:2609.29095, 2026 [13] — "Where Does Exactly-Once Live?"** This is the strongest
and most directly relevant baseline in the whole document, and it is a 2026 Microsoft paper.
All figures quoted from the abstract and Section 6.

Scale of the study: **25,930 episodes**, nine recent models, three production agent
harnesses, two contract variants, fifteen recovery conditions, against *Limbo* — a
deterministic sandbox of six services with realistic contracts and **twelve fault modes**
injected at the service boundary, including late commits, redelivery and partial batches.
Every episode is graded against a ground-truth ledger of committed effects.

| Condition | Duplicate rate |
|---|---|
| Frontier models told to act exactly once, acknowledgement lost, read-back available | **0.5%** |
| Same models, late commit (request still in flight) | **56%** |
| Same models, redelivery (transport delivered twice) | **74%** |
| Non-idempotent writes, committed-fault episodes | **28%** |
| Writes that accept an idempotency key, committed-fault episodes | **1.5%** |
| Contract offering an idempotency key on every write | **28% → 4%** |
| Explicit, non-ambiguous faults (503, 429, outage, schema drift) | **0.4%** |

Two further findings that bear directly on our design:

> *"agents reported success in 90% of the episodes in which they had duplicated an effect."*

> *"We prove that no verification-only policy is exactly-once under late commits without a
> bound on in-flight time."*

**Supporting peer-adjacent numbers.** Mansoor et al. [14] report an unwrapped agent
duplicating in **20% / 44% / 72%** of runs at low / medium / high injected fault rates,
reduced by a verify-before-retry wrapper to **0% / 16% / 20%**; on a second task, 76%
reduced to the same band, and verify-only alone cut duplicates *"from about 42% to 20%"*.
Wang [15] reports that effect-aware interfaces *"reduce duplicate effects and unsafe commits
by 56.9 and 50.0 percentage points, respectively"*.

### (c) The baseline's setup

Limbo is a software sandbox, not a cluster: six simulated services with Stripe-style
idempotency key semantics, eventually consistent and missing read paths, and twelve fault
modes injected at the boundary. Thirty-one unit tests with scripted agents validate the
fault semantics. The variables are the model, the harness, the tool contract and the
recovery policy; the task, the world and the fault are held fixed. There is no distributed
cluster and no node failure — the faults are injected at the service interface.

### (d) Comparability verdict — plain language

**Not comparable as a measurement. Comparable, and strongly supportive, as an argument.**

The quantities are different. Theirs is "what fraction of episodes contained a duplicate
side effect, when a language model decided whether to retry". Ours is "how many duplicate
`(shard, workflow, run, k)` tuples appear in a journal of 3,512 activity completions, when
two of three nodes were killed". Those two numbers cannot sit in the same column.

But this paper is the best thing that has happened to the project's framing, because its
central conclusion is the project's central premise, measured at scale:

- Exactly-once cannot live in the caller. Frontier models, explicitly instructed, still
  duplicated in 56% and 74% of episodes when verification could not help.
- Exactly-once can live in the substrate. Offering a key on every write took the duplicate
  rate from 28% to 4%.
- **The key they recommend is the key durexec already uses.** Our journal key is the
  workflow's position — shard, workflow, run, step index `k` — which is precisely the
  "derive the key from workflow position" shape this literature converges on, rather than a
  hash of the request body that changes when the caller rephrases its arguments.

So the honest claim is not "we beat 4%". It is: *this paper measures how badly duplicates
happen when no substrate enforces exactly-once, and durexec is such a substrate; our 0 in
3,512 is a measurement of the substrate working, on a much smaller and much easier
experiment.*

**What would make it comparable:** run durexec as the execution substrate behind Limbo (it
is a deterministic sandbox, so this is feasible) and report episode-level duplicate rate on
the same twelve fault modes. That would put our number in their table.

### A second baseline, for the 122 refusals

**Lee et al., SOSP 2021 [2]** has the one published availability number that is in the right
shape. Their experiment:

> A primary-only application with **10,000 shards on 60 servers**, configured to allow up to
> 10% of its containers to be restarted concurrently during a rolling upgrade. *"By
> gracefully handling container restarts and primary-replica migration, the application
> client's request success rate stays at ≈100%. Without graceful primary-replica migration,
> the success rate drops to ≈ 98%. With neither graceful primary-replica migration nor
> graceful handling of container restarts, although the upgrade finishes earlier (800 seconds
> vs. 1,500 seconds), the success rate further drops below 90%."*

Shard Manager's motivating statistic is also worth quoting: at Facebook, *"container stops
due to planned events are ≈1000 times more frequent than unplanned failures"*, and
*"treating planned events as failures amplifies unavailability by ≈1000x as every failover
causes a period of shard unavailability."*

**Our comparable number: 878 of 1,000 succeeded — 87.8%.** That sits just under Shard
Manager's "neither graceful mechanism" case of below 90%, which is exactly the case durexec
is in: there is no graceful drain and no primary-replica migration, so a killed owner's
shards simply have no owner until orphan adoption fires.

**Verdict on this one: a fair order-of-magnitude comparison, with one caveat that cuts in
our favour and one that cuts against.** In our favour: their fault is a *planned* rolling
restart of at most 10% of containers, while ours is SIGKILL of two of three nodes — 67% of
the cluster, unplanned. Getting 87.8% under a far harsher fault than theirs is a reasonable
place to be. Against us: 3 nodes and 16 shards versus 60 servers and 10,000 shards is three
orders of magnitude apart, so this is a sanity check on the shape of the number, not a
measurement of equivalent systems. The documented 30-second ownership gap (two lease TTLs
of 10 s, against NFR-4's target of one) is the mechanism behind our 122 refusals and should
be stated as the trade-off it is, not hidden.

**Netherite [3]** is worth one sentence of prose here: it guarantees exactly-once for
internal state and at-least-once for external effects, implemented with a per-partition
*deduplication vector* recording the last-processed incoming message position from each
remote partition. That is the same idea as our monotonic step index, in a different system.
The paper publishes the guarantee, not a duplicate count, so there is no number to compare.

### (e) One sentence to say out loud

> "The 2026 'Where Does Exactly-Once Live?' study measured 25,930 agent episodes and found
> duplicate rates of 56 to 74 percent when nothing in the substrate enforced exactly-once,
> falling to 4 percent once every write accepted an idempotency key derived from workflow
> position — which is exactly the key my engine uses, and in my own kill test it journalled
> zero duplicates across 3,512 activity completions."

---

# 3. Recovery and replay

### (a) What we measured

- Shard ownership moves to a surviving node **within seconds** of a kill; all 16 shards had
  an owner again 0.02 s after the nodes were healthy in the promotion drill.
- Replay of a crashed workflow's history: **under 1 ms**.
- The documented cost: orphan adoption deliberately waits two lease TTLs, so a shard is
  unavailable for new work for roughly **30 seconds** after its owner dies.

### (b) The published baseline

**Li et al., PVLDB 16(11), 2023 [5] — R3.** The only paper in the project's bibliography
that measures replay cost directly. Quoted from its abstract and Section 7:

> *"R3 always-on recording has a small performance overhead (<25% for point queries but
> <0.1% for complex transactions like in TPC-C) during normal application execution."*

> *"[R3 can] replay recorded traces within 0.33 – 1.6× and retroactively execute them within
> 0.11 – 0.78× of the original request execution time."*

Storage overhead: *"on average 42 – 100 bytes per request."* Parallel-execution
optimisations *"improve performance by up to 7.2× compared to the sequential baseline."*

Absolute replay times for a trace that originally ran for 60 seconds (Figure 4): Moodle at
1M requests replays in **606.5 s**; WordPress at 912K requests in **592.6 s**; TPC-C
*"always [replays] within 60 seconds."*

**Burckhardt-adjacent recovery figure — AMBROSIA [1]** gives the other half:

> *"we conduct an experiment where we perform our throughput test on 20GB of 64K messages,
> with only the initial checkpoint generated. We measure the total execution time of this
> experiment, without failures, to be 77 seconds. Ambrosia is able to recover from the
> initial checkpoint and log from start to finish in 40 seconds."*

### (c) The baselines' setups

**R3:** Google Cloud `c2-standard-8` VM instances — 8 vCPUs, 32 GB DRAM, a SCSI HDD — with
the Postgres server, the Vertica server and the application request handlers each on
separate VMs. 128 parallel JDBC connections per application. SERIALIZABLE (implemented as
snapshot isolation). Synchronous commit disabled during replay. Workloads: TPC-C with 24
warehouses, plus Moodle and WordPress ported to Java.

**AMBROSIA:** two `D14v2` Azure instances (16 cores, unlimited storage bandwidth); logs
written to 6× P10 Azure Premium Managed Disks pooled in software RAID with aggregate
bandwidth of 600 MB/s. Checkpointing disabled except in the resiliency tests.

### (d) Comparability verdict — plain language

**Our sub-millisecond replay number is not comparable to either baseline. Do not put it in a
table next to them.**

The quantities are genuinely different things:

- R3 replays **a whole trace of a multi-user application**, re-running thousands of
  interleaved transactions while reconstructing the original concurrency. Its unit is "replay
  time as a multiple of original execution time", and its workloads run for 60 seconds and
  replay in up to 606 seconds.
- Our figure is the cost of **rehydrating one workflow's own event history in process** — a
  handful of journaled events decoded into memory. It does not re-run anything against a
  database, and there is no concurrency to reconstruct.
- R3 is also a *debugging* tool, not a crash-recovery path. Replaying to find a bug and
  replaying to resume a workflow are different jobs.

Expressed in R3's own unit, a four-step workflow that originally took 2.02 s and rehydrates
in under 1 ms is a ratio below **0.0005×**, which is three orders of magnitude outside R3's
measured 0.33–1.6× band. That is not a win; it is evidence that the two numbers measure
different things, and quoting it as a win would be the fastest way to lose the point.

**AMBROSIA's recovery figure is the closer one in spirit** — 40 s to replay a log for work
that originally took 77 s, a ratio of about 0.52×, which sits inside R3's band and confirms
that "replay costs roughly half to one-and-a-half times the original run" is the honest
order of magnitude for whole-service log replay. Our number is not that quantity.

**What would make it comparable:** measure and report replay as a ratio — the wall-clock
time to bring a crashed workflow back to its last completed step, divided by the wall-clock
time that workflow originally took — and do it for a workflow long enough that the ratio
means something (say, 50 or 500 steps rather than 4). Then the number can sit beside R3's
0.33–1.6× and AMBROSIA's 0.52× honestly.

### A fair comparison for the ownership-transfer time

**Lee et al., SOSP 2021 [2]** publishes the only production shard-placement latency numbers:

> *"As the problem size grows 5x from 75K shards on 1K servers to 375K shards on 5K servers,
> the total solving time grows by 6.8x from 30 seconds to 205 seconds, indicating that the
> allocator is scalable. In our production environment, applications have much fewer
> violations than a random assignment and hence the solving time is much shorter, with P90
> and P99 at ≈10 seconds and ≈50 seconds, respectively."*

Each mini-SM runs on an 18-core, 64 GB RAM machine at P90 CPU ≈30%. Shard Manager as a whole
manages *"nearly 100M shards hosted on over one million servers"*, with the largest single
deployments at *"≈19K servers and ≈2.6M shards"*.

**Verdict: a fair order-of-magnitude comparison and the most favourable honest one in the
document.** Our "ownership moves within seconds" and our 30-second unavailability window sit
inside Facebook's own published production band for shard placement (P90 ≈10 s, P99 ≈50 s).
The caveat is enormous and must be stated: they are placing up to 2.6 million shards across
19,000 servers with a constraint solver, and we are reassigning 16 shards across 3 nodes
with a lease TTL. Being in the same band is a sanity check that our timings are not absurd
— it is not a claim of parity.

### (e) One sentence to say out loud

> "R3 is the only paper that measures replay cost, and it reports replay as 0.33 to 1.6
> times the original execution time for a whole application trace — my sub-millisecond
> figure is a per-workflow history rehydration, a different quantity, so I quote it as what
> it is and point at R3 for the number that would actually be comparable if I measured the
> ratio instead."

---

# 4. Database failover

### (a) What we measured

Across three `make promote-drill` runs — leader SIGKILLed 15 s into a 4-shard,
32-concurrency load, standby promoted with `pg_promote()`, three nodes recreated pointing at
it:

| | Run 1 (not kept) | Run 2 | Run 3 |
|---|---|---|---|
| `pg_promote()` call | 0.13 s | 0.14 s | 0.13 s |
| kill → promoted and writable | 0.87 s | 0.65 s | 0.82 s |
| kill → first new workflow accepted | 8.90 s | 15.39 s | 8.87 s |
| kill → that workflow COMPLETED | 13.09 s | 19.51 s | 14.02 s |
| committed events missing on promoted | 0 | **2** | **1** |
| workflows missing on promoted | 0 | 0 | 0 |

Replication is `sync_state = async`. Failover is **manual** — nothing detects the dead
leader, and the engine cannot switch databases without a restart. The 9–15 s is dominated by
container recreation, not by promotion.

### (b) The published baseline

**Goldstein et al., PVLDB 13(5), 2020 [1].** The only verified, peer-reviewed failover
measurement found in the 2020–2026 window for this class of system:

> *"To test Ambrosia's failover performance, we performed our latency test continuously with
> 3 active instances for the server, where the log is backed by Azure Files. We induced
> periodic failure of the primary server, resulting in failover to an active secondary, and
> restarted the failed server to setup for the next failure. We then found the corresponding
> spikes in ping latency. Since failover time was orders of magnitude higher than ping
> latency, we simply used these measured latency spikes as our failover time measurements.
> The result of this experiment shows that with this setup, Ambrosia fails-over in 1.8
> seconds, on average, with little variation."*

For context on the same hardware, AMBROSIA's ping latencies (Table 1, milliseconds):

| | p50 | p90 | p99 | p999 | Mean |
|---|---|---|---|---|---|
| Ambrosia | 6.57 | 7.1 | 8.71 | 11.34 | 6.63 |
| gRPC | 0.5 | 0.59 | 0.8 | 61.85 | 0.58 |
| Azure Serverless | 31.62 | 130.5 | 324.7 | 6708 | 80.51 |
| Ambrosia-NoLog | 2.15 | 2.49 | 3.05 | 7.59 | 2.32 |

### (c) The baseline's setup

Two `D14v2` Azure instances (16 cores each, unlimited storage bandwidth) for the main
experiments; the failover test specifically used **3 active instances for the server, with
the log backed by Azure Files**. Logs for the throughput tests went to 6× P10 Azure Premium
Managed Disks in software RAID at 600 MB/s aggregate. The same physical instances were
reused across all experiments to eliminate hardware variation.

### (d) Comparability verdict — plain language

**Fair on the quantity, unfair on the mechanism, and the honest reading is unfavourable to
us. This is the comparison most likely to be used against the project, so it should be made
first, by us.**

The quantity is the same: seconds from a kill to service being usable again. On that
quantity:

- AMBROSIA: **1.8 s**, automatic, no data loss.
- durexec: **roughly 9 to 15 s**, manual, 0 to 2 committed events lost.

So we are about **five to eight times slower, and we lose data where they lose none.**

**The dangerous mistake here would be to quote our 0.13 s `pg_promote()` against their
1.8 s.** Those are not the same step. `pg_promote()` is PostgreSQL changing its own internal
state; AMBROSIA's 1.8 s is the end of a visible outage in a continuously running latency
test. Putting 0.13 s next to 1.8 s would read as a 14× win and would be wrong — and an
examiner who has read the AMBROSIA evaluation will know it is wrong. The comparable number
on our side is "kill → first new workflow accepted", which is 8.87 s to 15.39 s.

The mechanisms also differ in a way that explains the gap, which is a legitimate defence but
not an excuse:

- AMBROSIA fails over among **three pre-warmed active instances sharing one log in Azure
  Files**. There is nothing to promote and nothing to restart; a secondary already has the
  log. Zero loss is structural.
- durexec promotes an **asynchronously replicating PostgreSQL standby** and then recreates
  three containers. The container recreation is most of the 9–15 s. The 0–2 lost events are
  the WAL that had not reached the standby at the moment of the kill — the documented cost of
  `async`, not a bug. Run 1 happened to lose nothing, which shows loss is a matter of timing
  rather than a guarantee.

**What would make it comparable:** (1) automate detection and promotion so a human is not in
the path; (2) take container recreation out of the critical path by letting the engine
re-resolve `DATABASE_URL` at runtime instead of needing a restart; (3) re-measure
kill-to-service-restored under a continuous load, the way AMBROSIA measured it — as a spike
in an ongoing latency series rather than as a scripted sequence of steps. With those three
changes the two numbers would be measuring the same thing.

### What is *not* available, stated plainly

**No peer-reviewed PostgreSQL failover measurement from 2020–2026 could be found.** Searches
for controlled evaluations of `pg_promote`, Patroni or equivalent returned only vendor
documentation, tutorials and practitioner blog posts — figures in the range of 30–60 s with
default Patroni settings, ~40 s measured in one author's lab test, under 20 s with aggressive
tuning. **None of this is cited above, and none of it should appear in the report as
research.** It can be mentioned in prose as "practitioner reports suggest", with the source
named as a blog, or left out entirely. Our own three drills are better evidence than any of
it, because at least the method is written down.

### (e) One sentence to say out loud

> "AMBROSIA reports 1.8 seconds to fail over, automatically, between three active instances
> sharing one log — my drill takes nine to fifteen seconds and loses one or two events,
> because promotion is manual, replication is asynchronous, and most of that time is
> recreating containers; the gap is real and the fix is automation plus a runtime-switchable
> database URL, not a different number."

---

# 5. Semantic idempotency — the negative result

### (a) What we measured

`all-MiniLM-L6-v2`, 213 pairs (98 labelled DUPLICATE, 115 labelled DISTINCT of which **91
are hard negatives** — lexically close, differing in one deciding field), labels generated by
construction, 61 threshold arms swept.

At the **shipped operating point** (escalation band [0.85, 0.90)):

| Arm | Recall (detect) | FPR (all distinct) | FPR (hard negatives) | Precision | F1 |
|---|---|---|---|---|---|
| exact | 0.143 | 0.000 | 0.000 | 1.000 | 0.250 |
| semantic [0.85, 0.90) | **0.571** | **0.939** | **1.000** | 0.341 | 0.427 |

Latency: exact 2.5 µs p50 / 8.4 µs p99; semantic 10.4 µs p50 / 45.7 µs p99 (cached vectors).

The double-booking fixture — `"BLR to DEL Friday morning"` versus `"Bangalore to Delhi,
Friday AM"` — scores 0.8695, so it is caught at τ ≤ 0.85 and missed at τ ≥ 0.90.

Reported honestly as a negative result: the mechanism catches four times as many reworded
duplicates as exact matching, and wrongly blocks 93.9% of genuinely distinct actions and
100% of the hard negatives. Sources: `docs/results/semantic-experiment-minilm.txt`, and the
opt-in hybrid run in `semantic-experiment-hybrid-minilm.*` with its own caveats.

### (b) Baseline 1 — MeanCache [7], the number already in our bibliography

**Gill et al., IEEE IPDPS 2025 [7].** Table I, verified from the paper:

| Metric | GPTCache (standalone) | MeanCache (MPNet) | MeanCache (Albert) | GPTCache (contextual) | MeanCache (contextual) |
|---|---|---|---|---|---|
| F score | 0.56 | 0.73 | 0.68 | 0.67 | **0.93** |
| Precision | 0.52 | 0.72 | 0.66 | 0.66 | **0.98** |
| Recall | 0.85 | 0.78 | 0.77 | 0.71 | 0.79 |
| Accuracy | 0.72 | 0.85 | 0.81 | 0.61 | 0.86 |

Confusion matrices (Figure 7), 1,000 queries = 700 unique + 300 duplicate:

| | TN | FP | FN | TP | FPR | Recall |
|---|---|---|---|---|---|---|
| MeanCache | 611 | **89** | 66 | 234 | 12.7% | 0.78 |
| GPTCache | 467 | **233** | 46 | 254 | **33.3%** | 0.85 |

Contextual queries (Figure 9), 100 unique + 150 duplicate:

| | TN | FP | FN | TP | FPR |
|---|---|---|---|---|---|
| MeanCache | 97 | **3** | 31 | 119 | 3% |
| GPTCache | 46 | **54** | 43 | 107 | **54%** |

Threshold findings: GPTCache's configuration is Albert at cosine threshold **0.7**;
MeanCache's learned optimum is **0.83 for MPNet** (F1 0.89, precision 0.92, accuracy 0.90)
and **0.78 for Albert** (F1 0.88). The paper's summary: *"GPTCache's suggested threshold of
0.7 will result in suboptimal performance during semantic matching. The optimal threshold τ
values varies with the embedding model."* Llama 2 embeddings reach a maximum F1 of only
**0.75**.

Most important for us, MeanCache weights precision deliberately:

> *"To prioritize precision, we adjust the beta value in the F score to 0.5, valuing
> precision twice as highly as recall to ensure user satisfaction by avoiding false
> positives... Thus, precision in semantic caching is more important than recall."*

**Setup:** a high-performance computing cluster with 128 cores, 504 GB of memory and four
NVIDIA A100 GPUs (80 GB each); the Flower federated-learning library with 20 clients; the
SBERT library for training; the GPTCache dataset split into train/validation/test, plus a
synthetic 450-query contextual dataset generated with GPT-4.

**Comparability verdict.** **A rough sanity check on precision and false-positive rate, not
a fair head-to-head.** Our 0.571 recall and 0.341 precision are worse than GPTCache's
0.85/0.52 and much worse than MeanCache's 0.78/0.72 — but the corpora are not of equal
difficulty, and saying so is not special pleading, it is the measurement:

- 91 of our 115 distinct pairs are **deliberate hard negatives**, built so that two intents
  differ in exactly one deciding field. GPTCache's 700 unique queries are ordinary distinct
  queries from a general dataset.
- The closest published number in difficulty is **GPTCache on contextual queries: 54 false
  hits in 100 non-duplicates, an FPR of 54%** — queries that are near-identical in wording
  and differ only in what came before them. Our hard-negative FPR of 100% is roughly twice
  that, on a corpus constructed to be harder still.
- The consequence differs too, and this is the project's real point. A false hit in a
  semantic cache returns a wrong answer and the user retries. A false hit in semantic
  idempotency **refuses to book a flight the user actually wanted**. MeanCache weighting
  precision at 2× recall is the same conclusion arrived at independently in a less dangerous
  setting.
- Our labels are generated by construction and have never been reviewed by a person;
  MeanCache's test split is a held-out portion of a real dataset. This alone disqualifies a
  head-to-head.

**What would make it comparable:** run the same threshold sweep on MeanCache's released
GPTCache-derived test split and its 450-query contextual set, with the same embedding model,
and report FPR at a matched recall. MeanCache's data and code are released, so this is a
realistic piece of future work and the right thing to promise if asked.

### (c) Baseline 2 — vCache [9], the independently published negative finding

**This is the answer to "has anybody published the same negative result?" — and the answer
is yes, at ICLR 2026, for the general structural claim.**

**Schroeder et al., "vCache: Verified Semantic Prompt Caching", ICLR 2026** (UC Berkeley,
TU Munich, ETH Zurich, Stanford). Appendix F.9, quoted exactly:

> *"To analyze the relationship between similarity scores s(x) and cache correctness c(x), we
> conduct an experiment on the 45,000 entries of the SemCacheClassification benchmark using
> an error tolerance of δ = 0.02 (2%)... The result shows that correct and incorrect
> observations are nearly indistinguishable in similarity space, with substantial overlap and
> similar means (0.84 vs. 0.85). This illustrates that one single similarity threshold is not
> a reliable decision boundary."*

> *"The thresholds span a range, from 0.71 to 1.0, indicating that no single similarity
> threshold can suffice across embeddings."*

And its measured baseline comparison (Table 3; E5-large-v2 embeddings, GPT-4o-mini,
SemCacheLmArena benchmark):

| Baseline | Threshold / δ | Error rate | Cache hit rate |
|---|---|---|---|
| GPTCache, static threshold | 0.99 | 2.5% | 37% |
| GPTCache, static threshold | 0.98 | 4.1% | 53% |
| GPTCache, static threshold | 0.97 | 5.2% | 67% |
| vCache, learned per-embedding | δ = 0.02 | **0.5%** | 41% |
| vCache | δ = 0.03 | 1.1% | 46% |
| vCache | δ = 0.05 | 2.0% | 54% |

Headline claim: *"reduces error rates by up to 26×"*.

**Setup:** three embedding models (GteLargeENv1-5, E5-large-v2, OpenAI
text-embedding-3-small), two LLMs (Llama-3-8B-Instruct, GPT-4o-mini), and four released
benchmarks derived from five real-world datasets. The threshold-dilemma experiment uses
45,000 entries of SemCacheClassification.

**Comparability verdict.** **This is the strongest comparison in the entire document, and it
is a corroboration rather than a contest.** vCache does not measure semantic idempotency, so
the numbers do not line up — but it measures *why* our result came out the way it did, on a
corpus 211 times larger, and reaches the same conclusion:

> A single global cosine threshold cannot separate correct from incorrect matches, because
> the similarity distributions of correct and incorrect matches overlap almost entirely.

The specific figure is striking. vCache finds the means of the correct and incorrect
distributions at **0.84 and 0.85**. The band durexec ships is **[0.85, 0.90)**. Our
escalation band sits exactly where an independent team, on a different task with a different
embedding model and 45,000 samples, measured that the distributions are indistinguishable.
That is not a coincidence; it is the same phenomenon, and it makes the project's negative
result reproducible rather than anecdotal.

It also validates the project's choice to *sweep* 61 thresholds and refuse to pick one by F1
on the test set. vCache's conclusion is that no single threshold works, which is precisely
why a threshold chosen on this corpus would be selection on the test set — a point the
results README already makes.

### (d) Baseline 3 — NevIR [10], the closest published analogue of "one deciding token"

**Weller et al., EACL 2024, pp. 2274–2287.** Documents that differ *only* by a negation;
models must rank the right one for each of two contrastive queries. Table 2, pairwise
accuracy (random = 25%):

| Type | Model | Score |
|---|---|---|
| Random | — | 25% |
| Sparse | TF-IDF | 2.0% |
| Sparse | SPLADEv2 ensemble-distill | 8.0% |
| Late interaction | ColBERTv2 | 13.0% |
| Late interaction | ColBERTv1 | 19.7% |
| Bi-encoder | DPR | 6.8% |
| Bi-encoder | coCondenser | 7.7% |
| Bi-encoder | **all-mpnet-base-v2** | **8.1%** |
| Bi-encoder | multi-qa-mpnet-base-dot-v1 | 11.1% |
| Cross-encoder | MonoT5 base | 34.9% |
| Cross-encoder | MonoT5 3B | 50.6% |

The paper's own summary: *"No bi-encoder architecture scores higher than 12% paired accuracy
despite the method of pre-training... with most models performing in the 5-10% range"* and
*"nearly all models perform worse than randomly ranking."*

**Setup:** 1,383 test pairs (948 train, 225 dev), built from CondaQA paragraphs with queries
written by 28 Amazon Mechanical Turk annotators paid ~$15/hour, average 91 query pairs each.
Questions average 11 words, documents 113 words; the two items in a pair are nearly the same
length. Human check: on 10 randomly sampled test instances, *"all human annotators ranked
all queries"* correctly.

**Comparability verdict.** **Not a numeric baseline — the task and the metric are different —
but the single best published evidence for the structural claim, and it should be cited for
exactly that.**

The structure matches ours precisely. Two texts, nearly identical in wording, differing in
one token that decides the meaning. In NevIR that token is a negation; in our corpus it is a
date, a route, a daypart or a passenger count. The model family matches too:
`all-MiniLM-L6-v2` is a sentence-transformers bi-encoder, the same architecture class as
`all-mpnet-base-v2`, which scores **8.1% against a 25% random baseline** on NevIR while
humans score 100%.

So the claim to make is not "nobody has found this before". It is: *this failure mode is a
documented property of bi-encoder sentence embeddings, measured at EACL 2024 on negation;
our result is the first measurement of what it costs when the deciding token is a date and
the consequence is a duplicated booking rather than a wrong search result.*

NevIR also offers the escape route the hybrid comparator is reaching for, and names its
price: cross-encoders do substantially better (MonoT5 3B at 50.6%) but are *"the slowest and
most expensive category of retrieval models"*, and even they *"perform significantly below
humans"*. That is a published reason not to expect a bigger model to fix this.

### (e) Baseline 4 — domain fine-tuning, which supports the hybrid diagnosis

**Gill et al., arXiv:2504.02268, 2025 [12]** (Redis and Virginia Tech — note the shared
first author with MeanCache). **Preprint, not peer-reviewed; flag it as such.**

> *"A key shortcoming of baseline (out-of-the-box) embedding models is that they are
> typically trained on broad, general-purpose corpora. When faced with domain-specific
> queries (e.g., finance, medical, or legal), these models may struggle to capture the
> nuanced relationships unique to that domain."*

Measured effect of one epoch of domain fine-tuning on ModernBERT (~149M parameters):

| Dataset | Metric | Out of the box | Fine-tuned |
|---|---|---|---|
| Quora duplicate questions | Precision | **64%** | **84%** |
| Quora duplicate questions | Average precision | 76% | 92% |
| Medical | Precision | 78% | 92% |
| Medical | Average precision | 92% | 97% |

Table 1, on the real-world medical dataset, puts the ceiling in context: OpenAI
text-embedding-3-large at precision 0.85 / recall 0.87; Linq-Embed-Mistral at 0.84 / 0.93;
the fine-tuned LangCache-Embed-Synthetic at 0.87 / 0.90.

**Setup:** SBERT library, online contrastive loss, one epoch, learning rate 6.54×10⁻⁵, batch
size 16, Adam, gradient norm 0.5 — deliberately conservative to avoid catastrophic
forgetting, which they measure (22% precision gain on Quora came with a precision drop on
medical test data).

**Comparability verdict.** **A fair comparison of the *right* quantity on a *different*
corpus — use it to justify the diagnosis, not to score the engine.** The number that matters
is **64% precision out of the box on Quora duplicate-question detection**, rising to 84%
after domain adaptation. That establishes, with a measurement, the general claim the project
needs: a general-purpose sentence embedding used off the shelf is not precise enough for
duplicate detection, and the published fix is domain adaptation of the embedding, not
tuning the threshold. Our hybrid comparator takes the other published route — pull the
deciding fields out and compare them exactly — and the results README is already honest that
this made the model irrelevant on this corpus rather than making the embedding better.

**One number deliberately not used.** Wang et al., arXiv:2510.26835, 2025 [16] states *"At
threshold 0.80, dense code embeddings produce 15% false matches between semantically
different queries. Threshold 0.90 reduces false matches to 3%."* This would be an attractive
false-positive-rate baseline, but it appears in the paper's design-motivation section and the
paper's own abstract describes parts of its results as *"theoretical projections"*. **It could
not be verified as a measurement, so it is not quoted as one.** The paper is worth citing for
its qualitative point — *"fixed thresholds cause false positives in dense spaces and miss
valid paraphrases in sparse spaces"* — which is the same finding as vCache's, independently
arrived at.

### (f) Has anyone published the same negative finding? — the careful answer

This needs to be said in three parts, because the three parts have different answers.

**Part 1 — "a single global cosine threshold cannot separate duplicates from distinct
items." PUBLISHED, INDEPENDENTLY, AND IT STRENGTHENS THE PROJECT.** vCache (ICLR 2026)
measured it on 45,000 entries and found the correct and incorrect similarity distributions
*"nearly indistinguishable... with substantial overlap and similar means (0.84 vs. 0.85)"*,
with per-embedding optimal thresholds spanning 0.71 to 1.0. Category-aware caching [16]
reaches the same conclusion from a different direction. Our finding is reproducible, not
anecdotal, and the band we ship sits exactly where vCache measured the overlap.

**Part 2 — "general-purpose bi-encoder sentence embeddings cannot resolve a pair that
differs in one deciding token." PUBLISHED FOR NEGATION, NOT FOUND FOR DATES OR NUMBERS.**
NevIR (EACL 2024) is the measurement: bi-encoders never exceed 12% pairwise accuracy against
a 25% random baseline when documents differ only by a negation, with `all-mpnet-base-v2` at
8.1% and humans at 100%. No paper could be found that performs the same minimal-pair
experiment with a **date** or a **numeric field** as the deciding token. Searches for numeric
and temporal perturbation benchmarks in sentence embeddings returned nothing in the
2020–2026 window that measures this directly.

**Part 3 — "embedding similarity applied to suppress duplicate *side-effecting actions*, with
measured precision, recall and hard negatives." NOT FOUND IN ANY PEER-REVIEWED VENUE. THIS
IS THE NOVELTY, AND IT SHOULD BE CLAIMED NARROWLY.** Embedding-based deduplication is
published for training data (NVIDIA NeMo Curator's SemDedup) and for LLM response caching
(GPTCache, MeanCache, vCache, and the category-aware work). Idempotency for agent tool calls
is published, and converges on **exact** keys — Li [13] measures 28% → 4% from idempotency
keys, and the recommended key shape is the workflow position (run ID plus step index), which
is what durexec uses. The term "semantic idempotency" appears only in vendor blog posts, with
no formal method and no evaluation. **No peer-reviewed paper was found that applies embedding
similarity to decide whether a side-effecting action is a repeat, and measures it against
hard negatives.**

**The claim to make, in exactly these words:** *the mechanism and its measurement appear to
be novel; the failure mode is not.* Three independent 2024–2026 papers — vCache, NevIR and
the domain-adaptation work — predicted this result before we measured it, and the fact that
they did is what makes a negative result worth reporting rather than an embarrassment. The
project's contribution is having measured the cost in a setting where a false positive
cancels a real action, and having said so.

### (g) One sentence to say out loud

> "vCache at ICLR 2026 measured 45,000 cache entries and found the similarity distributions
> of correct and incorrect matches overlapping with means of 0.84 and 0.85, and NevIR at
> EACL 2024 found bi-encoders scoring 8 percent against a 25 percent random baseline when
> documents differ by one negation — so my 94 percent false-positive rate at a 0.90 threshold
> is the expected result, independently corroborated, and what is new is measuring it where a
> false positive cancels a booking rather than returning a stale answer."

---

# 6. The "one component versus four to six" claim

### (a) What we claim

durexec runs on PostgreSQL alone. Comparable systems need four to six components — a
workflow service, a queue or broker, a coordination service, a database and a cache.

### (b) What is published

**No peer-reviewed number exists for this claim. Say so plainly rather than dressing a
vendor blog up as research.**

Three searches were run for peer-reviewed measurements of operational complexity, component
count or deployment footprint in workflow orchestration and microservice systems from
2020–2026. What exists:

- **The closest peer-reviewed metric work** counts services and inter-service connections
  from **static analysis of source code**, not deployed components: *"Assessing Evolution of
  Microservices Using Static Analysis"*, Applied Sciences, 2024 (doi 10.3390/app142210725).
  It is not a deployment-footprint metric and should not be cited as one.
- **A literature overview of microservice granularity** (PMC8444086) finds that almost all
  surveyed metrics concern individual services, with none standardising a deployment-component
  count.
- **No standardised deployment-footprint metric was found at all** — nothing counting
  containers, pods, Kubernetes objects or infrastructure dependencies as a comparative measure
  across orchestration engines.
- **Nagpal, SoftwareX 35, 102998, 2026 [11]** is the only peer-reviewed artefact that
  actually deploys Temporal, Conductor and Airflow side by side with engine-specific adapters
  and parameterised workloads, and it reports attempted and successful throughput separately.
  **Its numbers could not be verified** (publisher returned 403; the API rate-limited), and it
  describes a benchmark harness rather than a component-count comparison.

Everything else available is vendor or practitioner material: Temporal's own architecture
documentation, Orkes' "a billion workflows a month" post, Windmill's competitor benchmarks
(a vendor benchmarking its own competitors), and various comparison blogs. The project's
`Traditional_Architectures_Evidence.md` already handles these correctly, by citing primary
documentation and flagging vendor interest — that is the right treatment and it should not be
upgraded to look like research.

### (c) The one citation that *does* carry weight, and it is primary not peer-reviewed

The Apache Airflow scheduler documentation states the architectural position explicitly:

> *"…by not using direct communication or consensus algorithm between schedulers (Raft, Paxos,
> etc.) nor another consensus tool (Apache Zookeeper, or Consul for instance) we have kept the
> 'operational surface area' to a minimum."*

This is stable-release documentation of the most widely deployed orchestrator in the world,
saying that the database-as-coordinator choice was deliberate and justified on operational
grounds. It is primary evidence that the comparison axis is real. It is **not** a measurement,
and it should never be presented as one.

### (d) Comparability verdict — plain language

**Not comparable, because there is nothing published to compare against.** The component
count is a **description of an architecture**, verifiable by reading each system's deployment
documentation, and it should be presented that way: as a table of what each system requires,
each cell cited to that system's own docs, with no claim that the count is a measured
quantity or that fewer components is measurably better. Any attempt to turn "one versus four
to six" into a performance or reliability claim will not survive the first question, because
no study has measured the relationship.

**What would make it comparable:** define the metric before measuring it — for example,
"number of distinct long-running processes that must be running, monitored and upgraded for
the system to accept work" — then count it for durexec, Temporal, Conductor and Airflow from
each project's own recommended production deployment, and publish the counting rule so
somebody else can check it. That is a defensible contribution. An uncounted assertion is not.

### (e) One sentence to say out loud

> "There is no peer-reviewed measurement of component count or operational footprint for
> workflow engines — I checked — so I present it as an architectural description with each
> system's own deployment documentation as the citation, and I do not claim that fewer
> components is measurably faster or more reliable, because nobody has measured that."

---

# Summary table — every comparison, with its verdict

| Our result | Best published baseline | Verdict |
|---|---|---|
| 46.30 wf/s at 4 shards; 1.81× / 2.00× scaling | Stonebraker et al., CIDR 2026 [8]: 319.0–397.9 orders/sec at 50 workers, 4-step workflow on PostgreSQL 14 | **Order-of-magnitude sanity check.** Same substrate class, same workflow shape, same unit — but their hardware is undisclosed and their scaling axis is workers, not shards. |
| Scaling reported as a ratio | Burckhardt et al., PVLDB 15(8), 2022 [3]: all throughput normalized to 1×1; gains x2.35–x18.6 | **Precedent, not a baseline.** Establishes that this field reports ratios; gives no absolute number to compare to. |
| Temporal / Conductor / Airflow throughput | Nagpal, SoftwareX 35, 2026 [11] | **Could not verify.** Paper confirmed to exist; full text inaccessible. Do not quote a number. |
| 0 duplicates in 3,512 completions, 2 of 3 nodes killed | Li, arXiv:2609.29095, 2026 [13]: 56% and 74% duplicate rates with no substrate enforcement; 28% → 4% with idempotency keys on every write | **Not comparable as a measurement; the strongest supporting argument in the document.** Different layer, different unit, same conclusion — and the key shape they recommend is the one durexec uses. |
| 878/1,000 = 87.8% accepted under a 2-of-3 node kill | Lee et al., SOSP 2021 [2]: ≈100% graceful, ≈98% without migration, below 90% with neither, 10,000 shards on 60 servers | **Fair order-of-magnitude comparison.** We are in the "neither graceful mechanism" band, under a much harsher fault, at three orders of magnitude smaller scale. |
| Replay under 1 ms | Li et al., PVLDB 16(11), 2023 [5]: replay 0.33–1.6×, retroaction 0.11–0.78× of original execution time | **Not comparable.** Different quantity — whole-trace replay versus per-workflow history rehydration. Would become comparable if reported as a ratio. |
| Ownership moves within seconds; ~30 s shard unavailability | Lee et al., SOSP 2021 [2]: placement solving P90 ≈10 s, P99 ≈50 s in production | **Fair order-of-magnitude comparison**, with a three-orders-of-magnitude scale caveat. |
| Failover: 9–15 s to service, manual, 0–2 events lost | Goldstein et al., PVLDB 13(5), 2020 [1]: 1.8 s average failover, automatic, 3 active instances, no loss | **Fair on the quantity, unfair on the mechanism, and unfavourable to us.** 5–8× slower and we lose data. Never quote our 0.13 s `pg_promote()` against their 1.8 s. |
| Whole-service log replay cost | Goldstein et al. [1]: 77 s original run recovered in 40 s (≈0.52×) | **Fair in spirit.** Confirms the right order of magnitude for log replay; our number is not that quantity. |
| Recall 0.571, FPR 0.939, hard-negative FPR 1.000 at τ=0.90 | Gill et al., IPDPS 2025 [7]: GPTCache FPR 33.3% standalone, **54% on contextual queries**; MeanCache 12.7% and 3% | **Rough sanity check on FPR.** Their non-duplicates are ordinary; 91 of our 115 are deliberate hard negatives. The 54% contextual figure is the closest in difficulty. |
| "A single threshold cannot separate these" | Schroeder et al., ICLR 2026 [9]: correct/incorrect similarity means **0.84 vs 0.85** over 45,000 entries; optimal thresholds span 0.71–1.0 | **Corroboration, and the strongest comparison in the document.** Independently reproduced on a corpus 211× larger; our shipped band sits exactly in their overlap region. |
| "Embeddings miss a one-token difference" | Weller et al., EACL 2024 [10]: bi-encoders ≤12% pairwise accuracy vs 25% random; `all-mpnet-base-v2` 8.1%; humans 100% | **Structural corroboration, not a numeric baseline.** Published for negation; not found for dates or numbers. |
| "A general embedding is not precise enough" | Gill et al., arXiv:2504.02268, 2025 [12]: ModernBERT precision **64% → 84%** on Quora after one epoch of domain fine-tuning | **Fair comparison of the right quantity on a different corpus.** Supports the diagnosis; preprint, flag it. |
| "One component versus four to six" | **None.** No peer-reviewed component-count or deployment-footprint measurement exists for workflow engines. | **Not comparable — nothing published to compare against.** Present as an architectural description cited to primary documentation, never as a measured result. |

---

## What could not be verified, listed honestly

| Source | Why |
|---|---|
| Li et al., *The VLDB Journal* 34(3), 2025 [6] — "DBOS: three years later" | Full text is paywalled; the MIT DSpace copy is **embargoed until 2026-04-29** and OpenAlex lists the article as not open access. The abstract promises *"detailed performance evaluations"*, but none could be read. **No number from this paper is quoted anywhere above.** A widely repeated figure of ~30,000 workflow executions/sec on a Postgres-backed queue comes from a **vendor benchmark**, not from this journal paper, and must not be attributed to it. |
| Daraghmi et al., *Applied Sciences* 12(12), 2022 [4] | MDPI returned HTTP 403 on every route tried, including the PDF endpoint and the DOAJ landing page. The abstract and metadata (doi 10.3390/app12126242, article 6242) were verified; **the evaluation numbers could not be.** Note also a near-miss risk: a 2025 *Scientia Iranica* paper compares Paxos with an enhanced saga and reports throughput and latency — those figures belong to a different study and must not be attributed to [4]. |
| Nagpal, *SoftwareX* 35, 102998, 2026 [11] | Existence, author, journal, volume, page and DOI verified through Crossref. Full text returned HTTP 403 from both ScienceDirect and the Elsevier article API. **No number quoted.** |
| Wang et al., arXiv:2510.26835, 2025 [16] — the 15% / 3% false-match figures | The figures appear in a design-motivation section, and the paper's own abstract describes parts of its results as *"theoretical projections"*. **Could not be confirmed as a measurement**, so not quoted as one. |
| A published minimal-pair benchmark where the deciding token is a **date** or a **number** | Searched; not found in 2020–2026. NevIR [10] is the closest, with negation as the deciding token. This gap is what makes Part 2 of the novelty answer honest. |
| A peer-reviewed PostgreSQL failover measurement, 2020–2026 | Searched; not found. Only vendor documentation, tutorials and practitioner blogs (30–60 s default Patroni, ~40 s in one lab test). **None cited above.** |
| "IdempotencyBench" | Exists only as a GitHub repository (Gopnal Swamy, 2026), cited by [13]. **Not peer-reviewed; no number taken from it.** |
