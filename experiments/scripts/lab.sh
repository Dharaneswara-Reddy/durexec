#!/usr/bin/env bash
# durexec cloud-scale lab control script.
#
# Secrets are created here at deploy time, never committed and never baked into
# an image (spec section 14). The password is generated unless DUREXEC_DB_PASSWORD
# is already set in the environment.
#
#   ./lab.sh up        deploy namespace, postgres, engine
#   ./lab.sh status     show pods, shard ownership, leases
#   ./lab.sh down       delete the namespace (everything for this experiment)
#   ./lab.sh psql       open psql against the lab database
set -euo pipefail

NS=durexec-lab
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE="$HERE/../../engine/deploy/kubernetes/base"

secret() {
  local pw="${DUREXEC_DB_PASSWORD:-}"
  if [ -z "$pw" ]; then
    pw="$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | head -c 24)"
    echo "  generated a database password for this deployment" >&2
  fi
  kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$NS" create secret generic durexec-db \
    --from-literal=password="$pw" \
    --from-literal=url="postgres://durexec:${pw}@postgres.${NS}.svc.cluster.local:5432/durexec?sslmode=disable" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

case "${1:-status}" in
  up)
    secret
    kubectl apply -k "$BASE"
    echo "waiting for postgres..."
    kubectl -n "$NS" rollout status statefulset/postgres --timeout=300s
    echo "waiting for engine..."
    kubectl -n "$NS" rollout status statefulset/durexec --timeout=300s
    ;;
  status)
    kubectl -n "$NS" get pods -o wide
    echo
    echo "--- shard ownership (lease table, the source of truth) ---"
    kubectl -n "$NS" exec statefulset/postgres -- psql -U durexec -d durexec -c \
      "select owner_node, count(*) shards, max(fencing_token) tok,
              min(expires_at) > now() as live
       from shard_leases group by owner_node order by owner_node;" 2>/dev/null || true
    echo "--- what each pod says it owns (/healthz, NOT a readiness signal) ---"
    for p in $(kubectl -n "$NS" get pods -l app=durexec -o name 2>/dev/null); do
      printf "%s  " "${p#pod/}"
      kubectl -n "$NS" exec "$p" -- /healthcheck 2>/dev/null >/dev/null \
        && echo "healthz=200" || echo "healthz=FAIL"
    done
    ;;
  psql)
    kubectl -n "$NS" exec -it statefulset/postgres -- psql -U durexec -d durexec
    ;;
  down)
    kubectl delete namespace "$NS" --wait=true
    ;;
  *) echo "usage: lab.sh {up|status|psql|down}"; exit 2 ;;
esac
