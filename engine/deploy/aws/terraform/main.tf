# durexec validation cluster: one EC2 instance in a VPC of its own.
#
# The architecture and the reasoning behind it are in
# docs/cloud-deployment/ARCHITECTURE.md. The short version of why it is one
# instance and not several: the engine resolves its peers by node id
# (cmd/engined/main.go builds "http://" + nodeID + port), so a node id has to
# be a resolvable name. Docker Compose's embedded DNS provides that for free.
# Spreading the three nodes across three EC2 instances would need a Route 53
# private hosted zone and three public IPv4 addresses, and would validate
# nothing in Tier 1 that this does not.

provider "aws" {
  region = var.aws_region

  # Every resource gets these without having to say so, which is what makes
  # the tag-scoped teardown in scripts/aws/teardown.sh safe to run in an
  # account that holds other people's projects.
  default_tags {
    tags = merge(
      {
        Project   = "durexec"
        Component = "validation-cluster"
        Env       = var.environment
        ManagedBy = "terraform"
        Phase     = "phase9-aws"
        Repo      = "durexec"
      },
      var.extra_tags,
    )
  }
}

locals {
  name = "durexec-${var.environment}"
}

# Amazon Linux 2023, resolved from the SSM public parameter rather than
# hard-coded. A pinned AMI id is region-specific and goes stale; this asks AWS
# for the current one. The trade-off is that a `terraform apply` months apart
# can pick up a newer AMI and replace the instance, so the id is recorded in
# the outputs for the record of what a given run actually used.
#
# AL2023 specifically because it ships the SSM agent preinstalled and has
# docker in its default repositories, so user_data does not have to add a
# third-party repo before it can install anything.
data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

data "aws_availability_zones" "available" {
  state = "available"
}

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr

  # Both needed for Docker's embedded DNS to be reachable and for the instance
  # to resolve the SSM and EBS endpoints it needs.
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = "${local.name}-vpc" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${local.name}-igw" }
}

