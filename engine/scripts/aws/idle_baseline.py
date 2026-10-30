#!/usr/bin/env python3
"""Summarize one no-load PostgreSQL and engine counter window."""

import sys


def read_kv(path):
    values = {}
    with open(path, encoding="utf-8") as src:
        for line in src:
            key, sep, value = line.strip().partition("=")
            if sep:
                values[key] = value
    return values


def summarize(pg, engine):
    seconds = float(pg["pg_window_s"])
    if seconds <= 0:
        raise ValueError("PostgreSQL counter window must be positive")
    commits = float(pg["pg_d_xact_commit"])
    cpu_seconds = float(engine["d_process_cpu_seconds_total"])
    return {
        "idle_window_s": f"{seconds:.3f}",
        "pg_commits_per_s": f"{commits / seconds:.3f}",
        "dbhost_cpu_busy_pct": pg.get("dbhost_cpu_busy_pct", "unavailable"),
        "engine_cpu_cores_total": f"{cpu_seconds / seconds:.4f}",
        "engine_process_cpu_seconds": f"{cpu_seconds:.3f}",
    }


def self_test():
    values = summarize(
        {"pg_window_s": "100", "pg_d_xact_commit": "47700",
         "dbhost_cpu_busy_pct": "4.25"},
        {"d_process_cpu_seconds_total": "12.5"},
    )
    assert values == {
        "idle_window_s": "100.000",
        "pg_commits_per_s": "477.000",
        "dbhost_cpu_busy_pct": "4.25",
        "engine_cpu_cores_total": "0.1250",
        "engine_process_cpu_seconds": "12.500",
    }
    print("idle baseline reducer self-test: PASS")


def main():
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        self_test()
        return
    if len(sys.argv) != 3:
        raise SystemExit("usage: idle_baseline.py PG_REDUCED ENGINE_REDUCED")
    result = summarize(read_kv(sys.argv[1]), read_kv(sys.argv[2]))
    for key, value in result.items():
        print(f"{key}={value}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, KeyError, ValueError) as exc:
        print(f"idle baseline reducer: {exc}", file=sys.stderr)
        raise SystemExit(1)
