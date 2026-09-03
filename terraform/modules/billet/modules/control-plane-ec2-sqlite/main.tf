# control-plane-ec2-sqlite owns billet's control plane and nothing the compute
# side does. ADR-001: a single small EC2 with SQLite on a dedicated retained gp3
# EBS volume, recovered by EC2 auto-recovery — NOT an ASG, which would launch a
# fresh instance that does not reattach the data volume. A dead controller
# delays CI rather than failing it (GitHub queues a job for 24h), so the
# requirement is "recovers in minutes".
#
# This module provisions the instance, its security group, its ledger volume and
# its identity; the junioryono.billet.host Ansible role installs the binary,
# billet.yaml, certificates and units on it. Independently composable: alone it
# creates a minimal own instance profile; the opinionated root passes fleet-ec2's
# instead, so the co-located controller runs billet with the node role.

data "aws_partition" "this" {}

data "aws_region" "this" {}

locals {
  partition = data.aws_partition.this.partition
  region    = data.aws_region.this.region

  # Gated on the EXPLICIT bool, never inferred from the name: count cannot
  # depend on a value known only at apply, and a caller passing a computed
  # profile name could otherwise never plan.
  profile_name = var.create_instance_profile ? aws_iam_instance_profile.this[0].name : var.instance_profile_name

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

# THE MINIMAL OWN IDENTITY, created only when the caller supplied none: a bare
# EC2 trust role carrying no policies, so a standalone control plane has an
# instance profile to hang operator-attached policies on without this module
# granting anything. jsonencode, not the provider's policy-document data
# source, so the partition-following principal stays assertable under the
# mocked test provider.
resource "aws_iam_role" "this" {
  count = var.create_instance_profile ? 1 : 0

  name = "${var.name}-control-plane"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "ec2.${data.aws_partition.this.dns_suffix}" }
    }]
  })
  tags = local.tags
}

resource "aws_iam_instance_profile" "this" {
  count = var.create_instance_profile ? 1 : 0

  name = "${var.name}-control-plane"
  role = aws_iam_role.this[0].name
  tags = local.tags
}

# THE CONTROL PLANE'S SECURITY GROUP. Nodes dial its node-wire port over mTLS,
# so the ingress is that port from node_ingress_cidrs (the VPC by default),
# optionally the enrollment port from bootstrap_ingress_cidrs (empty, so closed),
# and, optionally, SSH for the Ansible role to converge it. The cache-endpoint
# rule is NOT here: it references the runner group, which belongs to fleet-ec2,
# so the cross-child rule lives in the root that knows both.
resource "aws_security_group" "control_plane" {
  name        = "${var.name}-control-plane"
  description = "billet control plane: node wire and optional SSH"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { "Name" = "${var.name}-control-plane" })
}

resource "aws_vpc_security_group_ingress_rule" "node_wire" {
  for_each = toset(length(var.node_ingress_cidrs) > 0 ? var.node_ingress_cidrs : [var.vpc_cidr])

  security_group_id = aws_security_group.control_plane.id
  description       = "billet node wire (mTLS)"
  cidr_ipv4         = each.value
  from_port         = var.listen_port
  to_port           = var.listen_port
  ip_protocol       = "tcp"
}

