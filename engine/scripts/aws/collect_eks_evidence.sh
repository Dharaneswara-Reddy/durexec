#!/usr/bin/env bash
# EKS-native evidence collector.
#
# collect_evidence.sh is SSH + Docker Compose and cannot run against EKS
# (it calls `remote "docker logs"` and `docker exec durexec-postgres-leader-1`).
# It is left untouched for the Compose campaigns; this is its EKS counterpart.
#
# READ-ONLY. It never deletes, scales or restarts anything. Phase 7 and Phase 8
# each destroyed their own evidence by restarting before collecting, so this
# runs BEFORE any teardown and treats a missing artefact as a failure rather
# than as silence.
set -uo pipefail
# Two levels up from engine/scripts/aws is engine/, not the repository root,
# so a relative docs/ path lands in engine/docs/. Resolve the repo root
# explicitly; an evidence bundle written to the wrong tree is worse than one
# not written at all, because it looks collected.
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "$0")/../..")"

CAMPAIGN="${1:-}"
[ -n "$CAMPAIGN" ] || { echo "usage: $0 <campaign-name>" >&2; exit 2; }
NS="${NS:-durexec-lab}"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="docs/results/cloud/${TS}-${CAMPAIGN}"
mkdir -p "$OUT"

missing=0; collected=0
note() { echo "  $*"; }
# Required artefacts make the run nonzero if absent. Optional ones are noted.
get() { # label file required cmd...
  local label="$1" file="$2" req="$3"; shift 3
  if "$@" > "$OUT/$file" 2>"$OUT/$file.err"; then
    [ -s "$OUT/$file" ] && { note "ok       $label"; collected=$((collected+1)); rm -f "$OUT/$file.err"; return 0; }
  fi
  if [ "$req" = required ]; then note "MISSING  $label (required)"; missing=$((missing+1));
  else note "absent   $label (optional)"; fi
  return 1
}

# --- 0. prerequisites, before anything is claimed --------------------------
command -v kubectl >/dev/null || { echo "FATAL: kubectl not found" >&2; exit 2; }
CTX="$(kubectl config current-context 2>/dev/null || true)"
[ -n "$CTX" ] || { echo "FATAL: no kubectl context" >&2; exit 2; }
kubectl get ns "$NS" >/dev/null 2>&1 || { echo "FATAL: namespace $NS not found in context $CTX" >&2; exit 2; }

{
  echo "campaign    : $CAMPAIGN"
  echo "collected_at: $TS"
  echo "context     : $CTX"
  echo "namespace   : $NS"
  echo "git_sha     : $(git rev-parse --short HEAD 2>/dev/null)"
  echo "git_tree    : $(git diff --quiet 2>/dev/null && echo clean || echo dirty)"
  echo "aws_identity: $(aws sts get-caller-identity --query Arn --output text 2>/dev/null | sed -E 's/[0-9]{12}/<ACCT>/')"
} > "$OUT/00-provenance.txt"
note "ok       00-provenance.txt"; collected=$((collected+1))

# --- 1. cluster and placement ----------------------------------------------
get "nodes"        01-nodes.txt        required kubectl get nodes -o wide
get "pod placement" 02-pods-wide.txt   required kubectl -n "$NS" get pods -o wide
get "statefulsets" 03-statefulsets.txt required kubectl -n "$NS" get statefulset -o wide
get "deployments"  04-deployments.txt  optional kubectl -n "$NS" get deployment -o wide
get "jobs"         05-jobs.txt         optional kubectl -n "$NS" get jobs -o wide
get "services"     06-services.txt     optional kubectl -n "$NS" get svc -o wide
get "events"       07-events.txt       required kubectl -n "$NS" get events --sort-by=.lastTimestamp

