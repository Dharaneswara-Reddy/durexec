#!/usr/bin/env bash
# perf-sweep.sh — offered-load sweep against the Kubernetes lab.
#
# One row per (shards, rate, trial). Each row carries BOTH the generator's own
# view (offered rate, accepted, completed, latency percentiles, generator lag,
# saturation verdict) and the infrastructure's view for the same window
# (PostgreSQL transactions, WAL, buffers, connections, container CPU and
# memory, engine pool pressure). They are joined on the window, so a row can
# never pair one run's latency with another run's database load.
#
# RESUME: re-run the exact same command line. Completed (shards,rate,trial)
# rows are read back from the CSV on disk and skipped. A run killed at trial 3
# leaves 3 usable rows and continues from 4.
#
# Deliberately NOT done here: no summary is computed in memory. summarise.py
# recomputes every aggregate from the CSV on disk, so a partial run still
# yields a valid partial report.
set -uo pipefail

IP="$(minikube ip 2>/dev/null)"
PROM="${PROM:-http://$IP:30090}"
NS=durexec-lab
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

OUT="${OUT:-$ROOT/docs/results/phase6-sweep.csv}"
LGCSV="${LGCSV:-$ROOT/docs/results/phase6-loadgen.csv}"
LOADGEN="${LOADGEN:?set LOADGEN to the built loadgen binary}"

SHARDS="${SHARDS:-1}"
RATES="${RATES:-2,4,8,12,16,24}"
TRIALS="${TRIALS:-1}"
DURATION="${DURATION:-60s}"
WARMUP="${WARMUP:-10s}"
GRACE="${GRACE:-40s}"
PREP="${PREP:-50ms}"
FLOOR="${FLOOR:-2.01s}"
TAG="${TAG:-baseline}"
# ARRIVAL_WORKERS caps the generator's concurrent in-flight starts. Left at 0
# the generator derives it from the rate and the declared service floor, which
# is right for a healthy engine but starves the generator once the engine is
# past its knee: in-flight requests stop returning, every worker is occupied,
# and arrivals are DROPPED. The run is then reported GENERATOR_BOUND and is no
# longer a measurement of the engine. Setting it high enough that drops stay
# near zero is what turns a point above the knee back into a valid
# observation of the engine rather than of the generator.
ARRIVAL_WORKERS="${ARRIVAL_WORKERS:-0}"
# CLIENT_POLL is the generator's completion-polling interval. It is exposed
# because it is a real contributor to measured database load: each in-flight
# workflow is polled over the API, so at 190 in-flight and 100ms the CLIENT
# alone issues ~1900 status reads per second against the same database.
CLIENT_POLL="${CLIENT_POLL:-100ms}"

