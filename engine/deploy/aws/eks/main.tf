# durexec EKS benchmark cluster.
#
# SEPARATE ROOT MODULE. deploy/aws/terraform/ builds the single-instance Tier 1
# campaign and is left untouched; this builds the multi-node Kubernetes
# campaign. Keeping them apart means the audited single-instance plan cannot be
# disturbed by work on this one, and either can be destroyed alone.
#
# NOT APPLIED. This file has been validated only. No AWS resource has been
# created from it.

terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
  # Local state on purpose; see deploy/aws/terraform/versions.tf for the
  # reasoning. This state DOES hold generated passwords (random_password and
  # the Secrets Manager secret version, in plaintext): treat terraform.tfstate
  # as a credential file. It is gitignored and never leaves the operator laptop.
}

provider "aws" {
  region = var.aws_region
  default_tags {
    tags = merge({
      Project   = "durexec"
      Component = "eks-benchmark"
      ManagedBy = "terraform"
      Env       = var.environment
      Campaign  = "k8s-scaling"
    }, var.extra_tags)
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}

# ---------------------------------------------------------------------------
# Networking
#
# Public subnets with per-node public IPs, deliberately. A NAT gateway is
# $0.045/hr plus $0.045/GB, which exceeds the entire engine nodegroup. Nodes
# are reachable only through the cluster security group; nothing is published.
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = "durexec-${var.environment}-eks" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "durexec-${var.environment}-igw" }
}

# EKS requires subnets in at least two availability zones even when every node
# runs in one. Two subnets; nodes are pinned to the first.
resource "aws_subnet" "public" {
  count                   = 2
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, count.index)
  availability_zone       = data.aws_availability_zones.available.names[count.index]
  map_public_ip_on_launch = true
  tags = {
    Name = "durexec-${var.environment}-public-${count.index}"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
  tags = { Name = "durexec-${var.environment}-public" }
}

resource "aws_route_table_association" "public" {
  count          = 2
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# ---------------------------------------------------------------------------
# IAM, least privilege
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "cluster_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "cluster" {
  name               = "durexec-${var.environment}-eks-cluster"
  assume_role_policy = data.aws_iam_policy_document.cluster_assume.json
}

resource "aws_iam_role_policy_attachment" "cluster" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

data "aws_iam_policy_document" "node_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name               = "durexec-${var.environment}-eks-node"
  assume_role_policy = data.aws_iam_policy_document.node_assume.json
}

# Exactly the four AWS-managed policies a worker node needs. No custom policy,
# no wildcard, nothing that can reach another project's resources.
resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
    "arn:aws:iam::aws:policy/AmazonEBSCSIDriverPolicy",
  ])
  role       = aws_iam_role.node.name
  policy_arn = each.value
}

# ---------------------------------------------------------------------------
# Cluster
#
# VERSION IS PINNED AND THE CHOICE IS LOAD-BEARING. `aws eks
# describe-cluster-versions` on 2026-10-29 reports standard support ending
# 2026-07-29 for 1.33 and 2026-12-02 for 1.34 -- so 1.33 and older are ALREADY
# in extended support, which costs $0.60/hr instead of $0.10/hr. Six times the
# control-plane rate for accepting a stale default. 1.37 is the AWS default,
# with standard support to 2027-12-01.
# ---------------------------------------------------------------------------

resource "aws_eks_cluster" "main" {
  name     = "durexec-${var.environment}"
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version

  vpc_config {
    subnet_ids              = aws_subnet.public[*].id
    endpoint_private_access = true
    endpoint_public_access  = true
    # The Kubernetes API is reachable only from the operator's own address.
    public_access_cidrs = [var.operator_cidr]
  }

  # Control-plane logs cost CloudWatch storage. Off by default; enable only
  # while diagnosing a control-plane problem.
  enabled_cluster_log_types = var.enable_control_plane_logs ? ["api", "audit"] : []

  depends_on = [aws_iam_role_policy_attachment.cluster]
}

# ---------------------------------------------------------------------------
# Nodegroups
#
# Labels and a taint, so placement is enforced rather than hoped for. The
# harness group is TAINTED: without it the scheduler may put engine pods on
# the load generator's node, and the benchmark would measure contention
# between the thing generating load and the thing serving it. That is exactly
# how the concurrency-16 run became invalid.
# ---------------------------------------------------------------------------

resource "aws_eks_node_group" "engine" {
  cluster_name    = aws_eks_cluster.main.name
  node_group_name = "engine"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = [aws_subnet.public[0].id]
  instance_types  = [var.engine_instance_type]
  disk_size       = 20

  scaling_config {
    desired_size = var.engine_node_count
    min_size     = 1
    max_size     = 6
  }

  labels = { "durexec.io/role" = "engine" }

  tags       = { Name = "durexec-${var.environment}-engine" }
  depends_on = [aws_iam_role_policy_attachment.node]

  lifecycle {
    # desired_size is the experimental variable for the node-scaling series.
    # Terraform must not fight a deliberate `aws eks update-nodegroup-config`
    # issued mid-campaign.
    ignore_changes = [scaling_config[0].desired_size]
  }
}

resource "aws_eks_node_group" "harness" {
  cluster_name    = aws_eks_cluster.main.name
  node_group_name = "harness"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = [aws_subnet.public[0].id]
  instance_types  = [var.harness_instance_type]
  disk_size       = 20

  scaling_config {
    desired_size = 1
    min_size     = 1
    max_size     = 1
  }

  labels = { "durexec.io/role" = "harness" }

  taint {
    key    = "durexec.io/harness"
    value  = "true"
    effect = "NO_SCHEDULE"
  }

  tags       = { Name = "durexec-${var.environment}-harness" }
  depends_on = [aws_iam_role_policy_attachment.node]
}

# ---------------------------------------------------------------------------
# Addons
#
# The EBS CSI driver is NOT installed by default on EKS. Without it a PVC stays
# Pending for ever and PostgreSQL never starts. The repository's own
# 20-postgres.yaml declares a volumeClaimTemplate with no storageClassName,
# relying on a cluster default that EKS also does not create -- so both halves
# of this are required, not optional.
# ---------------------------------------------------------------------------

resource "aws_eks_addon" "ebs_csi" {
  cluster_name = aws_eks_cluster.main.name
  addon_name   = "aws-ebs-csi-driver"

  resolve_conflicts_on_create = "OVERWRITE"
  depends_on                  = [aws_eks_node_group.engine]
}

resource "aws_eks_addon" "vpc_cni" {
  cluster_name = aws_eks_cluster.main.name
  addon_name   = "vpc-cni"

  # NetworkPolicy enforcement is OFF by default in the AWS VPC CNI
  # (enable-network-policy-controller defaults to false). A NetworkPolicy
  # applied without this is accepted by the API, appears in `kubectl get
  # networkpolicy`, and blocks nothing -- a partition test would report PASS
  # while partitioning nothing. Enabled here so pod-level isolation is real.
  configuration_values = jsonencode({
    enableNetworkPolicy = "true"
  })

  resolve_conflicts_on_create = "OVERWRITE"
  depends_on                  = [aws_eks_node_group.engine]
}
