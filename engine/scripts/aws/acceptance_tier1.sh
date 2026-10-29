#!/usr/bin/env bash
# Tier 1 cloud acceptance suite. Read-only against AWS (creates no resource),
# but it DOES start workflows and kill containers on the cluster, so it asks
# for confirmation.
#
# Resumable, per the project's rule that a long job must survive the session
# ending. Each case appends one CSV row the moment it finishes, flushed; a run
# killed at case 5 leaves five usable rows. On restart it reads the CSV, skips
# the cases already recorded and continues. The summary is recomputed from the
# file on disk, never from memory, so a partial run still yields a valid
# partial report.
#
# RESUME COMMAND (also written into the CSV header):
#   DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/acceptance_tier1.sh
source "$(dirname "$0")/_common.sh"

require_confirmation "start workflows and kill containers on the AWS cluster"

RESULTS="${DUREXEC_RESULTS:-${ENGINE_DIR}/../docs/results/cloud-tier1.csv}"
mkdir -p "$(dirname "$RESULTS")"

# PrepTime is set on EVERY workflow this suite starts, and it is not a
# cosmetic choice. app/delivery/workflow.go sets defaultPrepTime to TWELVE
# MINUTES. In Phase 8 a leftover workflow sitting on that timer fired while a
# crash point was armed, killed both engine nodes and destroyed the run. A
# short, explicit prep time means no timer from this suite outlives it.
PREP_NS=2000000000   # 2s

if [ ! -f "$RESULTS" ]; then
  {
    echo "# durexec AWS Tier 1 acceptance results"
    echo "# resume: DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/acceptance_tier1.sh"
    echo "# instance: $(tf_output instance_id)   ami: $(tf_output ami_id)"
    echo "# started: $(date -Is)"
    echo "case,status,detail,at"
  } > "$RESULTS"
fi

already() { grep -q "^$1," "$RESULTS" 2>/dev/null; }

record() {
  # One row, appended and flushed immediately.
  printf '%s,%s,"%s",%s\n' "$1" "$2" "${3//\"/\'}" "$(date -Is)" >> "$RESULTS"
  sync 2>/dev/null || true
  printf '  -> %s %s: %s\n' "$1" "$2" "$3"
}

# Any node will do: a node that does not own the shard FORWARDS the request
# (api.Deps.PeerAddr), so the caller never needs to know who owns what.
api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    remote "curl -sS -m 20 -X $method 'http://localhost:8081$path' -H 'Content-Type: application/json' -d '$body'"
  else
    remote "curl -sS -m 20 -X $method 'http://localhost:8081$path'"
  fi
}

order_body() {
  cat <<B
{"workflow_id":"$1","workflow_type":"DeliveryOrder","input":{"order_id":"$1","customer_id":"cust-1","restaurant_id":"rest-1","items":[{"sku":"dish-1","quantity":1}],"amount_cents":1250,"currency":"INR","prep_time_ns":$PREP_NS}}
B
}

wait_terminal() {
  local id="$1" tries="${2:-40}"
  for _ in $(seq 1 "$tries"); do
    local st
    st=$(api GET "/v1/workflows/$id" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")' 2>/dev/null)
    case "$st" in
      COMPLETED|FAILED|COMPENSATING_FAILED) echo "$st"; return 0 ;;
    esac
    sleep 3
  done
  echo "TIMEOUT"
  return 1
}

echo "results: $RESULTS"
echo

