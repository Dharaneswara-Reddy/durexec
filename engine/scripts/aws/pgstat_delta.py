#!/usr/bin/env python3
"""Reduce two pgstat_snapshot.sql outputs (before, after) and an optional file
of mid-window samples to one line of key=value per-run figures.

Usage: pgstat_delta.py BEFORE AFTER [SAMPLES]

SAMPLES is the concatenation of snapshots taken during the window; only its
g_ gauges and g_wait| lines are used, to report peaks and a wait-event profile.

Anything that cannot be computed is printed as `unavailable`, never as 0: a
missing measurement must not be mistaken for an idle resource.
"""
import sys

COUNTERS = ["xact_commit", "xact_rollback", "blks_read", "blks_hit",
            "tup_inserted", "tup_updated", "tup_deleted", "conflicts",
            "temp_bytes", "deadlocks", "blk_read_time_ms", "blk_write_time_ms",
            "wal_records", "wal_fpi", "wal_bytes", "wal_buffers_full",
            "wal_write", "wal_sync", "ckpt_timed", "ckpt_req", "ckpt_write_ms",
            "ckpt_sync_ms", "buffers_checkpoint", "buffers_backend",
            "buffers_backend_fsync", "stmt_calls", "stmt_exec_ms"]


def parse(path):
    kv, extra = {}, {}
    try:
        lines = open(path).read().splitlines()
    except OSError:
        return kv, extra
    for line in lines:
        if line.startswith(("host_cpu|", "host_mem|", "host_disk|", "host_loadavg|")):
            k, _, v = line.partition("|")
            extra[k] = v
        elif "=" in line and not line.startswith("g_wait|"):
            k, _, v = line.partition("=")
            kv[k.strip()] = v.strip()
    return kv, extra


def num(v):
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def cpu_fields(line):
    # "cpu  user nice system idle iowait irq softirq steal guest guest_nice"
    parts = line.split()
    if not parts or parts[0] != "cpu":
        return None
    return [int(x) for x in parts[1:]]


def disk_map(line):
    out = {}
    for rec in line.split(";"):
        f = rec.split()
        if len(f) >= 14:
            # reads_completed, sectors_read, writes_completed, sectors_written, io_ms
            out[f[2]] = (int(f[3]), int(f[5]), int(f[7]), int(f[9]), int(f[12]))
    return out


def fmt(x, nd=3):
    return "unavailable" if x is None else (f"{x:.{nd}f}" if isinstance(x, float) else str(x))


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    b, bx = parse(sys.argv[1])
    a, ax = parse(sys.argv[2])
    out = {}
    t0, t1 = num(b.get("ts_epoch_ms")), num(a.get("ts_epoch_ms"))
    secs = (t1 - t0) / 1000 if t0 is not None and t1 is not None and t1 > t0 else None
    out["pg_window_s"] = secs
    for k in COUNTERS:
        x, y = num(b.get(k)), num(a.get(k))
        out["pg_d_" + k] = (y - x) if x is not None and y is not None else None
    if secs:
        for k in ("xact_commit", "wal_bytes", "wal_records", "stmt_calls"):
            d = out.get("pg_d_" + k)
            out[f"pg_{k}_per_s"] = d / secs if d is not None else None
    hit, read = out.get("pg_d_blks_hit"), out.get("pg_d_blks_read")
    out["pg_buffer_hit_ratio"] = hit / (hit + read) if hit is not None and read is not None and hit + read > 0 else None
    calls, ms = out.get("pg_d_stmt_calls"), out.get("pg_d_stmt_exec_ms")
    out["pg_stmt_mean_ms"] = ms / calls if calls and ms is not None else None
    out["pg_db_size_bytes"] = num(a.get("g_db_size_bytes"))
    out["pg_max_connections"] = num(a.get("g_max_connections"))

    # Database HOST CPU from /proc/stat: busy = total - idle - iowait.
    cb, ca = cpu_fields(bx.get("host_cpu", "")), cpu_fields(ax.get("host_cpu", ""))
    if cb and ca:
        d = [y - x for x, y in zip(cb, ca)]
        total = sum(d[:8])
        if total > 0:
            out["dbhost_cpu_busy_pct"] = 100.0 * (total - d[3] - d[4]) / total
            out["dbhost_cpu_iowait_pct"] = 100.0 * d[4] / total
            out["dbhost_cpu_steal_pct"] = 100.0 * d[7] / total
    for k in ("dbhost_cpu_busy_pct", "dbhost_cpu_iowait_pct", "dbhost_cpu_steal_pct"):
        out.setdefault(k, None)

    # Busiest block device over the window (the data volume under load).
    db_, da = disk_map(bx.get("host_disk", "")), disk_map(ax.get("host_disk", ""))
    best = None
    for dev in set(db_) & set(da):
        r = [y - x for x, y in zip(db_[dev], da[dev])]
        if best is None or r[2] > best[1][2]:
            best = (dev, r)
    if best and secs:
        dev, (rd, srd, wr, swr, io_ms) = best
        out["dbhost_disk_dev"] = dev
        out["dbhost_disk_read_iops"] = rd / secs
        out["dbhost_disk_write_iops"] = wr / secs
        out["dbhost_disk_write_MBps"] = swr * 512 / secs / 1e6
        out["dbhost_disk_util_pct"] = min(100.0, io_ms / (secs * 1000) * 100)
    else:
        for k in ("dbhost_disk_dev", "dbhost_disk_read_iops", "dbhost_disk_write_iops",
                  "dbhost_disk_write_MBps", "dbhost_disk_util_pct"):
            out[k] = None

    # Mid-window gauges: peaks and a wait-event profile.
    peaks, waits, nsamp = {}, {}, 0
    if len(sys.argv) > 3:
        try:
            for line in open(sys.argv[3]).read().splitlines():
                if line.startswith("ts_epoch_ms="):
                    nsamp += 1
                elif line.startswith("g_wait|"):
                    k, _, v = line.partition("=")
                    waits[k[7:]] = waits.get(k[7:], 0) + int(v)
                elif line.startswith("g_") and "=" in line:
                    k, _, v = line.partition("=")
                    if num(v) is not None:
                        peaks[k] = max(peaks.get(k, 0), num(v))
        except OSError:
            pass
    out["pg_samples"] = nsamp
    for k in ("g_conns_total", "g_conns_active", "g_conns_waiting_lock", "g_locks_not_granted"):
        out["pg_peak_" + k[2:]] = peaks.get(k) if nsamp else None
    total_w = sum(waits.values())
    out["pg_wait_profile"] = ";".join(
        f"{k}:{100.0 * v / total_w:.0f}%" for k, v in sorted(waits.items(), key=lambda kv: -kv[1])[:6]
    ) if total_w else None

    for k, v in out.items():
        print(f"{k}={fmt(v)}")


if __name__ == "__main__":
    main()
