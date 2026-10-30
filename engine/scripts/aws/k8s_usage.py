#!/usr/bin/env python3
"""Reduce kubelet Summary API samples to per-pod and per-node CPU/memory/network.

Usage: k8s_usage.py SAMPLES.jsonl [WINDOW_START_EPOCH WINDOW_END_EPOCH]

Each line of SAMPLES.jsonl is {"node": "<name>", "summary": <stats/summary JSON>}
as written by eks_loadgen.sh. The Summary API reports CUMULATIVE CPU
nanoseconds with their own timestamps, so CPU over an interval is exact:
(usage_end - usage_start) / (time_end - time_start). Only samples whose CPU
timestamp falls inside the window (when given) are used, so the warm-up and
the grace period do not dilute the figure.

Prints key=value lines:
  pod|<ns>/<pod>|cpu_cores, pod|...|mem_peak_bytes, pod|...|net_rx_Bps, pod|...|samples
  node|<node>|cpu_cores, node|<node>|mem_peak_bytes, node|<node>|net_rx_Bps, ...
A pod seen in fewer than two samples prints cpu_cores=unavailable.
"""
import json
import sys
from datetime import datetime


def ts(s):
    if not s:
        return None
    s = s.rstrip("Z")
    if "." in s:
        head, frac = s.split(".", 1)
        s = head + "." + frac[:6]
    try:
        return datetime.fromisoformat(s).timestamp()
    except ValueError:
        return None


def net(obj):
    rx = tx = 0
    found = False
    n = obj.get("network") or {}
    for itf in n.get("interfaces") or ([n] if "rxBytes" in n else []):
        if "rxBytes" in itf:
            rx += itf.get("rxBytes", 0)
            tx += itf.get("txBytes", 0)
            found = True
    return (rx, tx) if found else None


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    lo = float(sys.argv[2]) if len(sys.argv) > 3 else None
    hi = float(sys.argv[3]) if len(sys.argv) > 3 else None
    series = {}  # key -> list of (t, cpu_ns, mem, net)

    def add(key, cpu, mem, nw):
        t = ts((cpu or {}).get("time"))
        u = (cpu or {}).get("usageCoreNanoSeconds")
        if t is None or u is None:
            return
        if lo is not None and not (lo <= t <= hi):
            return
        series.setdefault(key, []).append((t, u, (mem or {}).get("workingSetBytes"), nw))

    for line in open(sys.argv[1]):
        line = line.strip()
        if not line:
            continue
        try:
            rec = json.loads(line)
        except ValueError:
            continue
        s, node = rec.get("summary") or {}, rec.get("node", "?")
        nd = s.get("node") or {}
        add("node|" + node, nd.get("cpu"), nd.get("memory"), net(nd))
        for p in s.get("pods") or []:
            ref = p.get("podRef") or {}
            add(f"pod|{ref.get('namespace')}/{ref.get('name')}|{node}", p.get("cpu"), p.get("memory"), net(p))

    for key in sorted(series):
        pts = sorted({p[0]: p for p in series[key]}.values())
        mems = [p[2] for p in pts if p[2] is not None]
        print(f"{key}|samples={len(pts)}")
        print(f"{key}|mem_peak_bytes={max(mems) if mems else 'unavailable'}")
        if len(pts) < 2 or pts[-1][0] <= pts[0][0]:
            print(f"{key}|cpu_cores=unavailable")
            continue
        dt = pts[-1][0] - pts[0][0]
        print(f"{key}|cpu_cores={(pts[-1][1] - pts[0][1]) / 1e9 / dt:.3f}")
        print(f"{key}|interval_s={dt:.1f}")
        # Peak over any adjacent pair: a short burst hidden by the average.
        peak = max((b[1] - a[1]) / 1e9 / (b[0] - a[0]) for a, b in zip(pts, pts[1:]) if b[0] > a[0])
        print(f"{key}|cpu_cores_peak={peak:.3f}")
        if pts[0][3] and pts[-1][3]:
            print(f"{key}|net_rx_Bps={(pts[-1][3][0] - pts[0][3][0]) / dt:.0f}")
            print(f"{key}|net_tx_Bps={(pts[-1][3][1] - pts[0][3][1]) / dt:.0f}")


if __name__ == "__main__":
    main()
