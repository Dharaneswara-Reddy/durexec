# ---------------------------------------------------------------------------
# External PostgreSQL host.
#
# OUTSIDE Kubernetes, deliberately, for the first campaign:
#   * the study is about ENGINE scaling, and a Postgres-on-Kubernetes
#     streaming-replication implementation is a prerequisite nobody needs yet;
#   * database CPU, fsync latency and connection pressure are measurable on a
#     host that is doing nothing else;
#   * it reuses deploy/postgres/ assets whose replication this project has
#     already verified streaming, with a slot, on Compose.
#
# PILOT SCOPE: ONE primary, NO standby. This is a single point of failure and
# the pilot must not claim database high availability. The standby, promotion
# and stale-primary fencing belong to a later campaign; see
# docs/cloud-deployment/KUBERNETES-TEST-PLAN.md.
# ---------------------------------------------------------------------------

data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

resource "aws_security_group" "database" {
  name        = "durexec-${var.environment}-db"
  description = "PostgreSQL reachable only from EKS worker nodes"
  vpc_id      = aws_vpc.main.id
  tags        = { Name = "durexec-${var.environment}-db" }
}

# 5432 from the EKS cluster security group ONLY. Not from a CIDR, not from the
# VPC range: a security-group source means only instances EKS actually placed
# in that group can connect, so another project in this shared account cannot
# reach the database even if it lands in a neighbouring subnet.
resource "aws_vpc_security_group_ingress_rule" "db_from_nodes" {
  security_group_id            = aws_security_group.database.id
  referenced_security_group_id = aws_eks_cluster.main.vpc_config[0].cluster_security_group_id
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
  description                  = "PostgreSQL from EKS nodes"
}

resource "aws_vpc_security_group_ingress_rule" "db_ssh" {
  count             = var.enable_db_ssh ? 1 : 0
  security_group_id = aws_security_group.database.id
  cidr_ipv4         = var.operator_cidr
  from_port         = 22
  to_port           = 22
  ip_protocol       = "tcp"
  description       = "SSH from the operator only"
}

resource "aws_vpc_security_group_egress_rule" "db_all" {
  security_group_id = aws_security_group.database.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
  description       = "pull the postgres image"
}

resource "random_password" "postgres" {
  length  = 32
  special = false
}

resource "random_password" "replication" {
  length  = 32
  special = false
}

# PGDATA on its own volume so the journal survives the instance being
# terminated, which is what makes an instance-loss test possible later without
# destroying the evidence it collects.
resource "aws_ebs_volume" "pgdata" {
  availability_zone = aws_subnet.public[0].availability_zone
  size              = var.db_volume_gb
  type              = "gp3"
  encrypted         = true
  tags              = { Name = "durexec-${var.environment}-pgdata" }

  lifecycle {
    prevent_destroy = false
  }
}

resource "aws_volume_attachment" "pgdata" {
  device_name  = "/dev/sdf"
  volume_id    = aws_ebs_volume.pgdata.id
  instance_id  = aws_instance.database.id
  force_detach = true
}

resource "aws_instance" "database" {
  ami                         = data.aws_ssm_parameter.al2023.insecure_value
  instance_type               = var.db_instance_type
  subnet_id                   = aws_subnet.public[0].id
  vpc_security_group_ids      = [aws_security_group.database.id]
  key_name                    = var.db_key_name
  associate_public_ip_address = true

  # No Elastic IP: an allocated EIP bills while the instance is stopped,
  # an auto-assigned address does not.

  root_block_device {
    volume_size = 20
    volume_type = "gp3"
    encrypted   = true
  }

  metadata_options {
    http_tokens                 = "required"
    http_endpoint               = "enabled"
    http_put_response_hop_limit = 2
  }

  user_data = templatefile("${path.module}/db_user_data.sh.tftpl", {
    postgres_password    = random_password.postgres.result
    replication_password = random_password.replication.result
    max_connections      = var.db_max_connections
  })

  tags = { Name = "durexec-${var.environment}-db" }
}