# ---- T1.1 truthful health -------------------------------------------------
if already T1.1; then echo "T1.1 already recorded, skipping"; else
  echo "T1.1 engine starts and reports truthful health"
  # /v1/shards is authoritative: it lists every lease with its owner and
  # fencing token. /healthz only gives a COUNT, which cannot distinguish
  # "16 shards owned once each" from "two nodes both claiming shard 3".
  out=$(api GET /v1/shards | python3 -c '
import json,sys
d=json.load(sys.stdin)
sh=d.get("shards",[])
owners={}
unowned=[s["shard_id"] for s in sh if not s.get("owner")]
expired=[s["shard_id"] for s in sh if s.get("expired")]
for s in sh: owners.setdefault(s.get("owner"),[]).append(s["shard_id"])
print(f"shards={len(sh)} unowned={len(unowned)} expired={len(expired)} per_owner=" +
      ";".join(f"{k}:{len(v)}" for k,v in sorted(owners.items(), key=lambda x:(x[0] or ""))))
sys.exit(0 if len(sh)==16 and not unowned and not expired else 1)' 2>&1) \
    && record T1.1 PASS "$out" || record T1.1 FAIL "$out"
fi

# ---- T1.2 durable start, resume, complete ---------------------------------
if already T1.2; then echo "T1.2 already recorded, skipping"; else
  echo "T1.2 a workflow is started, durably recorded and completed"
  id="acc-t12-$(date +%s)"
  start=$(api POST /v1/workflows "$(order_body "$id")")
  st=$(wait_terminal "$id")
  hist=$(api GET "/v1/workflows/$id/history" | python3 -c 'import json,sys
try: print(len(json.load(sys.stdin).get("events",[])))
except Exception: print(0)')
  if [ "$st" = COMPLETED ] && [ "${hist:-0}" -gt 0 ]; then
    record T1.2 PASS "status=$st history_events=$hist"
  else
    record T1.2 FAIL "status=$st history_events=$hist start=$start"
  fi
fi

# ---- T1.3 duplicate starts ------------------------------------------------
if already T1.3; then echo "T1.3 already recorded, skipping"; else
  echo "T1.3 duplicate workflow starts behave correctly"
  # The duplicate is reported as SUCCESS with already_open set, not as an
  # error. Phase 6 instrumented this wrongly at first and counted a duplicate
  # as a second acceptance, so the check is on run_id identity: the second
  # start must return the SAME run, not a new one.
  id="acc-t13-$(date +%s)"
  a=$(api POST /v1/workflows "$(order_body "$id")")
  b=$(api POST /v1/workflows "$(order_body "$id")")
  out=$(python3 - "$a" "$b" <<'PY'
import json,sys
try:
    a=json.loads(sys.argv[1]); b=json.loads(sys.argv[2])
except Exception as e:
    print("unparseable:",e); sys.exit(1)
ra,rb=a.get("run_id"),b.get("run_id")
print(f"run_a={ra} run_b={rb} already_open={b.get('already_open')}")
sys.exit(0 if ra and ra==rb else 1)
PY
) && record T1.3 PASS "$out" || record T1.3 FAIL "$out"
  wait_terminal "$id" >/dev/null || true
fi

# ---- T1.4 concurrency across nodes ----------------------------------------
if already T1.4; then echo "T1.4 already recorded, skipping"; else
  echo "T1.4 concurrent workflows execute correctly across nodes"
  base="acc-t14-$(date +%s)"
  # Started through all three nodes so forwarding is exercised too.
  for i in $(seq 1 12); do
    port=$((8081 + (i % 3)))
    remote "curl -sS -m 20 -o /dev/null -X POST 'http://localhost:$port/v1/workflows' -H 'Content-Type: application/json' -d '$(order_body "$base-$i" | tr -d '\n')'" || true
  done
  done_n=0
  for i in $(seq 1 12); do
    [ "$(wait_terminal "$base-$i" 40)" = COMPLETED ] && done_n=$((done_n+1))
  done
  spread=$(api GET /v1/workflows | python3 -c '
import json,sys
d=json.load(sys.stdin)
ws=[w for w in d.get("workflows",[]) if w.get("workflow_id","").startswith("'"$base"'")]
sh=sorted({w.get("shard_id") for w in ws})
print(f"completed_shards={len(sh)} shards={sh}")' 2>/dev/null || echo "spread unknown")
  if [ "$done_n" -eq 12 ]; then record T1.4 PASS "12/12 completed; $spread"
  else record T1.4 FAIL "only $done_n/12 completed; $spread"; fi
fi

# ---- T1.5 engine process death and recovery -------------------------------
if already T1.5; then echo "T1.5 already recorded, skipping"; else
  echo "T1.5 engine process death recovers with no duplicate journal entries"
  base="acc-t15-$(date +%s)"
  for i in $(seq 1 6); do
    remote "curl -sS -m 20 -o /dev/null -X POST 'http://localhost:8081/v1/workflows' -H 'Content-Type: application/json' -d '$(order_body "$base-$i" | tr -d '\n')'" || true
  done
  # SIGKILL, not stop. A graceful stop lets the node drain and release its
  # leases, which is the opposite of the failure being tested: the point is a
  # node that dies holding leases, so another node has to wait out the TTL.
  echo "  killing node-b with SIGKILL"
  remote 'docker kill --signal=KILL node-b' >/dev/null 2>&1 || true
  killed_at=$(date -Is)
  # LEASE_TTL is 10s, so ownership cannot move before then.
  sleep 25
  recovered=0
  for i in $(seq 1 6); do
    [ "$(wait_terminal "$base-$i" 60)" = COMPLETED ] && recovered=$((recovered+1))
  done
  dups=$(remote bash -s <<'REMOTE'
set -a; . /opt/durexec/.env; set +a
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  psql -U durexec -d durexec -At -c \
  "select count(*) from (select run_id, seq from events group by run_id, seq having count(*)>1) d"
REMOTE
)
  echo "  restarting node-b"
  remote 'cd /opt/durexec/deploy/aws && set -a && . /opt/durexec/.env && set +a && docker compose -f docker-compose.cloud.yml up -d node-b' >/dev/null 2>&1 || true
  if [ "$recovered" -eq 6 ] && [ "${dups:-x}" = "0" ]; then
    record T1.5 PASS "killed node-b at $killed_at; 6/6 completed after failover; duplicate (run_id,seq) groups=$dups"
  else
    record T1.5 FAIL "recovered=$recovered/6 duplicates=$dups (killed at $killed_at)"
  fi
fi

# ---- T1.6 shard ownership moves, fencing ----------------------------------
if already T1.6; then echo "T1.6 already recorded, skipping"; else
  echo "T1.6 shard ownership changes and fencing tokens behave correctly"
  before=$(api GET /v1/shards)
  remote 'docker kill --signal=KILL node-c' >/dev/null 2>&1 || true
  sleep 25
  after=$(api GET /v1/shards)
  out=$(python3 - "$before" "$after" <<'PY'
import json,sys
b=json.loads(sys.argv[1]); a=json.loads(sys.argv[2])
bb={s["shard_id"]:s for s in b.get("shards",[])}
aa={s["shard_id"]:s for s in a.get("shards",[])}
moved=[s for s in aa if bb.get(s,{}).get("owner")!=aa[s].get("owner")]
# A fencing token must never go backwards. It is the thing that lets the
# writing transaction reject a stale owner, so a decreasing token would mean
# a stale write could be accepted.
regressed=[s for s in aa if aa[s].get("token",0) < bb.get(s,{}).get("token",0)]
bumped=[s for s in moved if aa[s].get("token",0) > bb.get(s,{}).get("token",0)]
unowned=[s for s,v in aa.items() if not v.get("owner")]
print(f"moved={len(moved)} token_bumped={len(bumped)} token_regressed={len(regressed)} unowned_after={len(unowned)}")
sys.exit(0 if moved and not regressed and len(bumped)==len(moved) and not unowned else 1)
PY
) && record T1.6 PASS "$out" || record T1.6 FAIL "$out"
  remote 'cd /opt/durexec/deploy/aws && set -a && . /opt/durexec/.env && set +a && docker compose -f docker-compose.cloud.yml up -d node-c' >/dev/null 2>&1 || true
  sleep 20
fi

# ---- T1.7 PostgreSQL restart and reconnection -----------------------------
if already T1.7; then echo "T1.7 already recorded, skipping"; else
  echo "T1.7 PostgreSQL restart and engine reconnection"
  remote 'docker restart durexec-postgres-leader-1' >/dev/null 2>&1 || true
  sleep 30
  id="acc-t17-$(date +%s)"
  api POST /v1/workflows "$(order_body "$id")" >/dev/null 2>&1 || true
  st=$(wait_terminal "$id" 60)
  if [ "$st" = COMPLETED ]; then record T1.7 PASS "a workflow started and completed after a database restart (status=$st)"
  else record T1.7 FAIL "status=$st after a database restart"; fi
fi

# ---- T1.8 no unexplained leftover work ------------------------------------
if already T1.8; then echo "T1.8 already recorded, skipping"; else
  echo "T1.8 no unexplained pending tasks or non-terminal workflows"
  sleep 15
  out=$(remote bash -s <<'REMOTE'
set -a; . /opt/durexec/.env; set +a
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  psql -U durexec -d durexec -At -F'|' -c "
  select
    (select count(*) from executions where status not in ('COMPLETED','FAILED','COMPENSATING_FAILED')),
    (select count(*) from tasks),
    (select count(*) from timers),
    (select count(*) from (select run_id, seq from events group by run_id, seq having count(*)>1) d)"
REMOTE
)
  IFS='|' read -r nonterm tasks timers dups <<< "$(echo "$out" | tr -d ' \r')"
  if [ "${nonterm:-x}" = "0" ] && [ "${tasks:-x}" = "0" ] && [ "${dups:-x}" = "0" ]; then
    record T1.8 PASS "nonterminal=$nonterm tasks=$tasks timers=$timers duplicates=$dups"
  else
    record T1.8 FAIL "nonterminal=$nonterm tasks=$tasks timers=$timers duplicates=$dups"
  fi
fi

# ---- summary, recomputed FROM THE FILE ON DISK ----------------------------
echo
echo "=== summary (recomputed from $RESULTS) ==="
python3 - "$RESULTS" <<'PY'
import csv,sys,collections
rows=[r for r in csv.DictReader(l for l in open(sys.argv[1]) if not l.startswith('#')) if r.get('case')]
c=collections.Counter(r['status'] for r in rows)
for r in rows: print(f"  {r['case']:<6} {r['status']:<6} {r['detail']}")
print()
print("  " + "  ".join(f"{k}={v}" for k,v in sorted(c.items())))
print(f"  cases recorded: {len(rows)} of 8")
if len(rows) < 8: print("  PARTIAL RUN -- rerun the same command to continue from here.")
PY
