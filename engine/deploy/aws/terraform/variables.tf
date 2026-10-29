variable "aws_region" {
  description = <<-D
    Region to deploy into. us-east-1 is the default because it is AWS's
    cheapest region for every resource this stack uses: at the time of
    writing the account's own Pricing API returned $0.0416/hr for t3.medium
    in us-east-1, and it is also where the credentials in the aiops-deploy
    profile are already pointed. ap-south-1 (Mumbai) is physically closer and
    would cut dashboard latency, but costs more per hour and the engine's
    correctness is unaffected by operator latency -- every path the tests
    exercise is inside the instance.
  D
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "Short environment name. Goes into every resource name and tag, so two deployments can coexist without colliding."
  type        = string
  default     = "lab"

  validation {
    # Resource names are built from this, and several AWS name fields reject
    # anything but alphanumerics and hyphens.
    condition     = can(regex("^[a-z0-9-]{1,16}$", var.environment))
    error_message = "environment must be 1-16 characters of lowercase letters, digits or hyphens."
  }
}

variable "instance_type" {
  description = <<-D
    t3.medium (2 vCPU, 4 GiB) is the default, and the memory is why.

    The stack is three engine nodes at a 300 MiB limit each, two PostgreSQL
    servers at 500 MiB, Prometheus at 300 MiB and Grafana at 250 MiB: about
    2.25 GiB of limits, plus roughly 550 MiB for Amazon Linux and the Docker
    daemon. That is ~2.8 GiB, which does not fit a 2 GiB t3.small once the
    streaming standby is included, and dropping the standby would remove the
    database failover test from the acceptance matrix.

    t4g.medium is the same memory on Graviton for about 19% less
    ($0.0336/hr against $0.0416/hr, from the account's Pricing API). It is
    NOT the default because every image validated in Phases 6 to 8 was built
    for linux/amd64, and switching architecture the night before a first
    deployment means re-validating the engine image and pgvector/pgvector:pg16
    on arm64 at the same time as everything else is being tried for the first
    time. The saving is about $5.84 a month. Switch to it once an arm64 build
    has been smoke-tested; the engine is pure Go with CGO_ENABLED=0, so it
    cross-compiles without changes.
  D
  type        = string
  default     = "t3.medium"
}

variable "operator_cidr" {
  description = <<-D
    The ONLY CIDR allowed to reach port 22. There is deliberately no default:
    Terraform refuses to plan until it is given, which is what stops an
    absent-minded run from opening SSH to the internet. Set it to your own
    address with a /32, for example "203.0.113.4/32"; `curl -s
    https://checkip.amazonaws.com` prints it.
  D
  type        = string

  validation {
    # 0.0.0.0/0 is rejected outright rather than warned about. This instance
    # runs a simulator whose /arm endpoint can make a payment processor fail
    # on demand, and a fault injector that can kill the engine. None of that
    # is published to a public interface (see
    # deploy/aws/docker-compose.cloud.yml), so an open SSH port would be the
    # only way in -- which is exactly why it must not be open.
    condition     = var.operator_cidr != "0.0.0.0/0" && can(cidrhost(var.operator_cidr, 0))
    error_message = "operator_cidr must be a valid CIDR and must not be 0.0.0.0/0. Use your own address with a /32 suffix."
  }
}

variable "enable_ssh" {
  description = <<-D
    Whether to create the port 22 ingress rule at all. Session Manager is
    attached regardless (see the IAM role), so `aws ssm start-session` works
    with no inbound rule whatsoever and this can be set to false for a
    zero-ingress deployment.

    It defaults to true because SSM is the stricter configuration but also the
    one with no fallback: if the agent fails to register with the SSM service,
    an instance with no ingress rule is an instance nobody can log into, and
    the only recovery is to terminate it. For a first deployment, having a
    working way in matters more.
  D
  type        = bool
  default     = true
}

variable "vpc_cidr" {
  description = <<-D
    CIDR for the VPC this stack creates.

    A dedicated VPC rather than the default one, because the target account is
    shared: it already holds the codearena, churn-ml-prod and aiops-demo
    projects, a non-default VPC on 10.20.0.0/16 and an associated Elastic IP.
    Putting a test cluster that deliberately kills processes into a VPC with
    someone else's work is how an experiment becomes an outage. 10.90.0.0/16
    is chosen not to overlap 10.20.0.0/16 or the default 172.31.0.0/16.
  D
  type        = string
  default     = "10.90.0.0/16"
}

variable "ssh_public_key_path" {
  description = <<-D
    Path to the PUBLIC half of an SSH key pair, uploaded as an EC2 key pair.
    Only the public key is ever read, and only the public key is sent to AWS;
    the private key never leaves the machine and is never read by Terraform.
    Generate one with:
      ssh-keygen -t ed25519 -f ~/.ssh/durexec-aws -C durexec-aws
  D
  type        = string
  default     = "~/.ssh/durexec-aws.pub"
}

variable "root_volume_gb" {
  description = "Root EBS volume size. Holds the OS, Docker and the loaded images (~100 MB of images), not the database."
  type        = number
  default     = 20
}

variable "data_volume_gb" {
  description = <<-D
    Size of the SEPARATE EBS volume that holds PGDATA, the Prometheus TSDB and
    collected evidence.

    It is separate from the root volume for one reason that is worth the extra
    $1.60 a month: it survives instance termination. That is what turns
    "what happens when the machine dies?" into an experiment -- terminate the
    instance, attach this volume to its replacement, and see whether committed
    work came back. On minikube the database lived on a hostPath on the one VM's
    disk, so instance loss was recorded as NOT TESTED in Phase 8. This is the
    cheapest change that closes it.
  D
  type        = number
  default     = 20
}

variable "data_volume_retain_on_destroy" {
  description = <<-D
    When true, `terraform destroy` leaves the data volume behind.

    Default false, so the documented teardown really does stop all charges --
    a retained 20 GB gp3 volume is $1.60 a month forever and is precisely the
    kind of thing that is forgotten. Set it to true before a destroy that is
    meant to preserve journal state for a later restore test, and remember
    that the volume then has to be deleted by hand.
  D
  type        = bool
  default     = false
}

variable "extra_tags" {
  description = "Additional tags merged into every resource, for cost attribution beyond the defaults."
  type        = map(string)
  default     = {}
}
