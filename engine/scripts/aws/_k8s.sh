# shellcheck shell=bash
# Shared helpers for reaching the engine and the database on EKS WITHOUT a
# shell inside the engine container.
#
# The engine image is gcr.io/distroless/static-debian12, which has no /bin/sh,
# no wget and no psql. Every `kubectl exec <engine> -- sh -c '...'` therefore
# fails with `exec: "sh": executable file not found in $PATH` (verified
# against the base image, 2026-10-30). The earlier smoke and evidence scripts
# did exactly that, so on the first paid run they would have reported a
# healthy cluster as broken and triggered the teardown stop condition.
#
# Instead:
#   * HTTP to an engine pod goes through the API server's pod proxy. On EKS the
#     managed node groups share the cluster security group, so the control
#     plane can reach pod port 8080.
#   * SQL goes through a probe pod on the HARNESS node that runs the same
#     pgvector/pgvector:pg16 image the init gate already pulls, and that reads
#     DATABASE_URL from the same Secret the engine uses. No credential is
#     typed, passed as an argument or written to this machine.
#   * CPU and memory come from the kubelet's Summary API through the node
#     proxy. It reports cumulative CPU nanoseconds per container, so a delta
#     between two samples is an exact CPU-time figure. metrics-server is not
#     installed on the pilot, and `kubectl top` on a finished Job pod returns
#     nothing, which silently disabled the generator-saturation verdict.
#
# Sourced, not executed. Expects NS to be set.

PROBE_POD="${PROBE_POD:-durexec-probe}"

# engine_get <pod> <path>  -> response body on stdout
engine_get() {
  kubectl get --raw "/api/v1/namespaces/${NS}/pods/${1}:8080/proxy${2}"
}

# engine_post <pod> <path> <json-file>  -> response body on stdout
engine_post() {
  kubectl create --raw "/api/v1/namespaces/${NS}/pods/${1}:8080/proxy${2}" -f "$3"
}

engine_pods() {
  kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{.items[*].metadata.name}'
}

# probe_ensure: create the probe pod if absent and wait until it is Ready.
# Read-only against the database by construction of what callers send it;
# it holds one connection only while a query runs.
probe_ensure() {
  if ! kubectl -n "$NS" get pod "$PROBE_POD" >/dev/null 2>&1; then
    kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE_POD}
  namespace: ${NS}
  labels: { app: durexec-probe }
spec:
  nodeSelector: { durexec.io/role: harness }
  restartPolicy: Always
  terminationGracePeriodSeconds: 1
  containers:
    - name: probe
      image: pgvector/pgvector:pg16
      command: ["sh", "-c", "trap 'exit 0' TERM; while :; do sleep 3600 & wait; done"]
      env:
        - name: DATABASE_URL
          valueFrom: { secretKeyRef: { name: durexec-db, key: url } }
        - name: PGCONNECT_TIMEOUT
          value: "5"
      resources:
        requests: { cpu: 50m, memory: 64Mi }
EOF
  fi
  kubectl -n "$NS" wait --for=condition=Ready "pod/${PROBE_POD}" --timeout=180s >/dev/null
}

# probe_sql: run the SQL on stdin against the engine's database; unaligned,
# tuples only, stop on the first error. The URL is expanded INSIDE the pod.
probe_sql() {
  kubectl -n "$NS" exec -i "$PROBE_POD" -- \
    sh -c 'exec psql "$DATABASE_URL" -X -At -v ON_ERROR_STOP=1 -F "|"'
}

# probe_rtt <n>: n timed round trips of `select 1` from the harness node to
# the database, one per line in milliseconds. Uses psql's own \timing so the
# figure excludes kubectl and process start-up.
probe_rtt() {
  local n="${1:-20}"
  { echo '\timing on'; for _ in $(seq 1 "$n"); do echo 'select 1;'; done; } \
    | kubectl -n "$NS" exec -i "$PROBE_POD" -- \
        sh -c 'exec psql "$DATABASE_URL" -X -At' \
    | sed -n 's/^Time: \([0-9.]*\) ms.*/\1/p'
}

# node_summary <node>: the kubelet Summary API JSON for one node.
node_summary() {
  kubectl get --raw "/api/v1/nodes/${1}/proxy/stats/summary"
}
