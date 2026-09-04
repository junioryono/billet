# terraform-aws-billet provisions the AWS infrastructure a billet deployment
# needs: the network, the control-plane instance, the cache storage, and the IAM
# role the compute assumes. It does NOT own billet.yaml, the mTLS certificates, or
# the systemd units — ADR-004 keeps those with the configuration-management layer
# (the junioryono.billet.host Ansible role). The module hands that layer the
# non-secret facts it needs as outputs; enrollment (a human fingerprint
# comparison) stays outside terraform apply.
#
# THE ROOT IS THE OPINIONATED COMPOSITION of two independently consumable
# children — modules/control-plane-ec2-sqlite and modules/fleet-ec2 — plus the
# network they share. An operator who has one half already (their own control
# plane, or their own compute IAM story) consumes the other child directly; the
# root exists for the co-located deployment, where it attaches fleet-ec2's
# instance profile to the controller so one machine runs billet with the node
# role. Its input/output contract is preserved across the split: every
# pre-split variable and output keeps its name and meaning, and moved.tf maps
# every relocated resource so an existing deployment plans ZERO changes.
#
# It works in ANY AWS partition. The committed IAM rendering carries TFPARTITION
# and TFDNSSUFFIX sentinels that fleet-ec2 rewrites from data.aws_partition, so
# the same rendering serves the commercial partition, GovCloud and China (whose
# ARNs and amazonaws.com.cn service suffix differ).

data "aws_region" "this" {}

# Adopted network facts, resolved so ingress can default to the real VPC CIDR and
# the cache availability zone can be reported to billet's node.ebs_s3 config.
data "aws_vpc" "adopted" {
  count = local.create_vpc ? 0 : 1
  id    = var.vpc_id
}

data "aws_subnet" "adopted" {
  count = local.create_subnet ? 0 : 1
  id    = var.subnet_id
}

locals {
  create_vpc    = var.vpc_id == ""
  create_subnet = var.subnet_id == ""

  vpc_id    = local.create_vpc ? aws_vpc.this[0].id : var.vpc_id
  subnet_id = local.create_subnet ? aws_subnet.this[0].id : var.subnet_id

  # The VPC CIDR, the subnet's CIDR and its availability zone, from whichever
  # side is real. The subnet CIDR is what proves a declared controller address
  # at plan time.
  vpc_cidr          = local.create_vpc ? var.vpc_cidr : data.aws_vpc.adopted[0].cidr_block
  subnet_cidr       = local.create_subnet ? var.subnet_cidr : data.aws_subnet.adopted[0].cidr_block
  availability_zone = local.create_subnet ? aws_subnet.this[0].availability_zone : data.aws_subnet.adopted[0].availability_zone

  region = data.aws_region.this.region

  tags = merge(var.tags, {
    "sh.billet.module" = "terraform-aws-billet"
    "Name"             = var.name
  })

  # THE COMMITTED CLASSIFICATION, READ RATHER THAN RESTATED. It is a file on
  # purpose: scripts/tfclassify joins the same bytes to `terraform show -json` to
  # gate an apply, and internal/tfclass's test refuses an entry for a resource no
  # module declares — or a module resource with no entry. A copy in HCL would be
  # the second source of truth those tests exist to prevent.
  #
  # The `_comment` key is the table's own prose. It is dropped here for the same
  # reason tfclass drops it: an operator reading `terraform output` wants the
  # classification, not the essay explaining it.
  operation_classes = {
    for key, entry in jsondecode(file("${path.module}/classification.json")) :
    key => entry if key != "_comment"
  }
}

module "fleet" {
  source = "./modules/fleet-ec2"

  name                          = var.name
  vpc_id                        = local.vpc_id
  enable_cache                  = var.enable_cache
  cache_bucket                  = var.cache_bucket
  cache_prefix                  = var.cache_prefix
  enable_kms                    = var.enable_kms
  enable_spot                   = var.enable_spot
  iam_policy_json               = var.iam_policy_json
  job_instance_profile_role_arn = var.job_instance_profile_role_arn
  tags                          = var.tags
}

module "control_plane" {
  source = "./modules/control-plane-ec2-sqlite"

  name              = var.name
  vpc_id            = local.vpc_id
  subnet_id         = local.subnet_id
  availability_zone = local.availability_zone
  vpc_cidr          = local.vpc_cidr
  # An adopted subnet must be IN the adopted VPC; when both are created this is
  # trivially true. Resolved here because the adopted lookup lives here.
  subnet_in_vpc_ok = local.create_subnet ? true : data.aws_subnet.adopted[0].vpc_id == var.vpc_id

  # THE ADDRESS, DECLARED, and the CIDR that proves it at plan. Resolved here
  # for the same reason subnet_in_vpc_ok is: the child cannot look the subnet
  # up without deferring the read through the instance.
  private_ip  = var.control_plane_private_ip
  subnet_cidr = local.subnet_cidr

  instance_type           = var.control_plane_instance_type
  ami                     = var.control_plane_ami
  architecture            = var.control_plane_architecture
  volume_gib              = var.control_plane_volume_gib
  listen_port             = var.control_plane_listen_port
  node_ingress_cidrs      = var.node_ingress_cidrs
  bootstrap_port          = var.control_plane_bootstrap_port
  bootstrap_ingress_cidrs = var.bootstrap_ingress_cidrs
  ssh_ingress_cidrs       = var.ssh_ingress_cidrs
  key_name                = var.key_name

  # THE CO-LOCATED OPINION: the controller runs billet itself, so it carries
  # the fleet's node role rather than a bare own identity — and the backup
  # grant has to follow it there, or the co-located controller gets a bucket
  # nobody may write to. The role NAME is plan-known, which is what lets the
  # grant's count stay decidable.
  create_instance_profile    = false
  instance_profile_name      = module.fleet.instance_profile_name
  instance_profile_role_name = module.fleet.node_role_name

  create_backup_bucket  = var.create_backup_bucket
  backup_bucket         = var.backup_bucket
  backup_prefix         = var.backup_prefix
  backup_kms_key_arn    = var.backup_kms_key_arn
  backup_retention_days = var.backup_retention_days

  tags = var.tags
}

# THE CACHE ENDPOINT — the one cross-child rule, living where both sides are
# known. When a cache is enabled the control plane serves guests an HTTPS cache
# listener; runners reach it, and ONLY runners, so this is an SG-to-SG rule
# from the runner group rather than a CIDR.
resource "aws_vpc_security_group_ingress_rule" "cache" {
  count = var.enable_cache ? 1 : 0

  security_group_id            = module.control_plane.security_group_id
  description                  = "billet EC2 cache endpoint (from runners)"
  referenced_security_group_id = module.fleet.runner_security_group_id
  from_port                    = var.cache_listen_port
  to_port                      = var.cache_listen_port
  ip_protocol                  = "tcp"
}
