#!/usr/bin/env bash
# Resumable shard sweep for durexec.
#
# WHY THIS EXISTS. `cmd/loadgen -mode scaling -sweep 1,2,4` accumulates every
# row in memory and writes the CSV only after the last point finishes
# (main.go:303 -> report.go:59-66). A sweep interrupted at point 4 of 5
# therefore leaves nothing on disk. This runner invokes loadgen ONCE PER SHARD
# COUNT and flushes that point's row before starting the next, so a run killed
# partway leaves usable evidence. The merge path cmd/loadgen/report.go already
# documents -- "from three separate invocations, one per shard count, into a
# single file" -- is exactly this shape.
#
# WHAT THE SWEEP VALUE MEANS. It is NOT the engine's configured shard count.
# The engine runs with a fixed SHARD_COUNT (16 in deploy/docker-compose.yml).
# loadgen's IDGen generates workflow ids that hash onto a chosen subset of
# those shards, so `-sweep 8` means "drive load at 8 of the 16 existing
# shards", not "redeploy the engine with 8 shards". Nothing here redeploys.
#
# These are LOCAL EXPLORATORY measurements. They exist to shape the cloud
# experiment and to validate the harness. No number produced here belongs in
# the study's performance results.
#
# RESUME: re-run the identical command. Completed config_ids are skipped.
#   ./experiments/scripts/shard_sweep.sh
#
# Tunables (environment):
#   SWEEP        shard counts, comma separated      (default 1,2,4,8,16)
#   CONCURRENCY  concurrent in-flight starts        (default 128)
#   DURATION     measurement window                 (default 25s)
#   OUT          output directory                   (default docs/results/local-shard-sweep)

set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
REPO="$PWD"
ENGINE="$REPO/engine"

SWEEP="${SWEEP:-1,2,4,8,16}"
CONCURRENCY="${CONCURRENCY:-128}"
DURATION="${DURATION:-25s}"
OUT="${OUT:-$REPO/docs/results/local-shard-sweep}"
RAW="$OUT/raw"
CSV="$OUT/results.csv"

mkdir -p "$RAW"

GIT_SHA="$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo unknown)"
DIRTY="$(git -C "$REPO" diff --quiet 2>/dev/null && echo clean || echo dirty)"
RUN_ID="${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}"

# The header carries the resume command, so a partial file explains itself.
if [ ! -f "$CSV" ]; then
  {
    echo "# durexec LOCAL EXPLORATORY shard sweep -- NOT cloud results, NOT for the study's performance tables."
    echo "# resume: SWEEP=$SWEEP CONCURRENCY=$CONCURRENCY DURATION=$DURATION ./experiments/scripts/shard_sweep.sh"
    echo "# engine SHARD_COUNT is fixed at 16; the shards column is how many of those 16 were driven."
    echo "# host: $(uname -srm); cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //')"
    echo "run_id,config_id,ts_start,ts_end,git_sha,tree,shards,concurrency,duration,started,completed,wf_per_s,p50_s,p95_s,p99_s,errors,owner_nodes,gen_cpu_pct,eng_cpu_pct,pg_conns,pg_xact_delta,verdict,raw_file"
  } > "$CSV"
fi

already() { grep -q ",$1," "$CSV" 2>/dev/null; }

# Sample the engine containers' CPU while a point runs. `docker stats
# --no-stream` taken once during the run would catch a single instant, which
# says nothing about whether the engine was the limit.
sample_engine_cpu() {
  local out="$1" stop="$2"
  while [ ! -f "$stop" ]; do
    docker stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}' \
      node-a node-b node-c 2>/dev/null >> "$out"
    sleep 5
  done
}

# Returns "conns,xact_commit". A single connection count is a level; the
# commit counter is a counter, so throughput at the database can only be had
# as a difference between two readings around the window.
pg_snapshot() {
  docker exec deploy-postgres-leader-1 psql -U durexec -d durexec -At -F, \
    -c "select (select count(*) from pg_stat_activity where datname='durexec'),
               (select xact_commit from pg_stat_database where datname='durexec')" \
    2>/dev/null | tr -d ' ' || echo ","
}

echo "run_id       : $RUN_ID"
echo "git          : $GIT_SHA ($DIRTY)"
echo "sweep        : $SWEEP   concurrency=$CONCURRENCY duration=$DURATION"
echo "output       : $CSV"
echo

