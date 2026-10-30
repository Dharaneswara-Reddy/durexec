# Optional in-VPC operator (enable_operator = true).
#
# For running the harness when the operator's own machine cannot reach the
# Kubernetes API: the public endpoint stays restricted to operator_cidr, and
# this instance reaches the PRIVATE endpoint from inside the VPC. It has no
# inbound rule at all; commands arrive through SSM Run Command, which is an
# AWS API call, not a network path into the VPC.
#
# Its role can: describe this cluster; push and read durexec/* images; read
# the database secret (eks_deploy.sh builds the Kubernetes Secret from it);
# read the Terraform state object and read/write the evidence prefix of the
# campaign bucket. Its EKS access entry is cluster-admin on this cluster only.
# It is destroyed with everything else by `terraform destroy`.

locals {
  op = var.enable_operator ? 1 : 0
}

data "aws_iam_policy_document" "operator_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "operator" {
  count              = local.op
  name               = "durexec-${var.environment}-operator"
  assume_role_policy = data.aws_iam_policy_document.operator_assume.json
}

resource "aws_iam_role_policy_attachment" "operator_ssm" {
  count      = local.op
  role       = aws_iam_role.operator[0].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "operator" {
  statement {
    sid       = "Cluster"
    actions   = ["eks:DescribeCluster"]
    resources = [aws_eks_cluster.main.arn]
  }
  statement {
    sid       = "EcrAuth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid = "EcrDurexecOnly"
    actions = [
      "ecr:CreateRepository", "ecr:DescribeRepositories", "ecr:DescribeImages", "ecr:TagResource",
      "ecr:BatchCheckLayerAvailability", "ecr:InitiateLayerUpload", "ecr:UploadLayerPart",
      "ecr:CompleteLayerUpload", "ecr:PutImage", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
    ]
    resources = ["arn:aws:ecr:${var.aws_region}:${data.aws_caller_identity.current.account_id}:repository/durexec/*"]
  }
  statement {
    sid       = "DbSecret"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.db.arn]
  }
  # Fault campaign: eks_fault.sh reboot-db checks the DB host's Project tag and
  # reboots it. Describe cannot be resource-scoped; reboot is limited to the
  # pilot's own database instance.
  statement {
    sid       = "FaultDescribe"
    actions   = ["ec2:DescribeInstances"]
    resources = ["*"]
  }
  statement {
    sid       = "FaultRebootDb"
    actions   = ["ec2:RebootInstances"]
    resources = [aws_instance.database.arn]
  }
  statement {
    sid       = "BucketList"
    actions   = ["s3:ListBucket"]
    resources = ["arn:aws:s3:::${var.campaign_bucket}"]
  }
  statement {
    sid       = "StateRead"
    actions   = ["s3:GetObject"]
    resources = ["arn:aws:s3:::${var.campaign_bucket}/eks/terraform.tfstate"]
  }
  statement {
    sid       = "Evidence"
    actions   = ["s3:GetObject", "s3:PutObject"]
    resources = ["arn:aws:s3:::${var.campaign_bucket}/campaign/*"]
  }
}

resource "aws_iam_role_policy" "operator" {
  count  = local.op
  name   = "durexec-operator"
  role   = aws_iam_role.operator[0].id
  policy = data.aws_iam_policy_document.operator.json
}

resource "aws_iam_instance_profile" "operator" {
  count = local.op
  name  = "durexec-${var.environment}-operator"
  role  = aws_iam_role.operator[0].name
}

resource "aws_eks_access_entry" "operator" {
  count         = local.op
  cluster_name  = aws_eks_cluster.main.name
  principal_arn = aws_iam_role.operator[0].arn
}

resource "aws_eks_access_policy_association" "operator" {
  count         = local.op
  cluster_name  = aws_eks_cluster.main.name
  principal_arn = aws_iam_role.operator[0].arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
  access_scope {
    type = "cluster"
  }
  depends_on = [aws_eks_access_entry.operator]
}

resource "aws_security_group" "operator" {
  count       = local.op
  name        = "durexec-${var.environment}-operator"
  description = "In-VPC harness operator: no inbound, egress only"
  vpc_id      = aws_vpc.main.id
  tags        = { Name = "durexec-${var.environment}-operator" }
}

resource "aws_vpc_security_group_egress_rule" "operator_all" {
  count             = local.op
  security_group_id = aws_security_group.operator[0].id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
  description       = "SSM, ECR, S3, image builds"
}

# The private API endpoint lives in the cluster security group, which admits
# only itself. One rule: HTTPS from the operator's group. No NodePort or other
# port is opened.
resource "aws_vpc_security_group_ingress_rule" "cluster_api_from_operator" {
  count                        = local.op
  security_group_id            = aws_eks_cluster.main.vpc_config[0].cluster_security_group_id
  referenced_security_group_id = aws_security_group.operator[0].id
  from_port                    = 443
  to_port                      = 443
  ip_protocol                  = "tcp"
  description                  = "Kubernetes API (private endpoint) from the in-VPC operator"
}

resource "aws_instance" "operator" {
  count                       = local.op
  ami                         = data.aws_ssm_parameter.al2023.insecure_value
  instance_type               = var.operator_instance_type
  subnet_id                   = aws_subnet.public[0].id
  vpc_security_group_ids      = [aws_security_group.operator[0].id]
  iam_instance_profile        = aws_iam_instance_profile.operator[0].name
  associate_public_ip_address = true

  root_block_device {
    volume_size = 30
    volume_type = "gp3"
    encrypted   = true
  }

  metadata_options {
    http_tokens                 = "required"
    http_endpoint               = "enabled"
    http_put_response_hop_limit = 2
  }

  user_data = <<-EOT
    #!/usr/bin/env bash
    set -euo pipefail
    exec > >(tee /var/log/durexec-operator-bootstrap.log) 2>&1
    dnf install -y docker git jq tar python3-pyyaml
    systemctl enable --now docker
    KV=$(curl -fsSL https://dl.k8s.io/release/stable.txt)
    curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/$${KV}/bin/linux/amd64/kubectl"
    chmod +x /usr/local/bin/kubectl
    curl -fsSLo /tmp/tf.zip https://releases.hashicorp.com/terraform/1.10.5/terraform_1.10.5_linux_amd64.zip
    python3 -c 'import zipfile; zipfile.ZipFile("/tmp/tf.zip").extract("terraform", "/usr/local/bin")'
    chmod +x /usr/local/bin/terraform
    touch /opt/durexec-operator-ready
  EOT

  tags = { Name = "durexec-${var.environment}-operator" }
}

output "operator_instance_id" {
  value = var.enable_operator ? aws_instance.operator[0].id : ""
}
