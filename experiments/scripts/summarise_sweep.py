#!/usr/bin/env python3
"""Compute scaling ratios and bottleneck verdicts from a shard-sweep CSV.

WHY THIS EXISTS. `cmd/loadgen -mode scaling` computes its own 1-to-N ratios,
but only when every point runs inside one process: throughput.go builds a
`results` map and divides by `results[1]`. `shard_sweep.sh` deliberately runs
one invocation per point so that a crash leaves usable rows, which means no
invocation ever sees a baseline and loadgen prints "NO EVIDENCE" for every
ratio. The arithmetic therefore has to happen here, from the durable file.

VERDICTS. Three, never two, and never a single utilisation threshold:

  GENERATOR-BOUND  the generator, not the engine, set the number. Excluded
                   from the scaling curve.
  ATTRIBUTED       a named resource is the constraint, with evidence.
  INCONCLUSIVE     nothing can be established. Kept, reported, and excluded
                   from the headline curve.

Usage:
    python3 experiments/scripts/summarise_sweep.py [results.csv]
"""

import csv
import sys
from collections import defaultdict

# A generator above this is doing enough work that its own cost is a plausible
# limit; it is a flag for investigation, never a verdict on its own.
GEN_CPU_SUSPECT = 90.0
# Below this, a closed-loop generator at fixed concurrency is very likely the
# limit rather than the engine, because it simply is not asking for more.
GEN_CPU_IDLE = 70.0
# Minimum completions before a point is worth believing at all.
MIN_SAMPLES = 150


def f(row, key):
    v = (row.get(key) or "").strip()
    try:
        return float(v)
    except ValueError:
        return None


def classify(row, same_shards_higher_conc):
    """Return (verdict, reason). Uses several signals, never one threshold."""
    comp = f(row, "completed")
    gen = f(row, "gen_cpu_pct")
    eng = f(row, "eng_cpu_pct")
    wfps = f(row, "wf_per_s")

    if row.get("verdict") not in ("RECORDED", None, ""):
        return "INVALID", f"runner recorded {row.get('verdict')}"

    if wfps is None:
        return "INCONCLUSIVE", "no throughput parsed"

    if comp is not None and comp < MIN_SAMPLES:
        return "INCONCLUSIVE", f"only {int(comp)} completions; below {MIN_SAMPLES}"

    # The decisive test: if the SAME configuration produced materially more
    # throughput at higher concurrency, this point was offered-load limited.
    # That is evidence, not an inference from a utilisation number.
    if same_shards_higher_conc:
        best = max(same_shards_higher_conc)
        if best > wfps * 1.15:
            return ("GENERATOR-BOUND",
                    f"same shard count reached {best:.2f} wf/s at higher "
                    f"concurrency ({best / wfps:.2f}x), so offered load set this")

    if gen is not None and gen >= GEN_CPU_SUSPECT:
        return ("INCONCLUSIVE",
                f"generator at {gen:.0f}% CPU; engine limit and generator "
                "limit cannot be separated")

    if gen is not None and gen < GEN_CPU_IDLE and not same_shards_higher_conc:
        return ("INCONCLUSIVE",
                f"generator only {gen:.0f}% busy and no higher-concurrency "
                "point exists to rule out offered-load limiting")

    if eng is not None and eng >= 250:
        return "ATTRIBUTED", f"engine CPU peaked at {eng:.0f}% of 300% available"

    return "ATTRIBUTED", "highest observed with the generator not saturated"


def main() -> int:
    path = sys.argv[1] if len(sys.argv) > 1 else \
        "docs/results/local-shard-sweep/results.csv"
    try:
        lines = [l for l in open(path) if not l.startswith("#")]
    except OSError as e:
        print(f"cannot read {path}: {e}", file=sys.stderr)
        return 1

    rows = list(csv.DictReader(lines))
    if not rows:
        print("no rows")
        return 1

    by_shards = defaultdict(list)
    for r in rows:
        w = f(r, "wf_per_s")
        if w is not None:
            by_shards[r.get("shards")].append((f(r, "concurrency") or 0, w))

    print(f"{'config':<22}{'wf/s':>9}{'p99 s':>9}{'gen%':>6}{'eng%':>6}"
          f"{'own':>5}  verdict")
    print("-" * 96)

    curve = {}
    for r in rows:
        higher = [w for c, w in by_shards.get(r.get("shards"), [])
                  if c > (f(r, "concurrency") or 0)]
        verdict, reason = classify(r, higher)
        w = f(r, "wf_per_s")
        print(f"{r.get('config_id',''):<22}"
              f"{(f'{w:.2f}' if w is not None else '-'):>9}"
              f"{(r.get('p99_s') or '-'):>9}"
              f"{(r.get('gen_cpu_pct') or '-'):>6}"
              f"{(r.get('eng_cpu_pct') or '-'):>6}"
              f"{(r.get('owner_nodes') or '-'):>5}  {verdict}")
        print(f"{'':<22}{'':>35} -> {reason}")
        if verdict == "ATTRIBUTED" and w:
            key = int(f(r, "shards") or 0)
            if w > curve.get(key, 0):
                curve[key] = w

    print()
    if len(curve) >= 2:
        base_k = min(curve)
        base = curve[base_k]
        print(f"=== scaling curve (ATTRIBUTED points only, base = {base_k} shard) ===")
        for k in sorted(curve):
            print(f"  {k:>3} shards : {curve[k]:8.2f} wf/s   {curve[k] / base:5.2f}x")
    else:
        print("=== no scaling curve ===")
        print("  Fewer than two ATTRIBUTED points. Every other point was")
        print("  generator-bound or inconclusive, so there is no curve to draw.")
        print("  That is the correct output, not a failure: a curve drawn through")
        print("  offered-load-limited points would be measuring the generator.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
