#!/usr/bin/env bash
# Is this laptop in a fit state to build and test? LOCAL ONLY.
#
# Exists because the development host has run out of memory repeatedly during
# this project -- VS Code has been killed mid-experiment more than once, and
# swap has reached 100% twice. A build started on a host with 300 MB free does
# not fail cleanly; it takes the editor with it. This refuses to pretend that
# is fine.
set -uo pipefail

warn=0

echo "=== memory ==="
free -h
avail=$(free -m | awk '/^Mem:/{print $7}')
swap_free=$(free -m | awk '/^Swap:/{print $4}')
echo
echo "available: ${avail} MB    swap free: ${swap_free} MB"
# A docker build of this module peaks around 1.2 GB with the Go toolchain and
# the module cache resident.
if [ "$avail" -lt 1500 ]; then
  echo "WARN: under 1500 MB available. A docker build peaks near 1.2 GB."
  echo "      Free memory first: stop the embedder container (unused by the"
  echo "      Kubernetes lab), stop the minikube VM (~3.8 GB), or close tabs."
  warn=1
else
  echo "OK: enough memory for a build."
fi
if [ "$swap_free" -lt 512 ]; then
  echo "WARN: swap is nearly full (${swap_free} MB free). The host is already"
  echo "      under memory pressure; builds will thrash."
  warn=1
fi

echo
echo "=== disk ==="
df -h /home /var/lib/docker 2>/dev/null | sed 's/^/  /'
home_avail=$(df -BG --output=avail /home 2>/dev/null | tail -1 | tr -dc '0-9')
echo "  /home available: ${home_avail} GB"
if [ "${home_avail:-0}" -lt 10 ]; then
  echo "WARN: under 10 GB free on /home."
  warn=1
fi

echo
echo "=== /tmp is tmpfs, so anything there is RAM ==="
findmnt -no FSTYPE,SIZE,USED /tmp | sed 's/^/  /'
if [ -d /tmp/uv-cache ]; then
  sz=$(du -sh /tmp/uv-cache 2>/dev/null | cut -f1)
  echo "  /tmp/uv-cache is ${sz} of RAM."
  echo "  Durable fix, once /home space is confirmed above:"
  echo "    mkdir -p ~/.cache/uv && export UV_CACHE_DIR=~/.cache/uv"
  echo "  Moving it is a user decision; this script does not delete caches."
fi

echo
echo "=== heavy processes worth stopping ==="
if command -v docker >/dev/null 2>&1; then
  docker ps --format '  {{.Names}}\t{{.Image}}' 2>/dev/null | head -20 || echo "  (docker not responding)"
fi
if command -v minikube >/dev/null 2>&1; then
  echo "  minikube: $(minikube status --format '{{.Host}}' 2>/dev/null || echo 'not running')"
  echo "  (the minikube VM holds ~3.8 GB; 'minikube stop' preserves the disk and PVCs)"
fi

echo
echo "=== toolchain ==="
for t in go docker terraform aws python3; do
  if command -v "$t" >/dev/null 2>&1; then
    # `go version`, not `go --version`: the Go CLI rejects the double dash.
    if [ "$t" = go ]; then ver=$(go version 2>&1 | head -1)
    else ver=$("$t" --version 2>&1 | head -1); fi
    printf '  %-10s %s\n' "$t" "$ver"
  else
    echo "  $t MISSING"
    warn=1
  fi
done

echo
if [ "$warn" -ne 0 ]; then
  echo "RESULT: PROCEED WITH CARE -- see the warnings above."
  exit 2
fi
echo "RESULT: OK to build and test."
