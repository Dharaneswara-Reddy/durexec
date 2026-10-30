# Shared helpers. Sourced, never executed.
#
# The single most important thing in this file is require_confirmation. Every
# script that can create a billable resource, change a running cloud cluster
# or delete anything calls it, and it refuses unless the operator has said so
# in an environment variable that cannot be produced by a stray tab-complete.
# The requirement is that the default workflow is safe and local-only; this is
# how that is enforced rather than documented.

set -euo pipefail

# Default to the named profile only when no credentials are supplied in the
# environment. Forcing a profile that does not exist on this machine makes
# every aws call fail with "profile not found" even though valid environment
# credentials are present -- the case on a cloud runner with injected secrets.
if [ -z "${AWS_ACCESS_KEY_ID:-}" ]; then
  : "${AWS_PROFILE:=aiops-deploy}"
  export AWS_PROFILE
fi
: "${AWS_REGION:=us-east-1}"
export AWS_REGION

. scripts/aws/_evidence.sh

TF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../deploy/aws/terraform" && pwd)"
ENGINE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

CONFIRM_PHRASE="i-understand-this-costs-money"

require_confirmation() {
  # Only $1 is used. Note that $* inside a function is the FUNCTION's
  # arguments, not the script's, so it must not be echoed into the suggested
  # command line -- it would print this description as if it were a flag.
  local what="$1"
  if [ "${DUREXEC_CONFIRM:-}" != "$CONFIRM_PHRASE" ]; then
    cat >&2 <<MSG

REFUSING: $what

This action touches AWS and can cost money or destroy data. It is not run by
accident. Set the confirmation variable in the same command:

    DUREXEC_CONFIRM=$CONFIRM_PHRASE $0

Current target:
    profile  ${AWS_PROFILE:-<environment credentials>}
    region   $AWS_REGION

MSG
    exit 1
  fi
}

# Print the account being acted on before doing anything. The credentials on
# this host can reach TWO accounts, and one of them holds three unrelated
# student projects (codearena, churn-ml-prod, aiops-demo). A script that
# destroys things must say out loud which account it is pointed at.
show_target() {
  echo "--- AWS target ---"
  aws sts get-caller-identity \
    --query '{Account:Account,Arn:Arn}' --output table 2>&1 || {
      echo "FATAL: cannot reach AWS with profile ${AWS_PROFILE:-<environment credentials>}" >&2
      exit 1
    }
  echo "  region: $AWS_REGION"
  echo
}

tf() { terraform -chdir="$TF_DIR" "$@"; }

tf_output() { terraform -chdir="$TF_DIR" output -raw "$1" 2>/dev/null || true; }

# The instance's address, read from Terraform state rather than remembered.
# There is no Elastic IP in this stack, so the address changes on every
# stop/start and a hard-coded one is always eventually wrong.
instance_ip() {
  local ip
  ip=$(tf_output public_ip)
  if [ -z "$ip" ]; then
    echo "FATAL: no public_ip in the Terraform state. Has the stack been applied?" >&2
    exit 1
  fi
  echo "$ip"
}

ssh_key() {
  local pub
  pub=$(terraform -chdir="$TF_DIR" output -raw ssh_command 2>/dev/null | sed -n 's/.*-i \([^ ]*\).*/\1/p')
  echo "${pub:-$HOME/.ssh/durexec-aws}"
}

remote() {
  ssh -o StrictHostKeyChecking=accept-new -i "$(ssh_key)" \
    "ec2-user@$(instance_ip)" "$@"
}
