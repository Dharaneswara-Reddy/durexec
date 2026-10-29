# Terraform, not CloudFormation or CDK, because the repository had no
# infrastructure tooling at all and terraform was already installed on the
# development host. It also has a real `plan` that can be inspected without
# touching the account, which is the one thing this phase needs most: every
# artifact here has to be reviewable without creating a single resource.
#
# Versions are pinned to a minimum rather than exactly so a newer patch
# release is usable, but the 5.x major is fixed: the AWS provider has
# repeatedly changed resource defaults across majors, and an unpinned major
# would let a `terraform init` months from now plan a different cluster from
# the same code.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    # Generates the PostgreSQL, replication and Grafana passwords so none of
    # them is ever committed. Their values live in terraform.tfstate, which is
    # gitignored and is a secret.
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # Local state on purpose. A remote S3 backend is the right answer for a team
  # and the wrong answer here: it needs a bucket and a lock table that exist
  # before anything else can be planned, which means billable resources and a
  # bootstrap step before the first deployment. This environment is a
  # single-operator, disposable test cluster. The state file is
  # terraform.tfstate in this directory and is gitignored -- it holds the
  # generated database password, so it is a secret.
  #
  # Consequence to be aware of: the state exists only on this laptop. Losing
  # it does not delete the AWS resources, it just orphans them, and teardown
  # then has to go through the tag-scoped script in scripts/aws/teardown.sh.
}
