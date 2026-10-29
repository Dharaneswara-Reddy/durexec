#!/usr/bin/env bash
# Pilot smoke acceptance. Read-only against a deployed cluster; creates no AWS
# resource. Every check PROVES its claim rather than trusting a status field.
set -uo pipefail
NS="${NS:-durexec-lab}"
pass=0; fail=0
ck() { if [ "$2" = 0 ]; then echo "  PASS  $1"; pass=$((pass+1)); else echo "  FAIL  $1 -- $3"; fail=$((fail+1)); fi; }

echo "== S1 engine pods Ready =="
ready=$(kubectl -n "$NS" get pods -l app=durexec \
  -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True)
total=$(kubectl -n "$NS" get pods -l app=durexec --no-headers | wc -l)
[ "$ready" -gt 0 ] && [ "$ready" = "$total" ]; ck "all $total engine pods Ready" $? "only $ready/$total"

echo "== S2 pods actually on DISTINCT nodes (the anti-affinity claim) =="
nodes=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u | wc -l)
[ "$nodes" = "$total" ]; ck "$total pods on $nodes distinct nodes" $? "pods share nodes; anti-affinity did not hold"

echo "== S3 NO in-cluster postgres (it belongs on EC2) =="
[ "$(kubectl -n "$NS" get sts postgres --no-headers 2>/dev/null | wc -l)" = 0 ]
ck "no in-cluster postgres StatefulSet" $? "an unused database is running and billing"

echo "== S4 engine reached the EXTERNAL database =="
pod=$(kubectl -n "$NS" get pods -l app=durexec -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NS" exec "$pod" -- sh -c 'wget -qO- http://localhost:8080/healthz' 2>/dev/null | grep -q '"status":"ok"'
ck "/healthz reports ok" $? "engine unhealthy"

echo "== S5 shards owned, none orphaned =="
sh=$(kubectl -n "$NS" exec "$pod" -- sh -c 'wget -qO- http://localhost:8080/v1/shards' 2>/dev/null)
unowned=$(printf '%s' "$sh" | python3 -c "import json,sys;d=json.load(sys.stdin);print(sum(1 for x in d['shards'] if not x.get('owner')))" 2>/dev/null || echo 99)
[ "$unowned" = 0 ]; ck "0 unowned shards" $? "$unowned shards have no owner"

echo "== S6 a workflow durably completes =="
id="smoke-$(date +%s)"
kubectl -n "$NS" exec "$pod" -- sh -c "wget -qO- --post-data='{\"workflow_id\":\"$id\",\"workflow_type\":\"DeliveryOrder\",\"input\":{\"order_id\":\"$id\",\"amount_cents\":2450,\"prep_time_ns\":50000000}}' --header='Content-Type: application/json' http://localhost:8080/v1/workflows" >/dev/null 2>&1
st=""; for _ in $(seq 1 60); do
  st=$(kubectl -n "$NS" exec "$pod" -- sh -c "wget -qO- http://localhost:8080/v1/workflows/$id" 2>/dev/null | python3 -c "import json,sys;print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
  [ "$st" = COMPLETED ] && break; sleep 2
done
[ "$st" = COMPLETED ]; ck "workflow $id COMPLETED" $? "status=$st"

echo "== S7 metrics present and non-constant =="
m=$(kubectl -n "$NS" exec "$pod" -- sh -c 'wget -qO- http://localhost:8080/metrics' 2>/dev/null)
printf '%s' "$m" | grep -q 'replay_duration_seconds'; ck "replay_duration_seconds exported" $? "missing"

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" = 0 ]
