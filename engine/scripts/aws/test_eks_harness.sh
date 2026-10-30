#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

for script in scripts/aws/eks_*.sh; do
  bash -n "$script"
done
python3 scripts/aws/node_inventory.py --self-test
python3 scripts/aws/idle_baseline.py --self-test

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/tee-child.sh" <<'SH'
. scripts/aws/_evidence.sh
enable_evidence_tee
echo tee-stdout-marker
echo tee-stderr-marker >&2
printf '%s\n' "$DUREXEC_EVIDENCE_LOG" > "$DUREXEC_EVIDENCE_DIR/logpath"
SH
DUREXEC_EVIDENCE_DIR="$tmp/logs" bash "$tmp/tee-child.sh"
log=$(cat "$tmp/logs/logpath")
grep -q 'tee-stdout-marker' "$log"
grep -q 'tee-stderr-marker' "$log"
echo "evidence tee stdout/stderr self-test: PASS"
