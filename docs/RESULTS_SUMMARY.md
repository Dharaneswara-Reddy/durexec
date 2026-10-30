# All measured results — best valid run of each

Every number below came from a run against the live cluster, the real
`all-MiniLM-L6-v2` encoder, or a Go benchmark on this host. Nothing is
estimated. Where a run was superseded or invalidated, only the valid one is
quoted; `docs/results/README.md` records the invalid ones and why they are
kept.

Hardware for every figure: **one laptop** (13th Gen Core i5-1335U) running
containers, or a 2-vCPU / 3.7 GiB minikube node. Absolute throughput is
therefore **not** comparable with published data-centre numbers. Scaling
ratios and correctness counts are.

---

## 1. Correctness under failure — the headline results

| Metric | Target | **Measured** | Source |
|---|---|---|---|
| Steps executed twice, 1,000 workflows, 2 nodes killed mid-run | 0 | **0 of 3,512** activity completions | `duplicates-under-failure.txt` |
| Same, re-run after the two liveness fixes | 0 | **0 of 3,052** across 763 runs | `duplicates-under-failure-after-liveness-fix.txt` |
| Accepted work lost when 2 of 3 nodes are killed | 0 | **0** — all 878 accepted executions COMPLETED | `duplicates-under-failure.txt` |
| Requests refused (not lost) during the outage | — | 122 received `409 NotOwner`; client gave up after 6 tries | same |
| Duplicate `(shard, workflow, run, k)` rows | 0 | **0** | same |
| Acceptance matrix, 36 FR/NFR rows | 0 FAIL | **0 FAIL**, identical outcome across 3 builds | `acceptance-matrix-final-image.txt` |

**Availability is 87.8 %** (878 of 1,000 accepted) while two of three nodes
were dead. The 122 refusals are a deliberate trade-off: orphan adoption waits
two lease TTLs (~30 s) so a brief stall does not move shards. NFR-4 asked for
one TTL (10 s). Shortening it would meet the target at the cost of churn.

---

## 2. Throughput and scaling

Best saturated run, concurrency 128, 20-second windows:

| Shards | Throughput | Ratio | p50 latency |
|---|---|---|---|
| 1 | 12.80 wf/s | — | 9.80 s |
| 2 | 23.15 wf/s | **1.81×** | 5.46 s |
| 4 | 46.30 wf/s | **2.00×** | — |

Reproduced twice on two later builds: 12.80 / 24.10 / 47.15 and
12.80 / 24.50 / 47.40 wf/s. Source `scaling-saturated.txt`,
`scaling-saturated-after-liveness-fix.txt`.

Independent confirmation on the Kubernetes lab (Phase 6, open-loop generator):

| Nodes | Throughput | Ratio |
|---|---|---|
| 1 | 12.98 wf/s | — |
| 2 | 25.78 wf/s | **1.99×** |

Source `phase6-sweep.csv` (32 rows, tag `aw600`).

**Do not quote the concurrency-16 run.** It plateaued at 8.5 wf/s regardless
of shard count because the *generator* was the bottleneck
(16 ÷ 1.9 s ≈ 8.4 wf/s). It is kept as a record of the trap.

---

## 3. Replay and rebalance

| Metric | Target | **Measured** | Source |
|---|---|---|---|
| Replay of 200 events (99 recorded activities), 50 samples | < 50 ms | **p50 3.11 ms, p99 5.65 ms** — 8.9× under budget | `go test ./internal/bench -run TestNFR6ReplayBudget` |
| Replay, 1,000 events | — | 10.7 ms/op | `BenchmarkReplayLongHistories` |
| Replay, 2,000 events | — | 23.3 ms/op | same |
| Replay, 5,000 events | — | 60.6 ms/op | same |
| Shards moved when a 4th node joins 3 | a minority | **5 of 16 (31.2 %)**; 11 untouched | `TestAddingAFourthNodeMovesAMinorityOfShards` |
| Keys moved, 2 → 3 nodes | ≈ fair share (33.3 %) | 33.69 % | `TestRingRebalanceMovesAboutOneNodesShare` |

Replay cost is **linear** in history length (10.7 / 23.3 / 60.6 ms at 1k / 2k /
5k events), which is the expected shape and the argument for log compaction
as future work.

