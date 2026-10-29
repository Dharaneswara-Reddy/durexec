output "instance_id" {
  description = "Instance id, for `aws ssm start-session --target <id>` and for the teardown script."
  value       = aws_instance.engine.id
}

output "public_ip" {
  description = <<-D
    Auto-assigned public IPv4. It CHANGES whenever the instance is stopped and
    started, because this stack deliberately allocates no Elastic IP (an EIP
    would keep billing while the cluster is stopped). Always read it from here
    rather than remembering it.
  D
  value       = aws_instance.engine.public_ip
}

output "ami_id" {
  description = "The AMI the instance actually launched from. Recorded because the AMI is resolved from an SSM 'latest' parameter, so two applies weeks apart can differ, and a results file has to be able to say what it ran on."
  value       = data.aws_ssm_parameter.al2023.insecure_value
}

output "availability_zone" {
  description = "AZ of both the instance and the data volume. They must match for the volume to attach."
  value       = aws_subnet.public.availability_zone
}

output "data_volume_id" {
  description = "The EBS volume holding PGDATA. Needed to re-attach it to a replacement instance in the instance-loss test, and to delete it by hand if data_volume_retain_on_destroy was true."
  value       = aws_ebs_volume.data.id
}

output "ssh_command" {
  description = "Ready-to-paste SSH command. Empty when enable_ssh is false; use Session Manager instead."
  value       = var.enable_ssh ? "ssh -i ${replace(var.ssh_public_key_path, ".pub", "")} ec2-user@${aws_instance.engine.public_ip}" : ""
}

output "tunnel_command" {
  description = <<-D
    One SSH command that forwards every dashboard to localhost. This is the
    only way to reach them: the security group opens no HTTP port and every
    listener inside the instance is bound to 127.0.0.1.
      Grafana      http://localhost:3000
      Prometheus   http://localhost:9090
      node-a/b/c   http://localhost:8081 .. 8083
  D
  value = var.enable_ssh ? join(" ", [
    "ssh -N",
    "-L 3000:localhost:3000",
    "-L 9090:localhost:9090",
    "-L 8081:localhost:8081",
    "-L 8082:localhost:8082",
    "-L 8083:localhost:8083",
    "-i ${replace(var.ssh_public_key_path, ".pub", "")}",
    "ec2-user@${aws_instance.engine.public_ip}",
  ]) : ""
}

output "grafana_password" {
  description = "Generated Grafana admin password. Read it with `terraform output -raw grafana_password`."
  value       = random_password.grafana.result
  sensitive   = true
}

output "postgres_password" {
  description = "Generated PostgreSQL password for the durexec role. Needed by psql over the tunnel; read it with `terraform output -raw postgres_password`."
  value       = random_password.postgres.result
  sensitive   = true
}

output "hourly_cost_estimate_usd" {
  description = <<-D
    Arithmetic, not a quote. Instance on-demand rate plus both gp3 volumes
    plus one public IPv4, for the default t3.medium/20+20 GB configuration in
    us-east-1. It does NOT include data transfer, snapshots, or anything else
    already running in this shared account. See
    docs/cloud-deployment/COST-ESTIMATE.md for where each number came from and
    what could make it wrong.
  D
  value = format(
    "%.4f USD/hr while running (instance %s + %d GB gp3 + 1 public IPv4); verify against COST-ESTIMATE.md",
    lookup({ "t3.medium" = 0.0416, "t3.small" = 0.0208, "t4g.medium" = 0.0336, "t4g.small" = 0.0168 }, var.instance_type, 0.0416)
    + ((var.root_volume_gb + var.data_volume_gb) * 0.08 / 730)
    + 0.005,
    var.instance_type,
    var.root_volume_gb + var.data_volume_gb,
  )
}
