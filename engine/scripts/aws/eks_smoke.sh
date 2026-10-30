#!/usr/bin/env bash
# Pilot smoke acceptance. Read-only against a deployed cluster except for one
# test workflow and the probe pod; creates no AWS resource. Every check PROVES
# its claim rather than trusting a status field.
#
# The engine image is distroless (no shell), so HTTP goes through the API
# server's pod proxy and SQL through the probe pod -- see scripts/aws/_k8s.sh.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
NS="${NS:-durexec-lab}"
. scripts/aws/_k8s.sh
enable_evidence_tee
pass=0; fail=0
ck() { if [ "$2" = 0 ]; then echo "  PASS  $1"; pass=$((pass+1)); else echo "  FAIL  $1 -- $3"; fail=$((fail+1)); fi; }
cfg() { kubectl -n "$NS" get configmap durexec-config -o jsonpath="{.data.$1}"; }

echo "== S1 engine pods Ready =="
ready=$(kubectl -n "$NS" get pods -l app=durexec \
  -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True)
total=$(kubectl -n "$NS" get pods -l app=durexec --no-headers | wc -l)
[ "$ready" -gt 0 ] && [ "$ready" = "$total" ]; ck "all $total engine pods Ready" $? "only $ready/$total"

echo "== S2 pods actually on DISTINCT nodes (the anti-affinity claim) =="
nodes=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u | wc -l)
[ "$nodes" = "$total" ]; ck "$total pods on $nodes distinct nodes" $? "pods share nodes; anti-affinity did not hold"

echo "== S2b replicas, ring and connection budget agree with the cluster =="
# The base manifest ships replicas=2 with a two-name ring. On a three-node
# engine group that silently runs a two-engine experiment and S1/S2 still pass.
engine_nodes=$(kubectl get nodes -l durexec.io/role=engine --no-headers | wc -l)
ring_n=$(cfg CLUSTER_NODES | tr ',' '\n' | grep -c .)
nc=$(cfg NODE_COUNT)
want="${EXPECT_REPLICAS:-$engine_nodes}"
[ "$total" = "$want" ] && [ "$ring_n" = "$total" ] && [ "$nc" = "$total" ]
ck "replicas=$total ring=$ring_n NODE_COUNT=$nc expected=$want (engine nodes: $engine_nodes)" $? "mismatch: the experiment is not the topology it claims"

echo "== S3 NO in-cluster postgres (it belongs on EC2) =="
[ "$(kubectl -n "$NS" get sts postgres --no-headers 2>/dev/null | wc -l)" = 0 ]
ck "no in-cluster postgres StatefulSet" $? "an unused database is running and billing"

pod=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{.items[0].metadata.name}')

echo "== S4 every engine reports healthy against the EXTERNAL database =="
bad=""
for p in $(engine_pods); do
  engine_get "$p" /healthz 2>/dev/null | grep -q '"status":"ok"' || bad="$bad $p"
done
[ -z "$bad" ]; ck "/healthz ok on every pod" $? "unhealthy:$bad"

echo "== S5 shards owned, none orphaned, spread over every pod =="
sh=$(engine_get "$pod" /v1/shards 2>/dev/null)
read -r unowned owners < <(printf '%s' "$sh" | python3 -c "
import json,sys
d=json.load(sys.stdin); s=d['shards']
print(sum(1 for x in s if not x.get('owner')), len({x.get('owner') for x in s if x.get('owner')}))" 2>/dev/null || echo "99 0")
[ "$unowned" = 0 ] && [ "$owners" = "$total" ]
ck "0 unowned shards, owned by $owners of $total pods, SHARD_COUNT=$(cfg SHARD_COUNT)" $? "$unowned unowned; $owners owners"

echo "== S6 a workflow durably completes =="
id="smoke-$(date +%s)"
body=$(mktemp); printf '{"workflow_id":"%s","workflow_type":"DeliveryOrder","input":{"order_id":"%s","amount_cents":2450,"prep_time_ns":50000000}}' "$id" "$id" > "$body"
engine_post "$pod" /v1/workflows "$body" >/dev/null 2>&1; rm -f "$body"
st=""; for _ in $(seq 1 60); do
  st=$(engine_get "$pod" "/v1/workflows/$id" 2>/dev/null | python3 -c "import json,sys;print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
  [ "$st" = COMPLETED ] && break; sleep 2
done
[ "$st" = COMPLETED ]; ck "workflow $id COMPLETED" $? "status=$st"

echo "== S7 metrics present on every pod, including process and pool series =="
bad=""
for p in $(engine_pods); do
  m=$(engine_get "$p" /metrics 2>/dev/null)
  for series in replay_duration_seconds process_cpu_seconds_total go_goroutines db_pool_conns_acquired; do
    grep -q "^$series" <<<"$m" || bad="$bad $p:$series"
  done
done
[ -z "$bad" ]; ck "required series exported on every pod" $? "missing:$bad"

echo "== S8 the probe reaches the database with the engine's own credentials =="
probe_ensure 2>/dev/null
v=$(echo "select count(*) from executions where workflow_id = '$id' and status = 'COMPLETED';" | probe_sql 2>/dev/null)
[ "$v" = 1 ]; ck "smoke workflow visible COMPLETED in PostgreSQL" $? "got '${v}'"

echo "== S9 database instrumentation that comparison with the local lab depends on =="
inst=$(probe_sql 2>/dev/null <<'SQL'
select (select count(*) from pg_extension where extname = 'pg_stat_statements')
    || '|' || current_setting('track_io_timing') || '|' || current_setting('is_superuser');
SQL
)
[ "$inst" = "1|on|on" ]; ck "pg_stat_statements, track_io_timing, superuser probe ($inst)" $? "got '$inst' (want 1|on|on); host CPU/disk and per-query latency would be missing"

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" = 0 ]
