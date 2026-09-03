# control-plane-postgres runs ONE billet control plane whose ledger is somewhere
# else.
#
# It is control-plane-ec2-sqlite with the ledger volume removed, and the removal
# is the whole point: with `server.state.backend: postgres` the capacity ledger,
# node registrations, custody and job history live in a database this module does
# not operate, so the instance carries no data that has to survive it and can be
# rebuilt rather than recovered.
#
# ONE CONTROLLER. NOT TWO, AND NOT A FAILOVER.
#
# A capacity ledger with two writers admits the same slot twice and the machine
# that has to run both jobs finds out later, so billet takes a CLAIM row in the
# database before it polls GitHub — a second controller is refused at startup
# naming the machine that holds it, and a replaced one is fenced by the claim's
# epoch on its next write. That catches the accident. It is not high
# availability: nothing here decides a controller is dead or promotes a standby
# by itself, so this module launches exactly one instance and says so
# rather than implying otherwise with a count variable.
#
# WHAT STILL LIVES ON THIS MACHINE, AND THEREFORE STILL HAS TO BE BACKED UP:
# `server.identity_dir` holds the deployment identity, the node-wire CA and its
# rotation state, the process lock and the maintenance fence. A private key is
# not rows and local process coordination is not SQL, so none of it moved. There
# is deliberately no EBS volume for it — a volume would pin this instance to an
# availability zone and undo the replaceability the profile exists for — which
# makes `billet local backup` to the bucket below the recovery path for that half
# rather than a nicety. A ledger without its identity is a fresh authority that
# cannot see the compute the old one launched.

data "aws_partition" "this" {}

data "aws_region" "this" {}

locals {
  partition = data.aws_partition.this.partition
  region    = data.aws_region.this.region

  # Gated on the EXPLICIT bool, never inferred from the name: count cannot depend
  # on a value known only at apply, and a caller passing a computed profile name
  # could otherwise never plan.
  profile_name = var.create_instance_profile ? aws_iam_instance_profile.this[0].name : var.instance_profile_name

  tags = merge(var.tags, {
    "sh.billet.module" = "terraform-aws-billet"
    "Name"             = var.name
  })
}

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

# THE GRANT THAT LETS THIS HOST ASSEMBLE ITS OWN DSN.
#
# billet reads BILLET_STATE_DSN from the environment, and the password in it is
# never written by Terraform — state-rds-postgres hands over the ARN of AWS's own
# managed master credential and the policy that reads it, so the connection
# string is composed on the machine at install time rather than travelling
# through a plan, a state file or cloud-init.
#
# THE BOOL IS EXPLICIT for the same reason create_instance_profile is: the policy
# document names a secret ARN that does not exist until apply, so count cannot be
# derived from whether the string is empty.
resource "aws_iam_role_policy" "state_secrets" {
  count = var.create_state_secret_policy && var.create_instance_profile ? 1 : 0

  name   = "${var.name}-control-plane-state-secrets"
  role   = aws_iam_role.this[0].id
  policy = var.state_secret_policy_json
}

# THE CONTROL PLANE'S SECURITY GROUP. Nodes dial its node-wire port over mTLS, so
# the ingress is that port from node_ingress_cidrs (the VPC by default),
# optionally the enrollment port from bootstrap_ingress_cidrs (empty, so closed),
# and optionally SSH for the Ansible role to converge it.
#
# NOTHING HERE OPENS THE DATABASE. The ledger's ingress rule references THIS
# group and lives in state-rds-postgres, so the permission is expressed once, on
# the side that owns the database, in terms of identity rather than addressing.
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
  description       = "all egress (GitHub, package repos, the ledger)"
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
    # A newer Canonical AMI must not force a replacement of the controller. The
    # ledger is elsewhere, so a replacement loses no rows — but it DOES lose the
    # identity directory, which is why this is ignored here exactly as it is on
    # the SQLite profile. Replace the controller deliberately, with a restore of
    # the identity archive on the far side.
    ignore_changes = [ami]

    precondition {
      # An adopted subnet must be IN the VPC whose security groups the launch
      # uses. The caller resolves the fact; this precondition is where it fails
      # the plan.
      condition     = var.subnet_in_vpc_ok
      error_message = "subnet_id is not in vpc_id — an adopted subnet must belong to the adopted VPC, whose security groups the launch uses."
    }
  }

  # THE WHOLE MACHINE IS DISPOSABLE ON THIS PROFILE, so the root disk carries
  # everything and there is no second volume. What is NOT disposable is
  # server.identity_dir on it — see the file header, and set up the backup bucket.
  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.root_volume_gib
    encrypted             = true
    delete_on_termination = true
  }

  tags = merge(local.tags, { "Name" = "${var.name}-control-plane" })
}

# EC2 AUTO-RECOVERY, not an ASG (ADR-001). A system-status-check failure recovers
# the SAME instance onto new hardware, keeping its root volume — and therefore
# keeping the identity directory, which an ASG's fresh launch would not. The
# action is the built-in ec2:recover automation, and its ARN follows the
# PARTITION so this module is correct in GovCloud and China too.
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
