# Shared command transcript helper for the EKS scripts. Source this file; call
# enable_evidence_tee from each eks_*.sh entry point after changing to engine/.

enable_evidence_tee() {
  [ "${DUREXEC_EVIDENCE_TEE_ACTIVE:-}" = 1 ] && return 0

  local root script stamp dir
  root=$(git rev-parse --show-toplevel 2>/dev/null) || {
    echo "FATAL: cannot locate repository root for the evidence transcript" >&2
    return 1
  }
  script="${BASH_SOURCE[1]##*/}"
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  dir="${DUREXEC_EVIDENCE_DIR:-$root/docs/results/cloud-campaign/commands}"
  mkdir -p "$dir"
  DUREXEC_EVIDENCE_LOG="$dir/${stamp}-${script}-$$.log"
  export DUREXEC_EVIDENCE_LOG DUREXEC_EVIDENCE_TEE_ACTIVE=1

  # Capture both streams from this point onward while leaving them visible to
  # the operator. Do not enable shell tracing: scripts may handle secrets.
  exec > >(tee -a "$DUREXEC_EVIDENCE_LOG") 2>&1
  echo "command transcript: $DUREXEC_EVIDENCE_LOG"
}
