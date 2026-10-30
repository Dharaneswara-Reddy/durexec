variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "environment" {
  type    = string
  default = "bench"
}

# 10.91 avoids the single-instance campaign's 10.90.0.0/16, the shared
# account's 10.20.0.0/16 (aiops-demo) and the default 172.31.0.0/16.
variable "vpc_cidr" {
  type    = string
  default = "10.91.0.0/16"
}

variable "operator_cidr" {
  type        = string
  description = "Your own address with /32. Gates the Kubernetes API endpoint."
  validation {
    condition     = var.operator_cidr != "0.0.0.0/0" && can(cidrhost(var.operator_cidr, 0))
    error_message = "operator_cidr must be a valid CIDR and must not be 0.0.0.0/0."
  }
}

# Standard support only. 1.33 and older are already in EXTENDED support at
# $0.60/hr against $0.10/hr -- verified with `aws eks describe-cluster-versions`
# on 2026-10-29. Re-check before deploying: support windows move.
variable "kubernetes_version" {
  type    = string
  default = "1.37"
  validation {
    condition     = contains(["1.35", "1.36", "1.37"], var.kubernetes_version)
    error_message = "Pin a version in STANDARD support. 1.34 and older bill at the $0.60/hr extended rate; re-verify with `aws eks describe-cluster-versions`."
  }
}

variable "engine_instance_type" {
  type    = string
  default = "m7i-flex.large"
}

# The node-scaling experimental variable. Changed between runs.
variable "engine_node_count" {
  type    = number
  default = 3
  validation {
    condition     = var.engine_node_count >= 1 && var.engine_node_count <= 6
    error_message = "1 to 6 engine nodes."
  }
}

variable "harness_instance_type" {
  type        = string
  default     = "c7i-flex.large"
  description = "Load generator plus Prometheus. Never shares a node with the engine."
}

variable "enable_control_plane_logs" {
  type        = bool
  default     = false
  description = "CloudWatch storage costs money; enable only to debug the control plane."
}

variable "extra_tags" {
  type    = map(string)
  default = {}
}

# --- external database host ------------------------------------------------

variable "db_instance_type" {
  type        = string
  default     = "m7i-flex.large"
  description = "PostgreSQL host. Dedicated so its fsync latency is measurable."
}

variable "db_volume_gb" {
  type    = number
  default = 30
}

variable "db_max_connections" {
  type        = number
  default     = 400
  description = "Must cover engine_pods x (20 pool + 4 renewal) + headroom. Compose's 100 caps the cluster at three nodes."
}

variable "db_key_name" {
  type        = string
  default     = null
  description = "Existing EC2 key pair name for SSH to the database host. null means no SSH key."
}

variable "enable_db_ssh" {
  type    = bool
  default = false
}

# --- optional in-VPC operator ------------------------------------------------

variable "enable_operator" {
  type        = bool
  default     = false
  description = "Create an in-VPC operator instance reached through SSM, for running the harness when the operator's own machine cannot reach the Kubernetes API. Off for laptop runs."
}

variable "operator_instance_type" {
  type    = string
  default = "c7i-flex.large"
}

variable "campaign_bucket" {
  type        = string
  default     = ""
  description = "S3 bucket holding remote state and evidence. The operator may read the state object and read/write the evidence prefix only."
}