# Go durations to whole seconds, for the window arithmetic.
secs() { python3 -c "
import re,sys
t=sys.argv[1]; m=re.fullmatch(r'(\d+(?:\.\d+)?)(ms|s|m|h)', t)
if not m: print(0); raise SystemExit
v=float(m.group(1)); u=m.group(2)
print(int(v*{'ms':0.001,'s':1,'m':60,'h':3600}[u]))" "$1"; }
WARMUP_S="$(secs "$WARMUP")"
DURATION_S="$(secs "$DURATION")"

SHA="$(cd "$ROOT" && git rev-parse HEAD)"
IMG="$(kubectl get pod durexec-0 -n $NS -o jsonpath='{.spec.containers[0].image}' 2>/dev/null)"

mkdir -p "$(dirname "$OUT")"

# ---- the metric keys recorded, in a fixed order -------------------------
KEYS="pg_xact_commit pg_xact_rollback pg_wal_bytes pg_wal_records pg_wal_fpi \
pg_ckpt_timed pg_ckpt_req pg_blks_hit pg_blks_read pg_stmt_calls pg_stmt_exec_ms \
pg_conns_peak pg_conns_max_cfg pg_db_size_bytes pg_deadlocks \
pool_acquires pool_empty pool_wait_s pool_conns_peak \
wf_starts_accepted wf_starts_already wf_starts_refused wf_starts_notowned wf_starts_invalid \
wf_completed task_failures fencing_rejections lease_acquired lease_lost \
replay_sum_s replay_count queue_age_peak_s \
cpu_pg_core_s cpu_engine_core_s cpu_prom_core_s cpu_pgexp_core_s \
mem_pg_bytes mem_engine_bytes mem_prom_bytes mem_pgexp_bytes \
fs_write_pg_bytes cadvisor_engine_series cadvisor_cpu_samples prom_series prom_scrape_p99_s"

if [ ! -f "$OUT" ]; then
  {
    echo "# durexec Phase 6 offered-load sweep."
    echo "# commit=$SHA image=$IMG host=2vCPU/3.7GiB-minikube"
    echo "# RESUME: OUT=$OUT LOADGEN=$LOADGEN SHARDS=$SHARDS RATES=$RATES TRIALS=$TRIALS DURATION=$DURATION ARRIVAL_WORKERS=$ARRIVAL_WORKERS CLIENT_POLL=$CLIENT_POLL TAG=$TAG $HERE/perf-sweep.sh"
    echo "# Absolute throughput on this host is NOT a production capacity estimate."
    printf 'tag,shards,rate,trial,start_epoch,end_epoch,window_s,lg_verdict,lg_offered,lg_completed_per_s,lg_p50_s,lg_p95_s,lg_p99_s,lg_lag_p99_s,lg_accounting,lg_inflight_slope'
    for k in $KEYS; do printf ',%s' "$k"; done
    printf '\n'
  } > "$OUT"
  sync
fi

done_row() { grep -q "^$TAG,$1,$2,$3," "$OUT" 2>/dev/null; }

# ---- preconditions ------------------------------------------------------
# Both are refusals to measure, not warnings. A trial run against a cluster
# that is not ready, or while a replaced pod's cgroup is still reported,
# produces a row that looks exactly like a valid one.
preflight() {
  local ready
  ready="$(kubectl get pods -n $NS -l app=durexec \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready}{"\n"}{end}' 2>/dev/null | grep -c true)"
  if [ "$ready" != "2" ]; then echo "PREFLIGHT: only $ready/2 engine pods ready" >&2; return 1; fi
  # The stale-cgroup check needs Prometheus, which does not exist in the
  # monitoring-disabled arm of the overhead experiment. Skipping it there is
  # sound because that arm's figures come from the generator alone and no
  # cAdvisor gauge is read; it must never be set for a normal run, so it is an
  # explicit opt-out rather than a fallback that triggers on a failed query.
  if [ "${SKIP_PROM_PREFLIGHT:-0}" = "1" ]; then return 0; fi
  local series
  series="$(curl -sS -X POST "$PROM/api/v1/query" \
    --data-urlencode 'query=count(container_memory_working_set_bytes{pod=~"durexec-[0-9]+"})' \
    | python3 -c 'import json,sys
r=json.load(sys.stdin)["data"]["result"]
print(int(float(r[0]["value"][1])) if r else -1)')"
  if [ "$series" != "2" ]; then
    echo "PREFLIGHT: cadvisor reports $series engine series, want 2 (stale cgroups not yet aged out)" >&2
    return 1
  fi
  return 0
}

IFS=',' read -ra SH_LIST <<< "$SHARDS"
IFS=',' read -ra RATE_LIST <<< "$RATES"

for sh in "${SH_LIST[@]}"; do
for rate in "${RATE_LIST[@]}"; do
for t in $(seq 1 "$TRIALS"); do
  if done_row "$sh" "$rate" "$t"; then
    echo "skip shards=$sh rate=$rate trial=$t (already on disk)"; continue
  fi

  tries=0
  until preflight; do
    tries=$((tries+1))
    if [ "$tries" -ge 20 ]; then
      echo "$TAG,$sh,$rate,$t,,,,INVALID_preflight_never_passed" >> "$OUT"; sync
      echo "recorded INVALID_preflight_never_passed for shards=$sh rate=$rate trial=$t" >&2
      continue 2
    fi
    sleep 15
  done

  echo "=== shards=$sh rate=$rate trial=$t ==="
  start=$(date +%s)
  "$LOADGEN" -mode arrival -rates "$rate" -shards "$sh" \
    -duration "$DURATION" -warmup "$WARMUP" -grace "$GRACE" \
    -prep "$PREP" -service-floor "$FLOOR" \
    -arrival-workers "$ARRIVAL_WORKERS" -poll "$CLIENT_POLL" \
    -base "http://$IP:30081,http://$IP:30082" \
    -node "durexec-0.durexec-hl=http://$IP:30081" \
    -node "durexec-1.durexec-hl=http://$IP:30082" \
    -csv "$LGCSV" > "/tmp/lg-$sh-$rate-$t.out" 2>&1
  end=$(date +%s)

  # The generator's own numbers, read from the CSV IT wrote (authoritative),
  # not scraped from its prose output.
  lg="$(python3 - "$LGCSV" "$sh" "$rate" <<'PY'
import csv,sys
path,shards,rate=sys.argv[1],sys.argv[2],sys.argv[3]
# loadgen labels each metric "<name> @<n>shard@<r>rps"; float formatting of the
# rate differs (2 vs 2.0), so match on the numeric value rather than the text.
want=f"@{shards}shard@"
rows=[]
try:
    with open(path) as f:
        for r in csv.DictReader(x for x in f if not x.startswith("#")):
            rows.append(r)
except Exception:
    pass
def pick(prefix):
    for r in reversed(rows):
        m=r.get("metric","")
        if m.startswith(prefix) and want in m:
            try:
                if abs(float(m.split("@")[-1].rstrip("rps"))-float(rate))>1e-6: continue
            except Exception: continue
            return r.get("value","")
    return ""
out=[pick("saturation_verdict"),pick("offered_per_sec"),pick("completed_per_sec"),
     pick("latency_p50"),pick("latency_p95"),pick("latency_p99"),
     pick("generator_lag_p99"),pick("accounting"),pick("in_flight_slope")]
print("|".join(x.replace(",",";") for x in out))
PY
)"
  IFS='|' read -r v_verdict v_off v_cps v_p50 v_p95 v_p99 v_lag v_acct v_slope <<< "$lg"

  # Infrastructure view for the generator's MEASUREMENT window, not for the
  # whole process lifetime. The generator spends `warmup` settling and up to
  # `grace` draining, and both are idle-ish; averaging database load over them
  # would understate the load at the offered rate and make every point look
  # cheaper than it was. The window is therefore [start+warmup, +duration].
  #
  # It is widened to a 60 s minimum because this kubelet's cAdvisor refreshes
  # container stats only about every 15 s: a shorter window can contain fewer
  # than two points per series, and increase() then yields nothing. Widening
  # is recorded in window_s so a reader can see which rows were stretched.
  w_start=$((start + WARMUP_S))
  w_end=$((w_start + DURATION_S))
  [ "$w_end" -gt "$end" ] && w_end=$end
  if [ $((w_end - w_start)) -lt 60 ]; then w_end=$((w_start + 60)); fi
  [ "$w_end" -gt "$end" ] && w_end=$end
  declare -A M=()
  while IFS='=' read -r k v; do [ -n "${k:-}" ] && M["$k"]="$v"; done < <("$HERE/perf_collect.py" "$PROM" "$w_start" "$w_end" 2>/dev/null)

  printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s' \
    "$TAG" "$sh" "$rate" "$t" "$w_start" "$w_end" "$((w_end-w_start))" \
    "$v_verdict" "$v_off" "$v_cps" "$v_p50" "$v_p95" "$v_p99" "$v_lag" "$v_acct" "$v_slope" >> "$OUT"
  for k in $KEYS; do printf ',%s' "${M[$k]:-}" >> "$OUT"; done
  printf '\n' >> "$OUT"
  sync
  echo "  recorded: verdict=$v_verdict completed/s=$v_cps p50=$v_p50 p99=$v_p99"
done; done; done
echo "sweep complete: $OUT"
