# Local shard sweep — corrected analysis (2026-10-30)

`results.csv` and `FINDINGS.md` are left exactly as they were. This file
records what was re-derived from the raw evidence, and corrects one erratum.

## 1. Latency re-derived from the raw loadgen output

`results.csv` columns `p50_ms`, `p95_ms` and `p99_ms` hold the shard count
on every row (1, 2, 4, 8, 16), and `owner_nodes` is empty on every row. Both
were written by a working-tree version of `experiments/scripts/shard_sweep.sh`
(rows are stamped `fde6ae0, dirty`) that was never committed, so the faulty
extraction cannot be inspected. The committed extractor (`lat()` in
`shard_sweep.sh`, the same as `eks_loadgen.sh`) applied to `raw/*.txt` gives:

| config | completed | wf/s | completed ÷ window | p50 (s) | p95 (s) | p99 (s) |
|---|---|---|---|---|---|---|
| shards1-c128 | 105 | 5.25 | 5.25 | 22.7845 | 24.5859 | 24.8801 |
| shards2-c128 | 97 | 4.85 | 4.85 | 13.2737 | 15.6646 | 15.7809 |
| shards4-c128 | 294 | 14.70 | 14.70 | 5.3698 | 8.7937 | 9.2045 |
| shards8-c128 | 577 | 28.85 | 28.85 | 2.9372 | 16.9542 | 19.5895 |
| shards16-c128 | 943 | 47.15 | 47.15 | 2.5652 | 4.3632 | 5.2728 |
| shards16-c256 | 2586 | 129.30 | 129.30 | 1.9153 | 2.6062 | 2.8205 |
| shards16-c512 | 890 | 44.50 | 44.50 | 3.2928 | 15.3651 | 18.6629 |

Every completed count matches the CSV, every throughput equals completed ÷
the 20 s window, and every p50/p99 matches `FINDINGS.md` §2 to its stated
precision. The **published** figures are therefore correct; only the CSV's
latency and owner columns are wrong. Use this table, not the CSV, for
latency. p95 was never published before.

## 2. Erratum to FINDINGS.md §9 item 2

§9 item 2 says the raw `raw/*.txt` loadgen outputs "were never committed".
That is no longer true: all seven are tracked since `ad75cb8`
(`git ls-files docs/results/local-shard-sweep/raw`), and §2's figures can be
re-derived from them, as above.

## 3. Shard ownership is uneven by construction, not by divisibility

`FINDINGS.md` attributes the hottest node to owning 9 of 16 shards. That is
the consistent-hash ring (`internal/shard/ring.go`, 150 vnodes) and it is
deterministic for the pod names `durexec-N.durexec-hl`:

| replicas | S=16 | S=24 | S=32 | S=64 |
|---|---|---|---|---|
| 3 | 5/9/2 (1.69) | 8/11/5 (1.38) | 11/14/7 (1.31) | 21/26/17 (1.22) |
| 4 | 5/6/1/4 (1.50) | 7/8/4/5 (1.33) | 10/11/6/5 (1.38) | 18/19/15/12 (1.19) |
| 6 | 4/4/0/3/2/3 (1.50) | 6/6/2/4/2/4 (1.50) | 8/7/3/4/4/6 (1.50) | 13/14/8/6/12/11 (1.31) |

(figure in brackets: busiest node ÷ fair share). A shard count divisible by
the replica count does **not** give even ownership. Cloud rows now record
`shards_per_owner` and `shard_skew` so this is measured per point.
