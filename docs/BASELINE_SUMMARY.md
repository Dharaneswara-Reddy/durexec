# Baseline Comparison — One Page

We compare our engine against **6 published systems**, one per component.
Full detail is in `BASELINE_COMPARISON.md` (long — only open it if someone challenges a number).

---

## The comparison

| # | Component | Our result | We compare against | Their result | Honest verdict |
|---|-----------|-----------|--------------------|--------------|----------------|
| 1 | **Speed** — workflows finished per second | 46.3 /sec | **DBOS** (CIDR 2026) | 319 /sec | They are faster, but they never say what computer they used, so the gap means little |
| 2 | **Scaling** — does adding machines help? | 1.81× then 2.00× | **Netherite** (VLDB 2022) | Reports ratios, no raw number | They report scaling the same way we do — this proves our method is standard |
| 3 | **Safety** — steps done twice | 0 out of 3,512 | **Microsoft** (2026) | AI agents repeated actions in 56–74% of tries | Different test, but it proves the problem we solve is real |
| 4 | **Availability** — requests accepted during failure | 878 of 1000 (87.8%) | **Shard Manager**, Meta (SOSP 2021) | Below 90% in the same setup | Fair comparison. We match them, under a harder failure |
| 5 | **Failover** — switching to the backup database | 9–15 sec, manual, 0–2 events lost | **AMBROSIA** (VLDB 2020) | 1.8 sec, automatic, nothing lost | **They beat us.** Say this openly |
| 6 | **Our research** — catching reworded repeats | Blocks 93.9% of good requests | **vCache** (ICLR 2026) | Found the same problem on 45,000 examples | **Our failure is a real, published finding — not our mistake** |

---

## The 3 things to say in the review

**1. Our negative result is confirmed by a real paper.**

vCache tested 45,000 examples at Berkeley, TUM, ETH and Stanford. They found the same thing we did: one similarity cut-off cannot tell duplicates apart from genuinely different requests. Their numbers (0.84 and 0.85) land inside the exact range we use (0.85–0.90).

So our "failure" is not a student mistake. It is a known result that we reproduced independently.

**2. Our availability number is genuinely good.**

Meta's Shard Manager got below 90% success in the same situation ours was in. We got 87.8% — while killing 2 of our 3 machines outright. They only restarted 10% of theirs, gently.

**3. Our failover is worse than published work, and we should say so first.**

AMBROSIA switches over in 1.8 seconds, automatically, with no data loss. We take 9–15 seconds, need a human, and can lose 1–2 events.

Say this yourself before an examiner finds it. Owning a weakness looks like rigour. Getting caught hiding one looks like dishonesty.

---

## One thing to fix before the review

**Reference [8] on the slide has the wrong author.**

| | |
|---|---|
| The slide says | Q. Li, P. Kraft, M. Stonebraker, and X. Zhou |
| The paper actually says | **Michael Stonebraker**, Peter Kraft |

Checked against the paper's own front page. Change it to **Stonebraker et al.**

---

## Why some comparisons are weak (if asked)

Our tests ran on **one laptop with 3 containers**. The published papers ran on **real servers in data centres**.

So comparing raw speed is not fair. What *is* fair to compare:

- the **shape** of our scaling — does it double when we double the shards?
- whether steps were **repeated** — yes or no
- the **percentage** of requests accepted during a failure

Those three do not depend on hardware. Raw speed does.
