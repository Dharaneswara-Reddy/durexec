#!/usr/bin/env python3
"""Collect one measurement window's infrastructure metrics from Prometheus.

Usage: perf_collect.py PROM_URL START_EPOCH END_EPOCH

Prints `key=value` lines on stdout. A metric that cannot be collected prints
no line at all rather than a zero, because a missing observation and a
measured zero are different facts and the report has to be able to tell them
apart. The caller writes an empty CSV cell for a key that never appeared.

Why increase() evaluated at the window end, rather than reading the counter
before and after: increase() is reset-aware. If an engine pod restarts
mid-window its counters go back to zero, and a naive after-minus-before would
report a large negative number or, worse, a plausible small one.

Why max_over_time for gauges: a gauge sampled once at the end of a run tells
you what the system looked like after it had finished and drained, which is
the least interesting instant. Peak occupancy is what a limit applies to.
"""
import json
import sys
import urllib.parse
import urllib.request

PROM, START, END = sys.argv[1], float(sys.argv[2]), float(sys.argv[3])
W = max(int(END - START), 1)          # window length in whole seconds
WS = f"{W}s"

# Each entry: output key -> PromQL. {W} is the window.
# Counters use increase(); gauges use max_over_time() or avg_over_time().
QUERIES = {
    # ---- PostgreSQL transactions and WAL -------------------------------
    "pg_xact_commit":    'sum(increase(pg_stat_database_xact_commit{{datname="durexec"}}[{W}]))',
    "pg_xact_rollback":  'sum(increase(pg_stat_database_xact_rollback{{datname="durexec"}}[{W}]))',
    "pg_wal_bytes":      'increase(pg_stat_wal_wal_bytes[{W}])',
    "pg_wal_records":    'increase(pg_stat_wal_wal_records[{W}])',
    "pg_wal_fpi":        'increase(pg_stat_wal_wal_fpi[{W}])',
    "pg_ckpt_timed":     'increase(pg_stat_bgwriter_checkpoints_timed_total[{W}])',
    "pg_ckpt_req":       'increase(pg_stat_bgwriter_checkpoints_req_total[{W}])',
    # ---- PostgreSQL buffer cache ---------------------------------------
    "pg_blks_hit":       'sum(increase(pg_stat_database_blks_hit{{datname="durexec"}}[{W}]))',
    "pg_blks_read":      'sum(increase(pg_stat_database_blks_read{{datname="durexec"}}[{W}]))',
    # ---- PostgreSQL statement cost -------------------------------------
    "pg_stmt_calls":     'increase(pg_statements_summary_calls[{W}])',
    "pg_stmt_exec_ms":   'increase(pg_statements_summary_total_exec_time_ms[{W}])',
    # ---- PostgreSQL connections (peak, not final) ----------------------
    "pg_conns_peak":     'max_over_time(sum(pg_stat_activity_count{{datname="durexec"}})[{W}:5s])',
    "pg_conns_max_cfg":  'pg_settings_max_connections',
    "pg_db_size_bytes":  'pg_database_size_bytes{{datname="durexec"}}',
    "pg_deadlocks":      'sum(increase(pg_stat_database_deadlocks{{datname="durexec"}}[{W}]))',
    # ---- engine connection pool ----------------------------------------
    "pool_acquires":     'sum(increase(db_pool_acquires_total[{W}]))',
    "pool_empty":        'sum(increase(db_pool_empty_acquires_total[{W}]))',
    "pool_wait_s":       'sum(increase(db_pool_acquire_wait_seconds_total[{W}]))',
    "pool_conns_peak":   'max_over_time(sum(db_pool_conns_acquired)[{W}:5s])',
    # ---- engine logic ---------------------------------------------------
    "wf_starts_accepted":    'sum(increase(workflow_starts_total{{outcome="accepted"}}[{W}]))',
    "wf_starts_already":     'sum(increase(workflow_starts_total{{outcome="already_open"}}[{W}]))',
    "wf_starts_refused":     'sum(increase(workflow_starts_total{{outcome="refused"}}[{W}]))',
    "wf_starts_notowned":    'sum(increase(workflow_starts_total{{outcome="not_owned"}}[{W}]))',
    "wf_starts_invalid":     'sum(increase(workflow_starts_total{{outcome="invalid"}}[{W}]))',
    "wf_completed":          'sum(increase(workflow_completed_total[{W}]))',
    "task_failures":         'sum(increase(worker_task_failures_total[{W}]))',
    "fencing_rejections":    'sum(increase(fencing_rejections_total[{W}]))',
    "lease_acquired":        'sum(increase(lease_transitions_total{{direction="acquired"}}[{W}]))',
    "lease_lost":            'sum(increase(lease_transitions_total{{direction="lost"}}[{W}]))',
    "replay_sum_s":          'sum(increase(replay_duration_seconds_sum[{W}]))',
    "replay_count":          'sum(increase(replay_duration_seconds_count[{W}]))',
    "queue_age_peak_s":      'max_over_time(max(task_queue_oldest_ready_seconds)[{W}:5s])',
    # ---- containers (cAdvisor via the kubelet) -------------------------
    # NOTE on the absent container label. This kubelet's cAdvisor exposes only
    # POD-level cgroup series: /api/v1/label/container/values is empty, so
    # there is no container="engine" to select on. Every figure below is
    # therefore per POD, which for these pods is nearly the same thing (one
    # application container each) but is not identical: the pod cgroup also
    # contains the pause container. Reported as pod-level rather than
    # relabelled to look like per-container data.
    #
    # NOTE on stale series. A pod that has been replaced keeps its cgroup
    # series until Prometheus ages it out, so one pod NAME can have two
    # series with different pod UIDs. For counters this is harmless —
    # increase() over a terminated pod's counter is 0 — but summing a GAUGE
    # across them double-counts. The memory queries therefore collapse to one
    # value per pod with `max by (pod)` before summing. perf-sweep.sh also
    # refuses to start a trial while any pod name has more than one series.
    "cpu_pg_core_s":     'sum(increase(container_cpu_usage_seconds_total{{pod="postgres-0"}}[{W}]))',
    "cpu_engine_core_s": 'sum(increase(container_cpu_usage_seconds_total{{pod=~"durexec-[0-9]+"}}[{W}]))',
    "cpu_prom_core_s":   'sum(increase(container_cpu_usage_seconds_total{{pod=~"prometheus-.*"}}[{W}]))',
    "cpu_pgexp_core_s":  'sum(increase(container_cpu_usage_seconds_total{{pod=~"postgres-exporter-.*"}}[{W}]))',
    "mem_pg_bytes":      'sum(max by (pod) (max_over_time(container_memory_working_set_bytes{{pod="postgres-0"}}[{W}])))',
    "mem_engine_bytes":  'sum(max by (pod) (max_over_time(container_memory_working_set_bytes{{pod=~"durexec-[0-9]+"}}[{W}])))',
    "mem_prom_bytes":    'sum(max by (pod) (max_over_time(container_memory_working_set_bytes{{pod=~"prometheus-.*"}}[{W}])))',
    "mem_pgexp_bytes":   'sum(max by (pod) (max_over_time(container_memory_working_set_bytes{{pod=~"postgres-exporter-.*"}}[{W}])))',
    # The staleness guard, recorded in every row so a reader can see whether
    # it held: 2 engine pods means 2 series. Anything higher means stale
    # cgroups were present and the memory figures are upper bounds.
    "cadvisor_engine_series": 'count(container_memory_working_set_bytes{{pod=~"durexec-[0-9]+"}})',
    "fs_write_pg_bytes": 'sum(increase(container_fs_writes_bytes_total{{pod="postgres-0"}}[{W}]))',
    # ---- observer cost --------------------------------------------------
    # How many distinct cAdvisor samples the CPU figures were computed from.
    # This kubelet refreshes container cgroup stats about every 15 s, so a
    # 60 s window yields only ~4 points per pod. A row showing 1 or 2 here
    # means the CPU columns are based on too little data to trust, and
    # increase() may legitimately have returned nothing at all.
    "cadvisor_cpu_samples": 'min(count_over_time(container_cpu_usage_seconds_total{{pod=~"durexec-[0-9]+"}}[{W}]))',
    "prom_series":        'prometheus_tsdb_head_series',
    "prom_scrape_p99_s":  'max_over_time(scrape_duration_seconds[{W}])',
}


def query(promql, at):
    body = urllib.parse.urlencode({"query": promql, "time": f"{at:.3f}"}).encode()
    req = urllib.request.Request(PROM + "/api/v1/query", data=body)
    with urllib.request.urlopen(req, timeout=20) as r:
        d = json.load(r)
    if d.get("status") != "success":
        return None
    res = d["data"]["result"]
    if not res:
        return None
    # Several queries are already aggregated to one series; for any that are
    # not, sum the samples rather than silently reporting the first one.
    total = 0.0
    for s in res:
        try:
            total += float(s["value"][1])
        except (TypeError, ValueError):
            return None
    return total


for key, tmpl in QUERIES.items():
    try:
        v = query(tmpl.format(W=WS), END)
    except Exception:
        v = None
    if v is not None:
        print(f"{key}={v:.6f}")