IFS=',' read -ra POINTS <<< "$SWEEP"
for n in "${POINTS[@]}"; do
  CONFIG_ID="shards${n}-c${CONCURRENCY}-d${DURATION}"

  if already "$CONFIG_ID"; then
    echo "== $CONFIG_ID already recorded, skipping"
    continue
  fi

  echo "== $CONFIG_ID starting"
  TS_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  RAWFILE="$RAW/${RUN_ID}-${CONFIG_ID}.txt"
  CPUFILE="$RAW/${RUN_ID}-${CONFIG_ID}.enginecpu"
  TIMEFILE="$RAW/${RUN_ID}-${CONFIG_ID}.time"
  STOPFILE="$RAW/.stop-${CONFIG_ID}"
  rm -f "$STOPFILE"

  PG_BEFORE="$(pg_snapshot)"
  sample_engine_cpu "$CPUFILE" "$STOPFILE" &
  SAMPLER=$!

  # /usr/bin/time gives the GENERATOR's own CPU, which is the signal that
  # separates "the engine is the limit" from "the generator is the limit".
  # Without it a flat curve cannot be attributed, which is the trap that
  # invalidated the concurrency-16 run recorded in docs/results/README.md.
  ( cd "$ENGINE" && /usr/bin/time -f "GEN_CPU_PCT=%P GEN_ELAPSED=%e GEN_MAXRSS_KB=%M" -o "$TIMEFILE" \
      go run ./cmd/loadgen -mode scaling -sweep "$n" \
        -concurrency "$CONCURRENCY" -duration "$DURATION" ) > "$RAWFILE" 2>&1
  EXIT=$?

  touch "$STOPFILE"; wait $SAMPLER 2>/dev/null; rm -f "$STOPFILE"
  TS_END="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  GEN_CPU="$(grep -oE 'GEN_CPU_PCT=[0-9]+' "$TIMEFILE" 2>/dev/null | head -1 | cut -d= -f2)"
  PG_AFTER="$(pg_snapshot)"

  # Parse loadgen's own report. Anything not found stays EMPTY rather than
  # being guessed at -- an invented number is worse than a blank.
  COMPLETED="$(grep -oE 'completed [0-9]+' "$RAWFILE" | tail -1 | awk '{print $2}')"
  WFPS="$(grep -oE '= [0-9.]+ wf/s' "$RAWFILE" | tail -1 | awk '{print $2}')"
  STARTED="$(grep -oE 'started [0-9]+' "$RAWFILE" | tail -1 | awk '{print $2}')"
  # The report prints "latency_p50 @4shard   5.3698 s"; the value is the field
  # after the @Nshard token, not merely the last number on the line.
  # The report mentions "latency_p50 @Nshard" TWICE: once in the results table
  # ("NFR-5  latency_p50 @4shard  5.3698 s") and again in the explanatory notes
  # ("latency_p50 @4shard  end to end, start request to..."). The two have
  # different field offsets, so a positional $N picks the word "end" off the
  # note. Take the FIRST line and the first field on it that is a bare number.
  lat() { awk -v k="latency_p$1 @" '
      index($0,k) && !seen {
        for (i=1;i<=NF;i++) if ($i ~ /^[0-9]+\.?[0-9]*$/) { print $i; seen=1; exit }
      }' "$2"; }
  P50="$(lat 50 "$RAWFILE")"
  P95="$(lat 95 "$RAWFILE")"
  P99="$(lat 99 "$RAWFILE")"
  ERRS="$(grep -oE 'failed [0-9]+' "$RAWFILE" | tail -1 | awk '{print $2}')"
  # "--- throughput: 4 shard(s) [0,2,4,1] owned by [node-b node-c node-a] ---"
  # The previous regex looked for the ratio note's "N vs M distinct owner
  # node(s)", which this runner never produces because each invocation holds
  # only one point and so has no baseline to compare against. That left the
  # column empty for the whole first sweep.
  OWNERS="$(sed -n 's/.*owned by \[\([^]]*\)\].*/\1/p' "$RAWFILE" | tail -1 | wc -w)"
  [ "$OWNERS" = "0" ] && OWNERS=""

  # Engine CPU, reduced from the samples taken during the window. The peak of
  # the per-sample cluster sum is what matters: a mean over a 25s window that
  # includes ramp-up understates the busiest moment, which is the one that
  # decides whether the engine was the limit.
  ENG_CPU="$(awk '{gsub(/%/,"",$2); s[NR_sample]+=0; c[$1]=$2}
                  (NR%3==0){t=0; for(k in c) t+=c[k]; if(t>mx) mx=t}
                  END{if(mx>0) printf "%.0f", mx}' "$CPUFILE" 2>/dev/null)"

  # Database deltas across the window, not an instantaneous reading.
  PG_XACT_DELTA="$(awk -F, -v a="$PG_BEFORE" -v b="$PG_AFTER" \
      'BEGIN{split(a,x,",");split(b,y,","); d=y[2]-x[2]; if(d>=0) print d}' </dev/null)"
  PG_CONNS="$(printf '%s' "$PG_AFTER" | cut -d, -f1)"

  if [ "$EXIT" -ne 0 ]; then
    VERDICT="RUN_FAILED_exit${EXIT}"
  elif [ -z "$WFPS" ]; then
    # A run that produced no parseable throughput is not a zero; it is a run
    # whose result could not be read. Those are different and must not merge.
    VERDICT="UNPARSEABLE"
  else
    VERDICT="RECORDED"
  fi

  printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
    "$RUN_ID" "$CONFIG_ID" "$TS_START" "$TS_END" "$GIT_SHA" "$DIRTY" \
    "$n" "$CONCURRENCY" "$DURATION" "${STARTED:-}" "${COMPLETED:-}" "${WFPS:-}" \
    "${P50:-}" "${P95:-}" "${P99:-}" "${ERRS:-}" "${OWNERS:-}" \
    "${GEN_CPU:-}" "${ENG_CPU:-}" "${PG_CONNS:-}" "${PG_XACT_DELTA:-}" \
    "$VERDICT" "$(basename "$RAWFILE")" >> "$CSV"
  sync "$CSV" 2>/dev/null || true

  echo "   -> ${WFPS:-?} wf/s  p99=${P99:-?}s  gen_cpu=${GEN_CPU:-?}%  eng_cpu=${ENG_CPU:-?}%  owners=${OWNERS:-?}  verdict=$VERDICT"
done

echo
echo "=== recorded so far (recomputed from disk, not from memory) ==="
grep -v '^#' "$CSV" | cut -d, -f2,7,8,12,15,17,18,19,22 | column -s, -t
