# fleet-ec2 owns everything billet's EC2 COMPUTE side needs and nothing the
# control plane does: the node IAM role and instance profile, the trusted-runner
# security group, the cache storage, and the spot interruption queue with its
# tag-scoped router. Independently composable — an operator with their own
# control plane consumes this child directly; the opinionated root composes it
# with control-plane-ec2-sqlite and attaches this child's instance profile to
# the controller for the co-located deployment.

data "aws_caller_identity" "this" {}

data "aws_partition" "this" {}

# THE REGION COMES FROM THE PROVIDER, not a variable, so the resources and the
# IAM conditions can never name a different region than they are created in.
data "aws_region" "this" {}

locals {
  account_id = data.aws_caller_identity.this.account_id
  region     = data.aws_region.this.region

  cache_bucket = var.cache_bucket != "" ? var.cache_bucket : "${var.name}-cache-${local.account_id}"

  # A customer-managed KMS key exists only when the cache is enabled AND requested.
  enable_cache_kms = var.enable_cache && var.enable_kms

  # THE SAME MARKER THE MONOLITH STAMPED, deliberately: moved blocks relocate
  # state but rewrite no attributes, so a per-child tag value would plan an
  # in-place update on every relocated resource and break the zero-change
  # upgrade contract. Per-child provenance, if ever wanted, is a separate,
  # announced change after that contract has been proved.
  tags = merge(var.tags, {
    "sh.billet.module" = "terraform-aws-billet"
    "Name"             = var.name
  })
}

# THE RUNNERS' SECURITY GROUP, for trusted work. Untrusted (fork) work needs a
# separately-described group that reaches only what a stranger's code should; the
# module leaves that to the operator rather than guess a safe boundary.
resource "aws_security_group" "runner" {
  name        = "${var.name}-runner"
  description = "billet trusted runner instances"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { "Name" = "${var.name}-runner" })
}

resource "aws_vpc_security_group_egress_rule" "runner_all" {
  security_group_id = aws_security_group.runner.id
  description       = "all egress (GitHub, the cache endpoint)"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}
