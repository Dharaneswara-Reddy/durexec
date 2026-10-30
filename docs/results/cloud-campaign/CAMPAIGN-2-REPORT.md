# Cloud campaign 2 — Stages E, F, G (AWS)

**Date:** 2026-10-30, 15:21–19:16 UTC (~3 h 55 min) · **Region:** us-east-1a
**Images:** `cf986fb` (engine `sha256:652717ed…`, the same digest as campaign 1).
**Harness:** `4c1cde6` → `283fb8b` → `4dcdbaa` (fixes below; no engine change).
**Infrastructure:** identical to campaign 1 (3 × m7i-flex.large engines,
c7i-flex.large harness, one m7i-flex.large PostgreSQL 16 host, in-VPC operator).
Fresh plan 44/0/0. The infrastructure was kept up for campaign 3, so teardown
is reported in `CAMPAIGN-3-REPORT.md`.

Every number is an **AWS observation** from `docs/results/cloud-campaign/`.

## Bring-up — PASS

Smoke 10/10; NetworkPolicy enforced and restored; three pods on three engine
nodes, each running engine digest `652717ed…`.

## Stage E — open-loop arrival curve (r3/S16/w4x8, floor 2.21 s)

Rates are fractions of the Stage D closed-loop knee (131.1 wf/s); 120 s
measured, 3 trials each.

| Offered wf/s | Completed wf/s | Dropped | p50 | p99 | lease changes | Verdicts |
|---|---|---|---|---|---|---|
| 65.6 (0.5×) | 64.6–64.9 | 0 | 1.5–1.6 s | 2.2 s | 0 | RECORDED ×3 |
| 98.3 (0.75×) | 96.9–97.2 | 0 | 1.5 s | 2.2–2.9 s | 0 | RECORDED ×3 |
| 108.2 (0.825×) | 67.6 / 76.3 / **106.8** | 3,340 / 2,507 / 0 | 8.7 / 4.7 / 1.5 s | 44.5 / 45.2 / **2.7 s** | 10 / 0 / 0 | 2 review, 1 RECORDED |
| 118.0 (0.9×) | 82–85 | 2,234–2,648 | 2.2–3.7 s | 46–48 s | 10 each | CORRECTNESS_REVIEW ×3 |
| 131.1 (1.0×) | 67–99 | 2,700–5,889 | 1.8–14 s | 43–51 s | 10 / 28 / 10 | CORRECTNESS_REVIEW ×3 |
| 144.2 (1.1×) | 75.0 / 102.8 / 128.8 | 5,849 / 3,010 / 0 | 10.9 / 1.8 / 1.7 s | 55.9 / 44.6 / 39.0 s | 14 / 10 / 0 | 2 review, 1 RECORDED |

1. **Sustainable open-loop throughput ≈ 100 wf/s**, below the closed-loop
   knee: a closed loop throttles its own arrivals as latency rises; an open
   loop does not.
2. **Above ~100 wf/s the system overloads, and lease loss is its most
   damaging form.** In 9 of the 11 overloaded trials one engine's five shard
   leases were lost and re-acquired (shards 0/1/4/8/15 — durexec-0's set),
   with 64–165 task failures; starts sent to those shards were **refused with
   409 `NotOwner, shard currently unowned`** and recorded by the runner as
   `CORRECTNESS_REVIEW_failed…`. Overload also happens without lease loss:
   108/s trial 2 had no lease change yet completed only 76 wf/s with p99
   45 s, and 144/s trial 3 matched the offered rate (128.8 wf/s, no drops)
   but still had p99 39 s. Closed-loop runs never lost a lease, even at 96 %
   DB CPU. Hypothesis (not proven here): with pools 60–73 % empty and the DB
   at 85 %+, a renewal misses the 10 s TTL.
3. **Safety held:** 0 duplicate seq, duplicate completion, orphan completion,
   sequence gap or multiple terminal, and every accepted run ended
   `COMPLETED`. Refused starts were never accepted, so nothing was lost; the
   cost is availability and a ~45 s p99.
4. Below the knee, latency is **lower than the c=1 floor** (p50 1.5 s vs
   2.2 s): busy pollers back off less — consistent with the floor being
   polling-bound.

## Stage F — worker sensitivity (r3/S16), knee from the mean of 3 trials

| Workers/shard | Idle commits/s | Knee c | wf/s at knee | p99 | DB CPU | pool empty |
|---|---|---|---|---|---|---|
| 2×4 | 165 | 256 | **106.2** (104.4/107.6/106.6) | 3.4–4.1 s | ~76 % | ~1 % |
| 4×8 (Stage D) | 251 | 256 | **131.1** (129.1/130.0/134.3) | 2.7–3.1 s | 74–96 % | 6–47 % |
| 8×16 | 443 | 256 | **120.0** (120.7/127.3/112.0) | 3.8–7.7 s | ~97 % | ~57 % |

An optimum at 4×8: too few workers is worker-bound with DB headroom; too many
spends DB CPU on idle polling. More workers do lower latency at moderate
load (8×16 at c=128: ~88 wf/s, p50 1.4 s, vs 66.5 wf/s for 4×8).

## Stage G — shard count at 192 total workers (r3), mean of 3 trials

| Shards × workers | Ring skew | Idle commits/s | Knee c | wf/s at knee | p99 |
|---|---|---|---|---|---|
| 16 × 4×8 (Stage D) | 1.69 (5/9/2) | 251 | 256 | **131.1** | 2.7–3.1 s |
| 32 × 2×4 | 1.31 | 303 | 256 | **130.3** (130.6/128.1/132.1) | 2.8–3.4 s |
| 64 × 1×2 | 1.22 | 410 | 256 | **113.8** (112.9/117.3/111.3) | 3.9–4.5 s |

More shards do not help on this deployment: S16 = S32 within noise, S64 is
13 % lower. The binding resource is the shared DB, so evening out ownership
buys nothing, while each shard adds idle overhead.

## Defects found and fixed during the session

| Defect | Effect | Fix | Test |
|---|---|---|---|
| `eks_loadgen.sh` / `eks_fault_trial.sh` never passed `-shard-count` | Every non-16-shard point failed (`-shards 32 exceeds -shard-count 16`); below 16 shards loadgen would have aimed at nonexistent shards | pass the deployed `SHARD_COUNT` | self-test checks every loadgen arg list (fails on old script) |
| CSV header guard `grep -v \| head -1 \| grep` under pipefail | Once the CSV grew, SIGPIPE made the guard refuse valid runs as "different header" | `awk` reads the first data line | self-test: 0/5 vs old 50/50 false refusals on a 20,000-line file |
| Collector copied all raw runs into every bundle | 300+ MB bundles; archives heading past GitHub's 100 MB limit | sha256 manifest of raw runs instead; duplicates untracked (history unchanged, archives in S3) | collector syntax; next bundles |

The invalid first Stage G attempts are kept in `results.csv` as `INVALID_*`
rows; the valid Stage G trials are numbered 11+ (S64) and 31+ (S32).

## Not established

Replica scaling (campaign 3, Stage H), fault recovery (campaign 3), behaviour
with a larger or replicated database, flex-instance variance across days.
No HA, exactly-once arbitrary external effects or production-readiness claim.

## Cost

~3 h 55 min at ~$0.75/h ≈ **$2.95** (estimate, not billing). Cap $6.
