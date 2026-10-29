#!/usr/bin/env bash
# Destroy the durexec validation cluster and stop every charge it creates.
#
# READ THIS BEFORE RUNNING IT.
#
# The target account is SHARED. It holds the codearena, churn-ml-prod and
# aiops-demo projects, three stopped EC2 instances, three EBS volumes and an
# associated Elastic IP that are nothing to do with durexec. So this script
# does NOT do anything of the form "delete all instances" or "delete the
# VPC that looks like ours". It destroys exactly what Terraform created, by
# state, and then verifies by TAG that nothing tagged for this project is
# still alive. Anything untagged or belonging to another project is never
# touched.
#
# Two safety properties worth stating:
#   1. `terraform destroy` acts only on resources in terraform.tfstate.
#   2. The post-destroy sweep is read-only. It REPORTS leftovers; it does not
#      delete them, because a tag query is not a safe basis for deletion in a
#      shared account.
source "$(dirname "$0")/_common.sh"

require_confirmation "destroy the durexec validation cluster in AWS"
show_target

echo "=== what Terraform believes exists ==="
if ! tf state list 2>/dev/null | sed 's/^/  /'; then
  echo "  (no state; nothing for Terraform to destroy)"
fi
echo

echo "=== destroying ==="
# -auto-approve is correct HERE and nowhere else: require_confirmation above
# has already made this an explicit, deliberate invocation, and a second
# interactive prompt inside a script just trains people to type yes.
tf destroy -auto-approve -input=false

echo
echo "=== read-only sweep: anything still tagged Project=durexec? ==="
echo "--- instances ---"
aws ec2 describe-instances \
  --filters 'Name=tag:Project,Values=durexec' 'Name=instance-state-name,Values=pending,running,stopping,stopped' \
  --query 'Reservations[].Instances[].[InstanceId,InstanceType,State.Name]' --output text 2>&1 | sed 's/^/  /'
echo "--- volumes (these charge even when nothing is running) ---"
aws ec2 describe-volumes --filters 'Name=tag:Project,Values=durexec' \
  --query 'Volumes[].[VolumeId,Size,State]' --output text 2>&1 | sed 's/^/  /'
echo "--- snapshots ---"
aws ec2 describe-snapshots --owner-ids self --filters 'Name=tag:Project,Values=durexec' \
  --query 'Snapshots[].[SnapshotId,VolumeSize,StartTime]' --output text 2>&1 | sed 's/^/  /'
echo "--- Elastic IPs (charge whether or not anything uses them) ---"
aws ec2 describe-addresses --query 'Addresses[].[PublicIp,AssociationId,Tags[?Key==`Project`].Value|[0]]' \
  --output text 2>&1 | sed 's/^/  /'

cat <<'NOTE'

If any line above is non-empty, that resource is STILL BILLING and Terraform
did not remove it. The usual causes:

  * data_volume_retain_on_destroy was true, so the data volume was kept on
    purpose. Delete it by hand when the journal is no longer needed:
        aws ec2 delete-volume --volume-id <vol-id>
  * a snapshot was taken during the Tier 2 backup test. Snapshots are
    $0.05/GB-month and persist for ever until deleted:
        aws ec2 delete-snapshot --snapshot-id <snap-id>

The Elastic IP listing is deliberately NOT filtered by tag, because the
account has a pre-existing associated Elastic IP belonging to another project.
If one appears with no Project=durexec tag, it is not ours -- leave it alone.
NOTE