# ENROLLMENT, ON ITS OWN PORT AND NORMALLY CLOSED. billet serves /v1/ca and
# /v1/enroll here rather than on the node wire, because neither can require a
# client certificate and a listener that admits strangers must not share a
# connection budget with the fleet. No CIDRs means no rule: a node that
# has enrolled never dials this again, so the steady state is shut.
resource "aws_vpc_security_group_ingress_rule" "node_bootstrap" {
  for_each = toset(var.bootstrap_ingress_cidrs)

  security_group_id = aws_security_group.control_plane.id
  description       = "billet node enrollment (no client certificate; open only while adding a machine)"
  cidr_ipv4         = each.value
  from_port         = var.bootstrap_port
  to_port           = var.bootstrap_port
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_ingress_rule" "ssh" {
  for_each = toset(var.ssh_ingress_cidrs)

  security_group_id = aws_security_group.control_plane.id
  description       = "SSH for configuration management"
  cidr_ipv4         = each.value
  from_port         = 22
  to_port           = 22
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "control_plane_all" {
  security_group_id = aws_security_group.control_plane.id
  description       = "all egress (GitHub, package repos)"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

data "aws_ami" "ubuntu" {
  count = var.ami == "" ? 1 : 0

  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-${var.architecture == "arm64" ? "arm64" : "amd64"}-server-*"]
  }

  filter {
    name   = "architecture"
    values = [var.architecture]
  }

  filter {
    name   = "state"
    values = ["available"]
  }
}

resource "aws_instance" "control_plane" {
  ami                    = var.ami != "" ? var.ami : data.aws_ami.ubuntu[0].id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = [aws_security_group.control_plane.id]
  iam_instance_profile   = local.profile_name
  key_name               = var.key_name != "" ? var.key_name : null

  metadata_options {
    http_tokens   = "required" # IMDSv2 only
    http_endpoint = "enabled"
  }

  lifecycle {
    # A newer Canonical AMI must not force a replacement of the controller: the OS
    # is updated in place by the Ansible role, and the ledger's durability is the
    # dedicated volume's job, not this instance's.
    ignore_changes = [ami]

    precondition {
      # An adopted subnet must be IN the VPC whose security groups the launch
      # uses. The caller resolves the fact (the root's adopted-subnet lookup);
      # this precondition is where it fails the plan.
      condition     = var.subnet_in_vpc_ok
      error_message = "subnet_id is not in vpc_id — an adopted subnet must belong to the adopted VPC, whose security groups the launch uses."
    }
  }

  # OS ONLY. The SQLite ledger lives on the dedicated, retained aws_ebs_volume.ledger
  # below (ADR-001 / the deployment guide), so it survives instance termination —
  # not on this disposable root.
  root_block_device {
    volume_type           = "gp3"
    volume_size           = 16
    encrypted             = true
    delete_on_termination = true
  }

  tags = merge(local.tags, { "Name" = "${var.name}-control-plane" })
}

# EC2 AUTO-RECOVERY, not an ASG. A system-status-check failure recovers the same
# instance onto new hardware WITH its EBS volume, which is what keeps the SQLite
# ledger. The action is the built-in ec2:recover automation.
resource "aws_cloudwatch_metric_alarm" "control_plane_recover" {
  alarm_name          = "${var.name}-control-plane-recover"
  namespace           = "AWS/EC2"
  metric_name         = "StatusCheckFailed_System"
  statistic           = "Maximum"
  comparison_operator = "GreaterThanOrEqualToThreshold"
  threshold           = 1
  period              = 60
  evaluation_periods  = 2
  alarm_actions       = ["arn:${local.partition}:automate:${local.region}:ec2:recover"]
  dimensions = {
    InstanceId = aws_instance.control_plane.id
  }
  tags = local.tags
}

# THE LEDGER VOLUME. ADR-001 and the deployment guide keep SQLite on a DEDICATED,
# retained encrypted gp3 volume so the control plane's state survives the instance:
# an EBS volume is a separate resource, kept when the instance is terminated or
# replaced, and prevent_destroy makes any plan that WOULD destroy it (a cross-AZ
# move, or a casual `terraform destroy`) fail closed. The fail-closed MOUNT is the
# configuration layer's job: the junioryono.billet.host role mounts it by its own
# NVMe identity when billet_ledger_volume_id is set to this module's output.
resource "aws_ebs_volume" "ledger" {
  availability_zone = var.availability_zone
  size              = var.volume_gib
  type              = "gp3"
  encrypted         = true
  tags              = merge(local.tags, { "Name" = "${var.name}-ledger" })

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_volume_attachment" "ledger" {
  device_name                    = "/dev/sdf"
  volume_id                      = aws_ebs_volume.ledger.id
  instance_id                    = aws_instance.control_plane.id
  stop_instance_before_detaching = true
}
