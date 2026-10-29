#!/usr/bin/env bash
# promq.sh — one instant PromQL query against the lab's Prometheus.
#
# Uses POST with a form body rather than a GET query string, because PromQL is
# full of characters (+ { } = ~ " ) that have to be percent-encoded in a URL and
# a half-encoded regex silently becomes a DIFFERENT query that still returns
# 200. POST with --data-urlencode makes curl do the encoding.
#
# Usage:  ./promq.sh 'rate(container_cpu_usage_seconds_total[1m])'
#         ./promq.sh 'up' raw     # raw JSON instead of the flattened form
set -uo pipefail
PROM="${PROM:-http://$(minikube ip 2>/dev/null):30090}"
q="${1:?usage: promq.sh <promql> [raw]}"
out="$(curl -sS -X POST "$PROM/api/v1/query" --data-urlencode "query=$q" 2>&1)"
if [ "${2:-}" = "raw" ]; then echo "$out"; exit 0; fi
printf '%s' "$out" | python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except Exception as e: print("PARSE-FAIL:", e); sys.exit(1)
if d.get("status")!="success":
    print("QUERY-FAIL:", d.get("errorType"), d.get("error")); sys.exit(1)
r=d["data"]["result"]
if not r: print("(empty result)"); sys.exit(0)
for s in r:
    m=dict(s["metric"]); name=m.pop("__name__","")
    lbl=",".join(f"{k}={v}" for k,v in sorted(m.items()))
    v = s["value"][1]
    print(f"{name}{{{lbl}}} = {v}")
'
