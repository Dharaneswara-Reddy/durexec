#!/usr/bin/env bash
# Tear down the EKS pilot and PROVE nothing billable survives.
#
# Scoped to this experiment's Terraform state only. This account is shared with
# codearena, churn-ml-prod and aiops-demo, so nothing is ever deleted by name
# pattern -- the sweep below is READ-ONLY and reports, it does not destroy.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
. scripts/aws/_common.sh
require_confirmation "destroy the EKS pilot"

REGION="${AWS_REGION:-us-east-1}"

# Kubernetes objects first. A Service of type LoadBalancer or a bound PVC
# creates AWS resources that Terraform does not know about, so destroying the
# cluster underneath them orphans an ELB or an EBS volume that bills for ever.
kubectl delete -k deploy/kubernetes/overlays/eks --ignore-not-found --timeout=180s || true
kubectl -n durexec-lab delete pvc --all --ignore-not-found --timeout=120s || true

terraform -chdir=deploy/aws/eks destroy -auto-approve
TF_RC=$?

echo
echo "=== READ-ONLY sweep: anything still billing? ==="
echo "-- EKS clusters --"
aws eks list-clusters --region "$REGION" --query 'clusters' --output text
echo "-- EC2 tagged durexec --"
aws ec2 describe-instances --region "$REGION" --filters Name=tag:Project,Values=durexec \
  Name=instance-state-name,Values=running,stopped \
  --query 'Reservations[].Instances[].[InstanceId,State.Name]' --output text
# Managed node-group instances are launched by EKS, not Terraform, so they do
# NOT carry the Project=durexec default tag -- the query above cannot see
# them. They carry eks:cluster-name instead.
echo "-- EC2 launched by EKS for the pilot cluster (no Project tag) --"
aws ec2 describe-instances --region "$REGION" --filters Name=tag:eks:cluster-name,Values=durexec-bench \
  Name=instance-state-name,Values=pending,running,stopping,stopped \
  --query 'Reservations[].Instances[].[InstanceId,InstanceType,State.Name]' --output text
echo "-- EBS volumes tagged durexec (bill even when detached) --"
aws ec2 describe-volumes --region "$REGION" --filters Name=tag:Project,Values=durexec \
  --query 'Volumes[].[VolumeId,Size,State]' --output text
echo "-- volumes with NO durexec tag, created by the CSI driver --"
aws ec2 describe-volumes --region "$REGION" --filters Name=status,Values=available \
  --query 'Volumes[?!not_null(Tags)]|[].[VolumeId,Size]' --output text
echo "-- snapshots (survive destroy, bill for ever) --"
aws ec2 describe-snapshots --region "$REGION" --owner-ids self --query 'Snapshots[].[SnapshotId,VolumeSize]' --output text
echo "-- load balancers (an orphaned ELB is ~\$16/month) --"
aws elbv2 describe-load-balancers --region "$REGION" --query 'LoadBalancers[].LoadBalancerName' --output text 2>/dev/null
echo "-- ECR repos (durexec only; others belong to other projects) --"
aws ecr describe-repositories --region "$REGION" --query 'repositories[?starts_with(repositoryName,`durexec/`)].repositoryName' --output text
echo "-- Elastic IPs (bill when unattached) --"
aws ec2 describe-addresses --region "$REGION" --query 'Addresses[].[PublicIp,AllocationId,AssociationId]' --output text
echo "-- network interfaces left in the pilot VPC (block VPC deletion) --"
VPC=$(aws ec2 describe-vpcs --region "$REGION" --filters Name=tag:Project,Values=durexec --query 'Vpcs[].VpcId' --output text)
[ -n "$VPC" ] && aws ec2 describe-network-interfaces --region "$REGION" --filters Name=vpc-id,Values="$VPC" \
  --query 'NetworkInterfaces[].[NetworkInterfaceId,Status,Description]' --output text
echo "-- pilot VPC still present --"
echo "${VPC}"
echo "-- CloudWatch log groups for the pilot cluster (bill for storage) --"
aws logs describe-log-groups --region "$REGION" --log-group-name-prefix /aws/eks/durexec \
  --query 'logGroups[].[logGroupName,storedBytes]' --output text
echo
echo "Empty output on every line above except ECR means nothing is billing."
echo "ECR images persist deliberately so a re-run need not rebuild; delete by hand to stop that charge."
if [ "$TF_RC" != 0 ]; then
  echo "TERRAFORM DESTROY FAILED (exit $TF_RC). Resources may still be billing; read the sweep above and re-run." >&2
  exit "$TF_RC"
fi