> **Correction to `README.md`.** The root README claims *"0.9 ms at the 99th
> percentile"* for 200-event replay. That figure is not supported by any
> recorded run — the acceptance matrix states plainly that its histogram mixes
> all history lengths and that no run had 200 or more events. The benchmark it
> says to run was executed on 2026-10-28 and gives **p99 = 5.65 ms**. The claim
> still passes NFR-6 comfortably, but the number in the README is wrong and
> should be replaced with this one.

---

## 4. Failover

### Shard ownership (engine level)

| Metric | **Measured** |
|---|---|
| Lease TTL / renew interval | 10 s / 3 s |
| Fencing token advanced mid-flight | 63 → 64, one effect performed |
| Duplicate events after ownership change | **0** |

### Database standby promotion (`make promote-drill`)

| | Run 2 | Run 3 |
|---|---|---|
| `pg_promote()` call | 0.14 s | 0.13 s |
| Kill → promoted and writable | 0.65 s | 0.82 s |
| Kill → first new workflow accepted | 15.39 s | 8.87 s |
| Kill → that workflow COMPLETED | 19.51 s | 14.02 s |
| **Committed events lost** | **2** | **1** |
| Workflows lost entirely | 0 | 0 |
| Duplicate-event assertion on promoted DB | clean | clean |

**State this weakness before an examiner does.** Replication is async, so a
crash loses whatever WAL had not reached the standby — 1–2 events of ~3,000
per run. Failover is **manual**: nothing detects the dead leader. Total
unavailability is **9–15 s**, dominated by container recreation, not by
promotion. AMBROSIA (VLDB 2020) does this in **1.8 s, automatically, with no
loss**. They beat us.

---

## 5. External effects (Phase 7 and Phase 8)

Seven scenarios, each reading PostgreSQL *and* an independent effect ledger in
a separate pod that survives the engine being killed.

| Property | **Measured across all 7 scenarios** |
|---|---|
| Invocations ≥ effects | holds everywhere |
| Effects per dedup key | **exactly 1** |
| Duplicate `(run_id, seq)` | **0** |
| Pending tasks left behind | **0** |

Representative counters: `{Requests:2, Performed:1, Replayed:1, Injected:1}` —
two requests, one real effect, one served from the ledger.

Phase 8 closed the two hardest boundaries:

- **(H) Real `SIGKILL` between effect and journal write** —
  `Requests=2 Performed=1 Replayed=1 Injected=0`, one journal completion.
- **(I) Ownership moved mid-flight** — one effect, token 63→64. **But the
  stale worker was stopped by the task-claim check, not by fencing.** The
  fencing path was not the thing exercised here; say so.

This proves **at-least-once invocation with effectively-once effect,
conditional on the recipient honouring the idempotency key**. It is *not*
exactly-once.

**Open gap:** scenario E — a failed saga leaves the external effect standing
with **0 compensations**.

---

## 6. Storage cost

| Metric | **Measured** |
|---|---|
| Events | 500,540 |
| Executions | 41,734 |
| Events per workflow | **11.99** |
| Bytes per event | **418 B** (308 heap + 110 index) |
| Bytes per workflow | **5,548 B** |
| Database size | 221 MB |

Also recorded: `tasks` carried **4,472 kB of indexes over 24 kB of heap** —
index bloat worth fixing.

Backup drill: 247 MB → **24.6 MB in 2.0 s** (`pg_dump -Fc`), restore **4.3 s**,
all counts match, 0 duplicate journal records. **But** it restored into a second
database in the *same instance*, so losing the instance, volume or node is
**NOT TESTED**, and `archive_mode=off` means there is **no PITR at all**.

---

## 7. Semantic idempotency — the research contribution

Same 213 pairs, same real encoder, shipped thresholds τ ∈ [0.85, 0.90):

| Arm | Recall | FPR | Precision | F1 |
|---|---|---|---|---|
| Exact match (baseline) | 0.143 | 0.000 | 1.000 | 0.250 |
| Pure semantic | 0.571 | **0.939** | 0.341 | 0.427 |
| Hybrid (fields exact + text semantic) | 1.000 | 0.000 | 1.000 | 1.000 |