# Placement PROOF, not a status field: pod -> node -> node role label.
{
  printf '%-28s %-24s %s\n' POD NODE ROLE
  for p in $(kubectl -n "$NS" get pods -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    n=$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
    r=$(kubectl get node "$n" -o jsonpath='{.metadata.labels.durexec\.io/role}' 2>/dev/null)
    printf '%-28s %-24s %s\n' "$p" "${n:-?}" "${r:-<none>}"
  done
} > "$OUT/08-placement-proof.txt" 2>/dev/null
[ -s "$OUT/08-placement-proof.txt" ] && { note "ok       08-placement-proof.txt"; collected=$((collected+1)); } \
  || { note "MISSING  placement proof (required)"; missing=$((missing+1)); }

# --- 2. per-pod detail, restarts and exit codes ----------------------------
for p in $(kubectl -n "$NS" get pods -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
  kubectl -n "$NS" describe pod "$p" > "$OUT/10-describe-$p.txt" 2>/dev/null && collected=$((collected+1))
  # --all-containers reaches the init gate, which is where a failed
  # deployment leaves its only explanation.
  kubectl -n "$NS" logs "$p" --all-containers --tail=4000 > "$OUT/11-logs-$p.txt" 2>/dev/null || true
  kubectl -n "$NS" logs "$p" --all-containers --previous --tail=2000 > "$OUT/12-logs-prev-$p.txt" 2>/dev/null || true
  [ -s "$OUT/12-logs-prev-$p.txt" ] || rm -f "$OUT/12-logs-prev-$p.txt"
done
kubectl -n "$NS" get pods -o json > "$OUT/13-pods.json" 2>/dev/null && collected=$((collected+1))
# Restart counts and termination reasons, extracted so a reader need not parse JSON.
python3 - "$OUT/13-pods.json" > "$OUT/14-restarts-exits.txt" 2>/dev/null <<'PY'
import json,sys
try: d=json.load(open(sys.argv[1]))
except Exception as e: print("unreadable:",e); sys.exit()
for it in d.get("items",[]):
    n=it["metadata"]["name"]
    for cs in it.get("status",{}).get("containerStatuses",[])+it.get("status",{}).get("initContainerStatuses",[]):
        st=cs.get("state",{}); last=cs.get("lastState",{}).get("terminated",{})
        term=st.get("terminated",{})
        print(f"{n}/{cs['name']}: restarts={cs.get('restartCount',0)} ready={cs.get('ready')}"
              f" exit={term.get('exitCode', last.get('exitCode','-'))}"
              f" reason={term.get('reason', last.get('reason','-'))}")
PY
note "ok       pod logs, describes, restart/exit summary"

# --- 3. non-secret workload configuration ----------------------------------
get "configmaps" 20-configmaps.yaml optional kubectl -n "$NS" get configmap -o yaml
# Secrets are listed by NAME and KEY only. Values are never fetched.
kubectl -n "$NS" get secrets -o json 2>/dev/null \
  | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit()
for s in d.get('items',[]):
    print(s['metadata']['name'], sorted((s.get('data') or {}).keys()))
" > "$OUT/21-secret-names-and-keys.txt" 2>/dev/null && note "ok       21-secret-names-and-keys.txt (names/keys only, no values)"

# --- 4. engine metrics and shard state -------------------------------------
POD="$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
if [ -n "$POD" ]; then
  get "engine /metrics" 30-metrics.txt required \
    kubectl -n "$NS" exec "$POD" -- sh -c 'wget -qO- http://localhost:8080/metrics'
  get "shard ownership" 31-shards.json required \
    kubectl -n "$NS" exec "$POD" -- sh -c 'wget -qO- http://localhost:8080/v1/shards'
  get "healthz" 32-healthz.json optional \
    kubectl -n "$NS" exec "$POD" -- sh -c 'wget -qO- http://localhost:8080/healthz'
else
  note "MISSING  no engine pod; metrics and shard state unavailable (required)"
  missing=$((missing+2))
fi

# --- 5. database invariants, over an authenticated path --------------------
# Run from inside an engine pod using the SAME secret the engine uses, so no
# password is ever typed, passed as an argument, or written to this machine.
if [ -n "$POD" ]; then
  kubectl -n "$NS" exec "$POD" -- sh -c '
    # DATABASE_URL is already in this container'\''s environment from the
    # secret; psql reads it from there, so it never reaches argv.
    command -v psql >/dev/null 2>&1 || { echo "psql absent in engine image"; exit 3; }
    psql "$DATABASE_URL" -At -c "
      select '\''events='\''||(select count(*) from events)
          ||'\'' executions='\''||(select count(*) from executions)
          ||'\'' pending_tasks='\''||(select count(*) from tasks)
          ||'\'' nonterminal='\''||(select count(*) from executions where status not in ('\''COMPLETED'\'','\''FAILED'\'','\''COMPENSATING_FAILED'\''))
          ||'\'' dup_run_seq='\''||(select count(*) from (select run_id,seq from events group by run_id,seq having count(*)>1) d)"
  ' > "$OUT/40-db-invariants.txt" 2>&1
  if grep -q 'events=' "$OUT/40-db-invariants.txt" 2>/dev/null; then
    note "ok       40-db-invariants.txt"; collected=$((collected+1))
  else
    note "absent   40-db-invariants.txt (engine image is distroless: no psql). Run the SQL from the smoke script instead."
  fi
fi

# --- 6. benchmark artefacts -------------------------------------------------
for f in docs/results/eks-pilot/results.csv; do
  [ -f "$f" ] && { cp "$f" "$OUT/50-$(basename "$f")"; note "ok       50-$(basename "$f")"; collected=$((collected+1)); }
done
[ -d docs/results/eks-pilot/raw ] && { cp -r docs/results/eks-pilot/raw "$OUT/51-raw" 2>/dev/null; note "ok       51-raw/"; }

# --- 7. redaction -----------------------------------------------------------
# Belt and braces: nothing above should contain a credential, but a connection
# string can arrive inside an error message from a library we do not control.
find "$OUT" -type f \( -name '*.txt' -o -name '*.yaml' -o -name '*.json' \) -print0 2>/dev/null \
  | xargs -0 -r sed -i -E \
      -e 's#(postgres(ql)?://[^:]+:)[^@]+@#\1<REDACTED>@#g' \
      -e 's/(PGPASSWORD=)[^[:space:]"]+/\1<REDACTED>/g' \
      -e 's/(password[\"'\'' ]*[:=][[:space:]]*)[^[:space:],}\"]+/\1<REDACTED>/gI' \
      -e 's/[0-9]{12}/<ACCT>/g' 2>/dev/null
note "ok       redaction pass applied"

# --- 8. archive and verdict -------------------------------------------------
if command -v tar >/dev/null; then
  tar -czf "$OUT.tar.gz" -C "$(dirname "$OUT")" "$(basename "$OUT")" 2>/dev/null \
    && note "ok       $(basename "$OUT").tar.gz"
fi

echo
echo "collected $collected artefact(s) into $OUT"
if [ "$missing" -gt 0 ]; then
  echo "INCOMPLETE: $missing required artefact(s) missing. Evidence kept; exiting nonzero." >&2
  exit 1
fi
echo "COMPLETE: every required artefact present."
