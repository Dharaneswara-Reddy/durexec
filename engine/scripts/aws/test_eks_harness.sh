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

# Every loadgen invocation must pass the deployed shard count (campaign 2:
# all non-16-shard points failed without it).
for script in scripts/aws/eks_loadgen.sh scripts/aws/eks_fault_trial.sh; do
  n_mode=$(grep -c '"-mode",' "$script")
  n_count=$(grep -c '"-shard-count","%s"' "$script" || true)
  [ "$n_mode" -ge 1 ] && [ "$n_mode" = "$n_count" ] \
    || { echo "FAIL: $script has $n_mode loadgen arg lists but $n_count pass -shard-count"; exit 1; }
done
echo "loadgen -shard-count self-test: PASS"