**Do not quote the 1.000.** Three reasons, all recorded:

1. **The model was never consulted.** Once deciding fields are removed from
   these intents no free text remains, so the similarity half contributed
   nothing. It measures exact matching over values scraped from prose.
2. **The scraper's vocabulary was written by reading this corpus** — selection
   on the test set.
3. **Labels are generated by construction**, not hand-assigned.

A separate **8-pair free-text probe**, which puts the decision back where
similarity must make it, scored **3 of 8**: all four reworded repeats missed.
`"refundable fare"` vs `"non refundable fare"` scored **0.9346** and would be
wrongly suppressed. The baseline's failure mode survived the fix — it moved
into the text the gate does not cover.

**The pure-semantic 93.9 % false-positive rate is the honest headline**, and
vCache (ICLR 2026) independently reports the same problem across ~45,000
examples with cut-offs in the same 0.84–0.85 band. Our negative result is a
reproduced published finding, not a student error.

The double-booking fixture is caught at **every** threshold in the sweep.

---

## 8. Test suite and engineering health

| Metric | **Measured** |
|---|---|
| Integration suite | **1220 passed, 0 failed, 0 skipped, 2 blocked** |
| `gofmt`, `go vet` | clean |
| `-race` on 7 critical packages | clean |
| Acceptance matrix | 0 FAIL across 36 rows, 3 builds |
| Terraform plan (Phase 9) | 18 resources, exactly 1 ingress rule (tcp/22 from one /32), no `0.0.0.0/0` |
| Engine source | ~43,000 lines Go, ~23,000 lines tests |

**Five "green but wrong" defects** found by reading code, not by tests failing
— each passed every health check while being silently wrong:

1. Hash ring collapsed 16 shards onto 2.
2. Three nodes running three different builds.
3. Test suite deleted four shards' lease rows from the live cluster.
4. `/healthz` reported `owned_shards=16` on every node regardless of ownership.
5. A lost sequence-number race was reported as lost ownership, permanently
   retiring all four of a shard's workers while the node reported healthy.

This list is a result in its own right: it is the evidence for "verify the
running state, not just the code".

---

## 9. Comparison against published work

| # | Property | **Ours** | Compared against | Theirs | Verdict |
|---|---|---|---|---|---|
| 1 | Throughput | 46.3 wf/s | DBOS (CIDR 2026) | 319 /s | They are faster; they never state their hardware |
| 2 | Scaling ratio | 1.81× then 2.00× | Netherite (VLDB 2022) | ratios only | Our method is the standard one |
| 3 | Duplicate steps | **0 of 3,512** | Microsoft (2026) | agents repeated actions 56–74 % of the time | Different test; proves the problem is real |
| 4 | Availability under failure | **87.8 %** | Shard Manager (SOSP 2021) | below 90 % | Fair comparison, under a harder failure |
| 5 | DB failover | 9–15 s, manual, 0–2 events lost | AMBROSIA (VLDB 2020) | **1.8 s, automatic, lossless** | **They beat us** |
| 6 | Reworded-duplicate detection | 93.9 % FPR | vCache (ICLR 2026) | same finding at 45,000 examples | Our failure is a published result |

---

## 10. What is NOT measured

Stating these plainly is part of the result.

| Gap | Status |
|---|---|
| Cloud deployment (AWS Tier 1 / Tier 2) | **NOT RUN** — Phase 9 was preparation only; no resource created |
| Recovery from losing the DB instance, volume or node | **NOT TESTED** — restore drill used the same instance |
| Point-in-time recovery | **Impossible** — `archive_mode=off` |
| Compensation after a successful external effect | **Open gap** — scenario E leaves the effect standing |
| Semantic check added latency (NFR-7) | **BLOCKED** |
| Hand-labelled retry pairs | **Not done** — all labels generated by construction |
| Cross-run (`dx2:`) idempotency key scheme | Designed, not built |
| Event-log garbage collection | Not built; log grows without bound |

---

*Generated 2026-10-28. Raw output for every figure is in `docs/results/`;
`docs/results/README.md` says which runs are valid and which were kept as
records of a trap.*
