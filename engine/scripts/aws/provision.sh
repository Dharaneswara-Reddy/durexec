#!/usr/bin/env bash
# Create the AWS infrastructure. BILLABLE. Refuses without confirmation.
#
# Separate from deploy_stack.sh on purpose. This one creates resources that
# cost money whether or not the engine ever runs; that one only copies images
# and starts containers on an instance that already exists. Keeping them apart
# means "redeploy the engine" is never one typo away from "rebuild the VPC".
source "$(dirname "$0")/_common.sh"

require_confirmation "create AWS infrastructure (EC2 instance, EBS volumes, VPC)"
show_target

if [ ! -f "$TF_DIR/terraform.tfvars" ]; then
  cat >&2 <<MSG
FATAL: $TF_DIR/terraform.tfvars does not exist.

    cp $TF_DIR/terraform.tfvars.example $TF_DIR/terraform.tfvars

Then set operator_cidr to your own address with a /32. Terraform refuses to
plan without it and rejects 0.0.0.0/0, which is what stops SSH being opened to
the internet by omission.
    curl -s https://checkip.amazonaws.com
MSG
  exit 1
fi

tf init -input=false

echo
echo "=== plan ==="
tf plan -input=false -out=tfplan

# The plan is shown and then confirmed again, because this is the one moment
# where a mistake costs money for as long as it goes unnoticed.
echo
echo "The plan above will be applied. Resources it creates begin billing"
echo "immediately. Press Ctrl-C within 10 seconds to abort."
sleep 10

echo
echo "=== apply ==="
tf apply -input=false tfplan
rm -f "$TF_DIR/tfplan"

echo
echo "=== outputs ==="
tf output

cat <<NOTE

Infrastructure is up and BILLING. Estimated cost while running:
  $(tf_output hourly_cost_estimate_usd)

Next:
  1. Wait for the bootstrap to finish (about two minutes):
       ./scripts/aws/wait_ready.sh
  2. Copy the images and start the engine:
       DUREXEC_CONFIRM=$CONFIRM_PHRASE ./scripts/aws/deploy_stack.sh

To stop every charge:
       DUREXEC_CONFIRM=$CONFIRM_PHRASE ./scripts/aws/teardown.sh
NOTE
