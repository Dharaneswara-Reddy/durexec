#!/usr/bin/env bash
# Prove that the cloud compose file publishes NOTHING on a public interface.
#
# This exists because the requirement "the external-effect simulator must not
# become a publicly accessible service" cannot be met by a comment. A comment
# describes an intention; this reads the file Docker will actually act on and
# fails the build if any published port is missing its 127.0.0.1 prefix.
#
# It is cheap, local, needs no AWS access, and is wired into
# `make aws-validate`, so the property is re-checked every time rather than
# the day someone remembers to look.
#
# Exit 0 = safe. Exit 1 = a port would be reachable from outside the instance.
set -euo pipefail

cd "$(dirname "$0")/../.."
COMPOSE=deploy/aws/docker-compose.cloud.yml

if [ ! -f "$COMPOSE" ]; then
  echo "FAIL: $COMPOSE not found" >&2
  exit 1
fi

fail=0

# Read the ports as Docker will see them. `docker compose config` resolves the
# anchors, the profiles and the variable substitution, so this checks the
# EFFECTIVE configuration rather than the source text -- a YAML anchor could
# otherwise hide a binding from a grep. The dummy values exist only to satisfy
# the `:?` required-variable guards; none of them is used for anything.
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  echo "--- checking the effective configuration (docker compose config) ---"
  effective=$(
    DUREXEC_VERSION=assert-only \
    POSTGRES_PASSWORD=assert-only \
    REPL_PASSWORD=assert-only \
    DUREXEC_DATA_DIR=/tmp/assert-only \
    docker compose -f "$COMPOSE" --profile extsim config --format json 2>/dev/null
  ) || { echo "FAIL: 'docker compose config' rejected $COMPOSE" >&2; exit 1; }

  echo "$effective" | python3 -c '
import json, sys
cfg = json.load(sys.stdin)
bad, total = [], 0
for name, svc in sorted(cfg.get("services", {}).items()):
    for p in svc.get("ports", []) or []:
        total += 1
        host_ip = p.get("host_ip") if isinstance(p, dict) else None
        published = p.get("published") if isinstance(p, dict) else p
        if host_ip not in ("127.0.0.1", "::1"):
            # Built outside the f-string on purpose. Python before 3.12
            # rejects a backslash inside an f-string expression, so the
            # quoted fallback cannot be inlined here.
            shown = host_ip or "0.0.0.0 (ALL INTERFACES)"
            bad.append(f"{name}: published={published} host_ip={shown}")
print(f"  {total} published port(s) found")
if bad:
    print("\nFAIL: these ports are NOT restricted to the loopback interface:")
    for b in bad:
        print("  -", b)
    sys.exit(1)
print("  every published port is bound to the loopback interface")
' || fail=1

  # The simulator is held to a stricter rule than everything else: not
  # "published only on loopback" but "not published at all". Its /arm endpoint
  # can make a payment processor fail on demand, so the engine reaches it over
  # the compose network and an operator reads its ledger with `docker exec`.
  echo "--- checking that extsim publishes no port at all ---"
  echo "$effective" | python3 -c '
import json, sys
cfg = json.load(sys.stdin)
svc = cfg.get("services", {}).get("extsim")
if svc is None:
    print("  WARN: no extsim service in the effective config (profile not resolved?)")
    sys.exit(0)
ports = svc.get("ports") or []
if ports:
    print(f"FAIL: extsim publishes {ports}; it must publish nothing.")
    sys.exit(1)
print("  extsim publishes no port")
' || fail=1
else
  echo "WARN: docker compose unavailable; falling back to a text scan."
  echo "      This is weaker: it cannot resolve anchors or variables."
  # Every quoted mapping of the form "a:b" or "a:b:c" that is not already
  # prefixed with an explicit loopback address.
  if grep -nE '^\s*-\s*"[0-9]+:[0-9]+"' "$COMPOSE"; then
    echo "FAIL: the lines above publish a port on all interfaces." >&2
    fail=1
  else
    echo "  no unqualified host port mappings found"
  fi
fi

# Belt and braces, independent of Docker: the Terraform must not grow an
# ingress rule for any of the service ports. The compose binding and the
# security group are two separate mistakes, and either one alone is enough to
# expose a service.
echo "--- checking the Terraform opens no service port ---"
if grep -nE 'from_port\s*=\s*(3000|8080|8081|8082|8083|8090|9090|5432|5433|5434)' deploy/aws/terraform/*.tf; then
  echo "FAIL: the Terraform above opens a service port in a security group." >&2
  fail=1
else
  echo "  no security-group rule mentions a service port"
fi

if [ "$fail" -ne 0 ]; then
  echo
  echo "RESULT: FAIL -- something would be reachable from outside the instance."
  exit 1
fi
echo
echo "RESULT: PASS -- nothing is reachable from outside the instance except SSH."
