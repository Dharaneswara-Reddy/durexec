#!/usr/bin/env bash
# Run a command on the in-VPC operator instance (deploy/aws/eks/operator.tf)
# through SSM Run Command, wait for it, and print its full output.
#
#   OPS_INSTANCE=i-... OPS_BUCKET=<bucket> scripts/aws/ops_remote.sh '<shell command>'
#
# The command runs as root in /opt/durexec/engine on the operator, with the
# instance role's credentials. Full stdout/stderr go to
# s3://<bucket>/campaign/ssm/ (SSM truncates inline output at 24 KB).
# Exit status is the remote command's.
set -euo pipefail
: "${OPS_INSTANCE:?set OPS_INSTANCE}"; : "${OPS_BUCKET:?set OPS_BUCKET}"
REGION="${AWS_REGION:-us-east-1}"
TIMEOUT="${OPS_TIMEOUT:-14400}"
[ $# -ge 1 ] || { echo "usage: $0 '<command>'" >&2; exit 2; }

params=$(python3 -c '
import json, sys
cmd = sys.argv[1]
print(json.dumps({"commands": ["set -o pipefail", "cd /opt/durexec/engine", cmd],
                  "executionTimeout": [sys.argv[2]]}))' "$1" "$TIMEOUT")

cid=$(aws ssm send-command --region "$REGION" --instance-ids "$OPS_INSTANCE" \
  --document-name AWS-RunShellScript --parameters "$params" \
  --output-s3-bucket-name "$OPS_BUCKET" --output-s3-key-prefix campaign/ssm \
  --timeout-seconds 600 --query Command.CommandId --output text)
echo "ssm command $cid" >&2

status=Pending
while :; do
  status=$(aws ssm get-command-invocation --region "$REGION" --command-id "$cid" \
             --instance-id "$OPS_INSTANCE" --query Status --output text 2>/dev/null || echo Pending)
  case "$status" in Pending|InProgress|Delayed) sleep 10 ;; *) break ;; esac
done

prefix="campaign/ssm/$cid/$OPS_INSTANCE/awsrunShellScript/0.awsrunShellScript"
for stream in stdout stderr; do
  aws s3 cp --quiet "s3://$OPS_BUCKET/$prefix/$stream" - 2>/dev/null || true
done
rc=$(aws ssm get-command-invocation --region "$REGION" --command-id "$cid" \
       --instance-id "$OPS_INSTANCE" --query ResponseCode --output text 2>/dev/null || echo 1)
echo "ssm status=$status rc=$rc" >&2
[ "$status" = Success ] || exit "${rc/-1/1}"
