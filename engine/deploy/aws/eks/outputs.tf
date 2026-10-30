output "cluster_name" { value = aws_eks_cluster.main.name }
output "cluster_version" { value = aws_eks_cluster.main.version }
output "cluster_endpoint" { value = aws_eks_cluster.main.endpoint }

output "kubeconfig_command" {
  value = "aws eks update-kubeconfig --region ${var.aws_region} --name ${aws_eks_cluster.main.name}"
}

output "hourly_cost_estimate_usd" {
  value = format(
    "control plane $0.10 + %d x %s engine + 1 x %s harness; verify against INSTANCE-COMPARISON.md. EXCLUDES the external PostgreSQL EC2 host.",
    var.engine_node_count, var.engine_instance_type, var.harness_instance_type
  )
}

output "teardown_warning" {
  value = "terraform destroy leaves nothing billable EXCEPT any EBS volume a released PersistentVolume left behind, and any snapshot. Check both before declaring the campaign over."
}

# --- database endpoint, consumed by scripts/aws/eks_deploy.sh ---------------

# PRIVATE address, never the public one. Engine pods reach the database inside
# the VPC; routing that traffic over the public IP would leave the VPC, cost
# transfer, and widen the exposure for no gain.
output "database_private_ip" {
  value       = aws_instance.database.private_ip
  description = "Private IPv4 of the PostgreSQL host. Feeds DATABASE_URL."
}

output "database_instance_id" {
  value = aws_instance.database.id
}

# The password is no longer a Terraform output. eks_deploy.sh reads it from
# Secrets Manager by ARN, so `terraform output` cannot print it.
output "database_secret_arn" {
  value       = aws_secretsmanager_secret.db.arn
  description = "Secrets Manager secret holding postgres_password and replication_password."
}

output "database_url_template" {
  value     = "postgres://durexec:<password>@${aws_instance.database.private_ip}:5432/durexec?sslmode=disable"
  sensitive = false
}
