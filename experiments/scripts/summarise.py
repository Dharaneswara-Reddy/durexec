#!/usr/bin/env python3
"""Recompute every Phase 6 summary from the CSV on disk.

Usage: summarise.py docs/results/phase6-sweep.csv [tag]

Nothing is held in memory between runs and nothing is carried over from the
process that produced the rows: a sweep killed at trial 4 yields a valid
four-point report. Rows whose verdict column records an INVALID_* preflight
failure are listed separately and excluded from the derived figures, never
dropped silently.
"""
import csv
import sys
from collections import defaultdict

path = sys.argv[1]
only_tag = sys.argv[2] if len(sys.argv) > 2 else None

rows, invalid = [], []
with open(path) as f:
    for r in csv.DictReader(x for x in f if not x.startswith("#")):
        if only_tag and r.get("tag") != only_tag:
            continue
        v = (r.get("lg_verdict") or "")
        if v.startswith("INVALID"):
            invalid.append(r)
        elif v:
            rows.append(r)


def num(r, k):
    try:
        return float(r[k])
    except (KeyError, TypeError, ValueError):
        return None


def fmt(v, nd=2):
    return "-" if v is None else f"{v:.{nd}f}"


print(f"source: {path}")
print(f"valid rows: {len(rows)}   invalid/preflight-failed rows: {len(invalid)}")
for r in invalid:
    print(f"  INVALID shards={r.get('shards')} rate={r.get('rate')} trial={r.get('trial')} -> {r.get('lg_verdict')}")

# ---- throughput versus offered load, by shard count ---------------------
by = defaultdict(list)
for r in rows:
    by[r["shards"]].append(r)

for sh in sorted(by, key=lambda x: int(x)):
    print(f"\n=== {sh} shard(s) ===")
    hdr = (f"{'offered':>8} {'done/s':>8} {'ratio':>6} {'p50 s':>7} {'p95 s':>7} {'p99 s':>7} "
           f"{'verdict':>14} {'lag p99':>8} {'pgCPU%':>7} {'engCPU%':>8} {'commits/s':>10} "
           f"{'WAL kB/s':>9} {'conns':>6} {'poolwait%':>10} {'refused':>8}")
    print(hdr)
    for r in sorted(by[sh], key=lambda x: float(x["rate"])):
        w = num(r, "window_s") or 1
        off, done = num(r, "lg_offered"), num(r, "lg_completed_per_s")
        ratio = (done / off) if (off and done) else None
        pgcpu = (num(r, "cpu_pg_core_s") or 0) / w * 100 if num(r, "cpu_pg_core_s") is not None else None
        engcpu = (num(r, "cpu_engine_core_s") or 0) / w * 100 if num(r, "cpu_engine_core_s") is not None else None
        commits = (num(r, "pg_xact_commit") or 0) / w if num(r, "pg_xact_commit") is not None else None
        wal = (num(r, "pg_wal_bytes") or 0) / w / 1024 if num(r, "pg_wal_bytes") is not None else None
        acq, empty = num(r, "pool_acquires"), num(r, "pool_empty")
        poolwait = (empty / acq * 100) if (acq and empty is not None) else None
        print(f"{fmt(off):>8} {fmt(done):>8} {fmt(ratio):>6} {fmt(num(r,'lg_p50_s'),3):>7} "
              f"{fmt(num(r,'lg_p95_s'),3):>7} {fmt(num(r,'lg_p99_s'),3):>7} "
              f"{(r.get('lg_verdict') or '-'):>14} {fmt(num(r,'lg_lag_p99_s'),4):>8} "
              f"{fmt(pgcpu,1):>7} {fmt(engcpu,1):>8} {fmt(commits,1):>10} {fmt(wal,1):>9} "
              f"{fmt(num(r,'pg_conns_peak'),0):>6} {fmt(poolwait,1):>10} {fmt(num(r,'wf_starts_refused'),0):>8}")

# ---- saturation ---------------------------------------------------------
print("\n=== saturation ===")
sat = [r for r in rows if "NOT_SATURATED" not in (r.get("lg_verdict") or "")]
if not sat:
    best = max(rows, key=lambda r: num(r, "lg_completed_per_s") or 0) if rows else None
    if best:
        print(f"No point reached saturation. Highest sustained throughput "
              f"{fmt(num(best,'lg_completed_per_s'))} wf/s at offered "
              f"{fmt(num(best,'lg_offered'))} wf/s on {best['shards']} shard(s).")
        print("That is a LOWER BOUND on capacity, not a measurement of it.")
else:
    for r in sat:
        print(f"  shards={r['shards']} offered={fmt(num(r,'lg_offered'))} "
              f"done/s={fmt(num(r,'lg_completed_per_s'))} verdict={r.get('lg_verdict')}")

# ---- storage growth ----------------------------------------------------
print("\n=== storage and replay ===")
for r in sorted(rows, key=lambda x: (int(x["shards"]), float(x["rate"]))):
    done, w = num(r, "lg_completed_per_s"), num(r, "window_s") or 1
    wal, commits = num(r, "pg_wal_bytes"), num(r, "pg_xact_commit")
    rs, rc = num(r, "replay_sum_s"), num(r, "replay_count")
    n = (done * w) if done else None
    print(f"  shards={r['shards']} rate={r['rate']:>3}: "
          f"WAL/workflow={fmt((wal/n/1024) if (wal and n) else None,1):>7} kB  "
          f"commits/workflow={fmt((commits/n) if (commits and n) else None,1):>6}  "
          f"mean replay={fmt((rs/rc*1000) if (rs and rc) else None,3):>7} ms over {fmt(rc,0)} replays")

# ---- observer cost -----------------------------------------------------
print("\n=== monitoring cost (as measured, same windows) ===")
for r in sorted(rows, key=lambda x: (int(x["shards"]), float(x["rate"]))):
    w = num(r, "window_s") or 1
    pc, pm = num(r, "cpu_prom_core_s"), num(r, "mem_prom_bytes")
    ec, em = num(r, "cpu_pgexp_core_s"), num(r, "mem_pgexp_bytes")
    tot = num(r, "cpu_engine_core_s")
    share = ((pc + ec) / (pc + ec + tot) * 100) if (pc is not None and ec is not None and tot) else None
    print(f"  shards={r['shards']} rate={r['rate']:>3}: prometheus {fmt((pc/w*100) if pc is not None else None,2)}% core, "
          f"{fmt((pm/2**20) if pm else None,1)} MiB; exporter {fmt((ec/w*100) if ec is not None else None,2)}% core, "
          f"{fmt((em/2**20) if em else None,1)} MiB; series={fmt(num(r,'prom_series'),0)}; "
          f"observer share of observer+engine CPU={fmt(share,1)}%")
