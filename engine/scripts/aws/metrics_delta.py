#!/usr/bin/env python3
"""Per-run deltas of the engine's own /metrics, summed over every pod.

Usage: metrics_delta.py BEFORE_1 AFTER_1 [BEFORE_2 AFTER_2 ...]

Each pair is one pod's Prometheus text exposition before and after the load
window. Counters are differenced per pod, then summed; gauges report the
maximum across pods AFTER. Series are summed over their labels, except where a
label is the thing being asked (lease transitions by kind, task failures by
reason), which are printed per label value.

A metric absent from either file of a pair prints `unavailable` -- an engine
that stopped exporting a series must not read as an engine that did nothing.
"""
import re
import sys

LINE = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+([^\s]+)')

COUNTERS = [
    "db_pool_acquires_total", "db_pool_empty_acquires_total",
    "db_pool_canceled_acquires_total", "db_pool_acquire_wait_seconds_total",
    "workflow_completed_total", "workflow_starts_total",
    "worker_task_failures_total", "fencing_rejections_total",
    "lease_transitions_total", "process_cpu_seconds_total",
    "go_gc_duration_seconds_sum", "go_gc_duration_seconds_count",
    "replay_duration_seconds_sum", "replay_duration_seconds_count",
    "go_memstats_alloc_bytes_total",
]
BY_LABEL = {"lease_transitions_total", "worker_task_failures_total", "workflow_starts_total"}
GAUGES = ["go_goroutines", "process_resident_memory_bytes", "db_pool_conns_acquired",
          "db_pool_conns_total", "db_pool_conns_max", "task_queue_oldest_ready_seconds"]


def parse(path):
    out = {}
    try:
        text = open(path).read()
    except OSError:
        return None
    for line in text.splitlines():
        if line.startswith("#"):
            continue
        m = LINE.match(line)
        if not m:
            continue
        try:
            v = float(m.group(3))
        except ValueError:
            continue
        out[(m.group(1), m.group(2) or "")] = v
    return out or None


def total(series, name, labels=None):
    vals = [v for (n, l), v in series.items() if n == name and (labels is None or l == labels)]
    return sum(vals) if vals else None


def main():
    files = sys.argv[1:]
    if not files or len(files) % 2:
        sys.exit(__doc__)
    pairs = [(parse(files[i]), parse(files[i + 1])) for i in range(0, len(files), 2)]
    print(f"pods={len(pairs)}")
    print(f"pods_unreadable={sum(1 for b, a in pairs if b is None or a is None)}")
    ok = [(b, a) for b, a in pairs if b is not None and a is not None]
    for name in COUNTERS:
        ds, absent = [], 0
        for b, a in ok:
            x, y = total(b, name), total(a, name)
            if x is None and y is None:
                # client_golang exports a labelled counter only after its
                # first observation, so "absent in both" means "never
                # incremented OR not registered" -- reported as such, not as 0.
                absent += 1
                continue
            ds.append((y or 0) - (x or 0))
        if not ok:
            v = "unavailable"
        elif absent == len(ok):
            v = "absent"
        else:
            v = f"{sum(ds):.6g}"
        print(f"d_{name}={v}")
        if name in BY_LABEL and ds:
            labels = sorted({l for _, a in ok for (n, l) in a if n == name and l})
            for lab in labels:
                d = sum((total(a, name, lab) or 0) - (total(b, name, lab) or 0) for b, a in ok)
                if d:
                    print(f"d_{name}{lab}={d:.6g}")
    for name in GAUGES:
        vs = [total(a, name) for _, a in ok]
        vs = [v for v in vs if v is not None]
        print(f"max_{name}=" + (f"{max(vs):.6g}" if vs else "unavailable"))
    gcn = sum((total(a, "go_gc_duration_seconds_count") or 0) - (total(b, "go_gc_duration_seconds_count") or 0) for b, a in ok)
    gcs = sum((total(a, "go_gc_duration_seconds_sum") or 0) - (total(b, "go_gc_duration_seconds_sum") or 0) for b, a in ok)
    print("gc_pause_mean_ms=" + (f"{1000 * gcs / gcn:.3f}" if gcn else "unavailable"))
    acq = sum((total(a, "db_pool_acquires_total") or 0) - (total(b, "db_pool_acquires_total") or 0) for b, a in ok)
    emp = sum((total(a, "db_pool_empty_acquires_total") or 0) - (total(b, "db_pool_empty_acquires_total") or 0) for b, a in ok)
    print("pool_empty_fraction=" + (f"{emp / acq:.4f}" if acq else "unavailable"))


if __name__ == "__main__":
    main()
