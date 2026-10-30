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

# `cmd | head -1 | grep` under pipefail fails at random once the input is large
# (SIGPIPE on the first command). None may remain in the EKS scripts.
if grep -nE 'head -1 \| grep' scripts/aws/eks_*.sh; then
  echo "FAIL: pipefail-unsafe 'head -1 | grep' pattern above"; exit 1
fi
big="$tmp/big.csv"; python3 -c "print(\"h,e,a\"); print(\"x,y,z\\n\" * 20000, end=\"\")" > "$big"
for _ in 1 2 3 4 5; do
  bash -c 'set -o pipefail; awk "!/^#/ {print; exit}" "$1" | grep -qxF h,e,a' _ "$big" \
    || { echo "FAIL: header check is not pipefail-safe"; exit 1; }
done
echo "pipefail-safe header check self-test: PASS"

# eks_fault.sh's engine-pod check against a kubectl that, like the real one,
# refuses a pod name together with -l (campaign 3: every fault target failed).
mkdir -p "$tmp/kbin"
cat > "$tmp/kbin/kubectl" <<'K'
#!/usr/bin/env bash
case " $* " in
  *" get pod "*" -l "*) echo "error: name cannot be provided when a selector is specified" >&2; exit 1 ;;
  *" get pod durexec-1 "*labels.app*) printf durexec ;;
  *" get pod other "*labels.app*) printf other ;;
  *) exit 1 ;;
esac
K
chmod +x "$tmp/kbin/kubectl"
check_pod() { PATH="$tmp/kbin:$PATH" NS=durexec-lab bash -c '
  die() { echo "$*" >&2; exit 1; }
  eval "$(sed -n "/^engine_pod() {/,/^}/p" scripts/aws/eks_fault.sh)"
  engine_pod "$1"' _ "$1"; }
check_pod durexec-1 || { echo "FAIL: engine_pod rejects a real engine pod"; exit 1; }
if check_pod other 2>/dev/null; then echo "FAIL: engine_pod accepts a non-engine pod"; exit 1; fi
echo "fault injector engine-pod check self-test: PASS"

# Campaign 3: busybox `pgrep -x engined` never matched /engined, so no kill or
# freeze was injected; and drain's cleanup resolved the node through the
# evicted (Pending) pod, leaving the node cordoned. Neither may return.
if grep -n 'pgrep -x engined' scripts/aws/eks_fault.sh; then echo "FAIL: pgrep -x cannot find /engined under busybox"; exit 1; fi
grep -q 'drain)     "$FAULT" uncordon "$VNODE"' scripts/aws/eks_fault_trial.sh \
  || { echo "FAIL: drain cleanup must uncordon the node recorded before injection"; exit 1; }
echo "fault injector process lookup and drain cleanup self-test: PASS"