# One public subnet in one AZ. A second AZ would cost nothing by itself, but
# nothing in this stack spans AZs: there is one instance, and cross-AZ traffic
# is billed per GB. A multi-AZ design belongs with the multi-instance option
# described in the architecture document, not here.
resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, 0)
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = { Name = "${local.name}-public" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = { Name = "${local.name}-rt-public" }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

# ---------------------------------------------------------------------------
# Security group
# ---------------------------------------------------------------------------

# Rules are separate aws_vpc_security_group_*_rule resources rather than
# inline ingress/egress blocks. Inline blocks are authoritative: a rule added
# by hand during an incident is silently deleted on the next apply, which is
# how a working firewall quietly reverts. Separate resources also mean the
# plan output names each rule individually, so a review can see exactly what
# is being opened.
resource "aws_security_group" "instance" {
  name        = "${local.name}-sg"
  description = "durexec validation cluster: SSH from the operator only, no inbound HTTP"
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-sg" }

  lifecycle {
    create_before_destroy = true
  }
}

# The ONLY inbound rule, and it is conditional. There is deliberately no rule
# for 8081-8083 (the engine), 3000 (Grafana), 9090 (Prometheus) or 8090 (the
# external-effect simulator). Those listeners are bound to 127.0.0.1 inside
# the instance and are reached through an SSH or SSM tunnel. The simulator has
# no published port at all.
#
# This is the mechanism that answers the requirement that the test-only
# simulator must not become a public service: not a configuration convention
# that could be edited, but the absence of any route to it.
resource "aws_vpc_security_group_ingress_rule" "ssh" {
  count = var.enable_ssh ? 1 : 0

  security_group_id = aws_security_group.instance.id
  description       = "SSH from the operator's address only"
  cidr_ipv4         = var.operator_cidr
  from_port         = 22
  to_port           = 22
  ip_protocol       = "tcp"
}

# Egress is open, and that is a real decision rather than laziness. The
# instance has to reach the Amazon Linux package mirrors, the Docker and
# distroless registries for the pgvector and Grafana images, and the SSM
# endpoints for Session Manager. Enumerating those as prefix lists is
# possible but they change, and a stack that cannot install its own packages
# fails in a way that looks like a bug in the engine. Nothing inbound is open,
# so an attacker has no foothold from which open egress helps them.
resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.instance.id
  description       = "Package mirrors, container registries, SSM endpoints"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

# ---------------------------------------------------------------------------
# IAM
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "instance" {
  name               = "${local.name}-instance"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

# Session Manager only. This is the whole managed policy set, and it is worth
# being explicit about what is NOT here: no S3 access, no EC2 API access, no
# ability to describe or modify any other resource in the account. The
# instance cannot reach the codearena or churn-ml projects that share this
# account even if something running on it tried to.
#
# Attached even when enable_ssh is true, as the fallback route in if the key
# is lost or the operator's address changes.
resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "instance" {
  name = "${local.name}-instance"
  role = aws_iam_role.instance.name
}

# ---------------------------------------------------------------------------
# Key pair
# ---------------------------------------------------------------------------

# Only the public key is read. A private key is never generated by Terraform
# here on purpose: tls_private_key would write the private half into
# terraform.tfstate in plaintext, where it would outlive the cluster.
resource "aws_key_pair" "operator" {
  count = var.enable_ssh ? 1 : 0

  key_name   = "${local.name}-operator"
  public_key = file(pathexpand(var.ssh_public_key_path))
}

# ---------------------------------------------------------------------------
# Secrets
# ---------------------------------------------------------------------------

# Database passwords are generated, not committed. They land in
# terraform.tfstate (which is gitignored and must be treated as a secret) and
# are written to the instance's env file by user_data.
#
# AWS Secrets Manager would be the production answer. It is not used here
# because it costs $0.40 per secret per month -- more than 1% of the entire
# monthly budget for two strings that live and die with a disposable cluster.
resource "random_password" "postgres" {
  length  = 32
  special = false # Goes into a DATABASE_URL; percent-encoding is a bug waiting to happen.
}

resource "random_password" "replication" {
  length  = 32
  special = false
}

resource "random_password" "grafana" {
  length  = 20
  special = false
}

# ---------------------------------------------------------------------------
# Storage
# ---------------------------------------------------------------------------

resource "aws_ebs_volume" "data" {
  availability_zone = aws_subnet.public.availability_zone
  size              = var.data_volume_gb
  type              = "gp3"

  # Encryption at rest with the AWS-managed key. gp3 encryption is free and
  # has no measurable performance cost, so there is no argument for leaving it
  # off. Note what it does NOT give: it protects the volume if the underlying
  # media is compromised, and does nothing about the durability gaps Phase 8
  # recorded -- archive_mode is still off, so there is still no PITR, and
  # data_checksums is still off, so silent corruption is still undetectable.
  encrypted = true

  tags = { Name = "${local.name}-data" }

  lifecycle {
    prevent_destroy = false
  }
}

resource "aws_volume_attachment" "data" {
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.engine.id

  # Without this, destroying the instance can leave the attachment in place
  # and the destroy hangs. With it, Terraform detaches first.
  force_detach = true

  # Honours data_volume_retain_on_destroy by skipping the detach-and-delete
  # path; the volume itself is kept by the operator afterwards.
  skip_destroy = var.data_volume_retain_on_destroy
}

# ---------------------------------------------------------------------------
# Instance
# ---------------------------------------------------------------------------

resource "aws_instance" "engine" {
  # insecure_value, not value. aws_ssm_parameter marks `value` sensitive
  # unconditionally, because a parameter CAN be a SecureString -- and a
  # sensitive value may not be exported from a root output, so `value` made
  # the ami_id output illegal. This one is a public AWS-published AMI id, so
  # `insecure_value` is the accurate reading: it asserts the parameter is not
  # a secret, which for /aws/service/ami-amazon-linux-latest/* is true.
  ami                    = data.aws_ssm_parameter.al2023.insecure_value
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.public.id
  vpc_security_group_ids = [aws_security_group.instance.id]
  iam_instance_profile   = aws_iam_instance_profile.instance.name
  key_name               = var.enable_ssh ? aws_key_pair.operator[0].key_name : null

  # No Elastic IP anywhere in this stack, and that is a cost decision. An EIP
  # is billed at $0.005/hr whether or not the instance is running, so an
  # allocated EIP keeps charging after the cluster is stopped -- the account
  # already has one such address doing exactly that. An auto-assigned public
  # IPv4 is billed only while the instance runs and is released on stop.
  #
  # The cost of that choice is that the address CHANGES every time the
  # instance is stopped and started, so the runbook reads it from the
  # Terraform output rather than hard-coding it anywhere.
  associate_public_ip_address = true

  root_block_device {
    volume_size           = var.root_volume_gb
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
    tags                  = { Name = "${local.name}-root" }
  }

  # IMDSv2 required, not optional. The v1 endpoint answers an unauthenticated
  # GET, so any process that can make an HTTP request from this instance --
  # including a server-side request forgery through something running in a
  # container -- can read the instance role's credentials. Requiring the
  # token turns that into a PUT the same bug usually cannot make.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2 # 2, not 1: a request from inside a container is one hop further than one from the host.
  }

  user_data_replace_on_change = true
  user_data = templatefile("${path.module}/user_data.sh.tftpl", {
    postgres_password = random_password.postgres.result
    repl_password     = random_password.replication.result
    grafana_password  = random_password.grafana.result
    # No data_device is passed. The attachment requests /dev/sdf, but t3 is a
    # Nitro type and the kernel names EBS volumes /dev/nvme*, so the script
    # discovers the device itself rather than trusting a name that may not
    # exist. Passing one would only invite someone to rely on it.
    data_dir = "/mnt/durexec-data"
  })

  tags = { Name = "${local.name}-engine" }
}
