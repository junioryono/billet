# control-plane-ec2-sqlite's plan tests, against the child directly. Same
# mocked provider: no credentials, nothing created.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }

  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_data "aws_ami" {
    defaults = {
      id = "ami-0123456789abcdef0"
    }
  }
}

run "standalone_creates_its_own_minimal_identity" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
  }

  # Standalone, the child mints a bare identity so the operator has a profile
  # to hang policies on — and its trust policy follows the partition, asserted
  # as the complete document.
  assert {
    condition     = length(aws_iam_role.this) == 1 && length(aws_iam_instance_profile.this) == 1
    error_message = "a standalone control plane should create its own minimal profile"
  }
  assert {
    condition     = aws_instance.control_plane.iam_instance_profile == "billet-test-control-plane"
    error_message = "the own minimal profile must actually be the one attached to the instance"
  }
  assert {
    condition = jsondecode(aws_iam_role.this[0].assume_role_policy) == {
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Action    = "sts:AssumeRole"
        Principal = { Service = "ec2.amazonaws.com" }
      }]
    }
    error_message = "the own trust policy must be exactly one Allow of sts:AssumeRole to the partition's EC2 service principal"
  }

  # The ledger is a dedicated, retained, encrypted gp3 volume; the root is OS-only.
  assert {
    condition     = aws_ebs_volume.ledger.encrypted && aws_ebs_volume.ledger.type == "gp3"
    error_message = "the ledger volume must be an encrypted gp3 volume"
  }
  assert {
    condition     = aws_ebs_volume.ledger.size == 20
    error_message = "the ledger volume should size from volume_gib (default 20)"
  }
  assert {
    condition     = aws_ebs_volume.ledger.availability_zone == "us-east-1a"
    error_message = "the ledger volume must be created in the given availability zone"
  }
  assert {
    condition     = aws_volume_attachment.ledger.device_name == "/dev/sdf"
    error_message = "the ledger volume should attach at /dev/sdf"
  }
  assert {
    condition     = one(aws_instance.control_plane.root_block_device).volume_size == 16
    error_message = "the instance root should be OS-only (16 GiB); the ledger lives on the dedicated volume"
  }

  # THE PREMISE EC2 AUTO-RECOVERY DEPENDS ON, asserted beside the volume rather
  # than beside the alarm. Recovery keeps the instance's volumes, which is only
  # worth anything because the ledger is a SEPARATE retained volume rather than
  # part of a root disk that goes with a terminate.
  assert {
    condition     = one(aws_instance.control_plane.root_block_device).delete_on_termination == true
    error_message = "the root disk should be disposable; if the ledger lived on it, recovery would be the only thing between a terminate and losing the deployment"
  }
  assert {
    condition     = aws_volume_attachment.ledger.stop_instance_before_detaching == true
    error_message = "the ledger attachment must stop the instance before detaching, or a detach races a live SQLite writer"
  }

  # The AMI lookup is a QUERY the mock never executes, so its text is pinned:
  # an equality on owners (with most_recent, an appended owner could supply the
  # selected image) and the full name pattern.
  assert {
    condition     = tolist(data.aws_ami.ubuntu[0].owners) == tolist(["099720109477"])
    error_message = "the AMI lookup must be scoped to exactly Canonical's owner id"
  }
  assert {
    condition     = data.aws_ami.ubuntu[0].most_recent == true
    error_message = "the AMI lookup must select the most recent matching image"
  }
  assert {
    condition = anytrue([
      for f in data.aws_ami.ubuntu[0].filter :
      f.name == "name" && contains(tolist(f.values), "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*")
    ])
    error_message = "the AMI lookup must ask for the Ubuntu 24.04 gp3 image by its full name pattern"
  }

  # The default node-wire ingress opens exactly the given VPC CIDR.
  assert {
    condition     = contains(keys(aws_vpc_security_group_ingress_rule.node_wire), "10.0.0.0/16")
    error_message = "the node-wire ingress must default to the vpc's cidr"
  }

  # AND THE ENROLLMENT PORT IS SHUT. It is the one surface that admits a caller
  # with no certificate, and nothing a running fleet does goes through it, so
  # the steady state is no rule at all. A default that opened it -- even to the
  # VPC, as the node wire does -- would be a listener nobody asked for.
  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.node_bootstrap) == 0
    error_message = "the enrollment port must be closed unless bootstrap_ingress_cidrs names something"
  }
}

# AND WHEN IT IS OPENED, ON EXACTLY THE ENROLLMENT PORT AND NOTHING ELSE.
#
# Both directions, because a rule that opened the wrong port, or the node wire's
# port, would satisfy the "closed by default" assertion above unchanged.
run "opening_enrollment_opens_only_the_enrollment_port" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                    = "billet-test"
    vpc_id                  = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id               = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone       = "us-east-1a"
    vpc_cidr                = "10.0.0.0/16"
    subnet_in_vpc_ok        = true
    listen_port             = 7717
    bootstrap_port          = 7718
    bootstrap_ingress_cidrs = ["203.0.113.7/32"]
  }

  assert {
    condition = (
      length(aws_vpc_security_group_ingress_rule.node_bootstrap) == 1 &&
      aws_vpc_security_group_ingress_rule.node_bootstrap["203.0.113.7/32"].from_port == 7718 &&
      aws_vpc_security_group_ingress_rule.node_bootstrap["203.0.113.7/32"].to_port == 7718 &&
      aws_vpc_security_group_ingress_rule.node_bootstrap["203.0.113.7/32"].ip_protocol == "tcp"
    )
    error_message = "opening enrollment must create exactly one rule, on bootstrap_port/tcp, for the named cidr"
  }

  # The node wire keeps its own rule and its own port: opening enrollment is not
  # a way to widen the wire.
  assert {
    condition = (
      length(aws_vpc_security_group_ingress_rule.node_wire) == 1 &&
      aws_vpc_security_group_ingress_rule.node_wire["10.0.0.0/16"].from_port == 7717
    )
    error_message = "the node-wire rule must be unchanged by opening the enrollment port"
  }
}

# THE OTHER AMI BRANCH: the root's default architecture is x86_64, so the
# branch every root consumer takes must be pinned too — the lookup is the one
# query the mock cannot execute.
run "pins_the_amd64_ami_pattern" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
    architecture      = "x86_64"
  }

  assert {
    condition = anytrue([
      for f in data.aws_ami.ubuntu[0].filter :
      f.name == "name" && contains(tolist(f.values), "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*")
    ])
    error_message = "the x86_64 branch must ask for the amd64 image by its full name pattern"
  }
}

# THE COMPOSED SHAPE: a supplied instance profile is attached verbatim and the
# own identity is NOT created — this is what lets the root put fleet-ec2's node
# role on the co-located controller.
run "composed_attaches_the_supplied_profile" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                    = "billet-test"
    vpc_id                  = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id               = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone       = "us-east-1a"
    vpc_cidr                = "10.0.0.0/16"
    subnet_in_vpc_ok        = true
    create_instance_profile = false
    instance_profile_name   = "billet-test-node"
  }

  assert {
    condition     = length(aws_iam_role.this) == 0 && length(aws_iam_instance_profile.this) == 0
    error_message = "a supplied profile must suppress the own identity entirely"
  }
  assert {
    condition     = aws_instance.control_plane.iam_instance_profile == "billet-test-node"
    error_message = "the supplied profile must be the one attached"
  }
}

# A NAME SUPPLIED ALONGSIDE create_instance_profile = true would be silently
# ignored — the caller believes their profile's policies are attached while the
# bare own identity is. Refused at validation instead.
run "refuses_a_profile_name_that_would_be_ignored" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                  = "billet-test"
    vpc_id                = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id             = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone     = "us-east-1a"
    vpc_cidr              = "10.0.0.0/16"
    subnet_in_vpc_ok      = true
    instance_profile_name = "operator-profile"
  }

  expect_failures = [var.instance_profile_name]
}

# THE OTHER HALF of the identity truth table: false with NO name would launch
# an instance with no profile at all.
run "refuses_composed_mode_without_a_profile" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                    = "billet-test"
    vpc_id                  = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id               = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone       = "us-east-1a"
    vpc_cidr                = "10.0.0.0/16"
    subnet_in_vpc_ok        = true
    create_instance_profile = false
  }

  expect_failures = [var.instance_profile_name]
}

# THE SUBNET-IN-VPC PRECONDITION FAILS THE PLAN when the caller's resolution
# says the subnet is foreign — a launch cannot mix a subnet and security
# groups from different VPCs.
run "refuses_a_subnet_outside_the_vpc" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = false
  }

  expect_failures = [aws_instance.control_plane]
}

# NO BACKUP BUCKET BY DEFAULT. The archive directory is the contract and an
# operator's own tooling carrying it is a supported answer, so this child creates
# no bucket and grants nothing unless asked.
run "no_backup_bucket_unless_asked" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
  }

  assert {
    condition     = length(aws_s3_bucket.backups) == 0 && length(aws_iam_role_policy.backups) == 0
    error_message = "a control plane that asked for no backup bucket should get neither a bucket nor a grant"
  }
}

# THE CONTROLLER'S GRANT CAN PUT, GET AND LIST ITS OWN PREFIX — AND NEVER
# DELETE. billet issues no delete at all (internal/archivestore has none), so the
# permission would be dead weight on the one host that also holds the GitHub App
# private key and the node-wire CA, and the archives it could destroy are the
# copies whose whole purpose is surviving the loss of that host.
run "backup_bucket_is_versioned_encrypted_and_never_deletable_by_billet" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                 = "billet-test"
    vpc_id               = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id            = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone    = "us-east-1a"
    vpc_cidr             = "10.0.0.0/16"
    subnet_in_vpc_ok     = true
    create_backup_bucket = true
  }

  assert {
    condition     = aws_s3_bucket_versioning.backups[0].versioning_configuration[0].status == "Enabled"
    error_message = "the backup bucket must be versioned: it is what makes a no-delete grant mean something"
  }

  # `rule` is a SET, whose elements have no addressable index, so this asks
  # whether one of them says AES256 rather than indexing.
  assert {
    condition = anytrue([
      for r in aws_s3_bucket_server_side_encryption_configuration.backups[0].rule :
      anytrue([
        for d in r.apply_server_side_encryption_by_default : d.sse_algorithm == "AES256"
      ])
    ])
    error_message = "an archive is two private keys and a ledger; it must be encrypted at rest"
  }

  assert {
    condition     = aws_s3_bucket_public_access_block.backups[0].block_public_policy && aws_s3_bucket_public_access_block.backups[0].restrict_public_buckets
    error_message = "the backup bucket must be closed to the public"
  }

  # RETENTION APPLIES TO NONCURRENT VERSIONS ONLY. A rule that expired current
  # objects would delete backups on a timer.
  assert {
    condition     = length(aws_s3_bucket_lifecycle_configuration.backups[0].rule[0].expiration) == 0
    error_message = "the lifecycle rule must not expire CURRENT objects: that removes backups on a timer"
  }

  assert {
    condition     = aws_s3_bucket_lifecycle_configuration.backups[0].rule[0].noncurrent_version_expiration[0].noncurrent_days == 90
    error_message = "noncurrent archive versions should expire on the configured schedule"
  }

  # THE GRANT ITSELF: exactly get, put and a prefix-scoped list.
  assert {
    condition     = jsondecode(aws_iam_role_policy.backups[0].policy).Statement[0].Action == ["s3:GetObject", "s3:PutObject"]
    error_message = "the controller's object grant must be exactly get and put"
  }

  assert {
    condition     = jsondecode(aws_iam_role_policy.backups[0].policy).Statement[1].Condition.StringLike["s3:prefix"] == ["billet-backups/*"]
    error_message = "the listing must be scoped to this deployment's prefix, not the whole bucket"
  }

  assert {
    condition = length([
      for s in jsondecode(aws_iam_role_policy.backups[0].policy).Statement :
      s if length([for a in s.Action : a if strcontains(lower(a), "delete")]) > 0
    ]) == 0
    error_message = "a backup credential that can destroy its own history is not an off-site copy"
  }

  # AND NO COMPUTE PERMISSIONS: a control plane launches nothing.
  assert {
    condition = length([
      for s in jsondecode(aws_iam_role_policy.backups[0].policy).Statement :
      s if length([for a in s.Action : a if startswith(a, "ec2:")]) > 0
    ]) == 0
    error_message = "the control plane's backup policy must grant no compute permissions"
  }
}

# AN ADOPTED BUCKET IS GRANTED, NOT CONFIGURED. This child does not set
# versioning, encryption or a lifecycle rule on a bucket it does not own.
run "an_adopted_backup_bucket_is_granted_but_not_configured" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
    backup_bucket     = "someone-elses-backups"
  }

  assert {
    condition     = length(aws_s3_bucket.backups) == 0 && length(aws_s3_bucket_versioning.backups) == 0
    error_message = "this child must not configure a bucket it does not own"
  }

  assert {
    condition     = jsondecode(aws_iam_role_policy.backups[0].policy).Statement[1].Resource == ["arn:aws:s3:::someone-elses-backups"]
    error_message = "the grant must name the adopted bucket"
  }
}

# EXACTLY ONE SOURCE. A bucket name alongside create_backup_bucket would be
# silently ignored, and the controller granted access to a bucket nobody uploads
# to.
run "refuses_both_a_created_and_an_adopted_bucket" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                 = "billet-test"
    vpc_id               = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id            = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone    = "us-east-1a"
    vpc_cidr             = "10.0.0.0/16"
    subnet_in_vpc_ok     = true
    create_backup_bucket = true
    backup_bucket        = "someone-elses-backups"
  }

  expect_failures = [var.backup_bucket]
}

# A WILDCARD PREFIX WIDENS THE IAM GRANT to every sibling prefix, and every
# sibling prefix is another deployment's App key.
run "refuses_a_wildcard_backup_prefix" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                 = "billet-test"
    vpc_id               = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id            = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone    = "us-east-1a"
    vpc_cidr             = "10.0.0.0/16"
    subnet_in_vpc_ok     = true
    create_backup_bucket = true
    backup_prefix        = "billet/*"
  }

  expect_failures = [var.backup_prefix]
}

# EC2 AUTO-RECOVERY, WHICH ADR-001 RESTS ON AND NOTHING ASSERTED.
#
# The whole reason this module is not an ASG is that an ASG launches a FRESH
# instance, and a fresh instance does not reattach the ledger volume — so the
# SQLite ledger, the deployment identity and the node-wire CA would be gone while
# every surface reported healthy. What replaces it is a status-check alarm whose
# action is EC2's built-in recover automation, which brings the SAME instance up
# on new hardware with its volumes.
#
# The instance id is overridden because it is plan-unknown under the mocked
# provider, and an assertion comparing two unknowns proves nothing about which
# instance the alarm watches.
run "auto_recovery_recovers_this_instance" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
  }

  override_resource {
    target          = aws_instance.control_plane
    override_during = plan
    values          = { id = "i-0cp0000000000cafe" }
  }

  # THE ACTION IS THE WHOLE POINT, and it is asserted as EQUALITY rather than
  # with contains(): an appended SNS topic or a second automation would satisfy a
  # containment check while changing what a status-check failure does.
  assert {
    condition     = aws_cloudwatch_metric_alarm.control_plane_recover.alarm_actions == toset(["arn:aws:automate:us-east-1:ec2:recover"])
    error_message = "the control plane's alarm must fire EXACTLY EC2's built-in recover automation; ADR-001 chose it over an ASG because recovery has to keep the ledger volume"
  }

  # ...ON THIS INSTANCE. An alarm watching the wrong instance never fires for the
  # machine it was created beside, and nothing else in a plan would say so.
  assert {
    condition     = aws_cloudwatch_metric_alarm.control_plane_recover.dimensions["InstanceId"] == "i-0cp0000000000cafe"
    error_message = "the recovery alarm must be dimensioned on the control-plane instance it was created beside"
  }

  # THE METRIC IS PINNED WHOLE. StatusCheckFailed_System is the only one
  # ec2:recover can act on — the INSTANCE status check is the guest's own health,
  # which recovery cannot fix and which would power-cycle a machine over an
  # application fault. Maximum over two 60s periods is the shape AWS documents for
  # this action; an Average statistic or a lower threshold silently changes when a
  # controller is recovered.
  assert {
    condition = (
      aws_cloudwatch_metric_alarm.control_plane_recover.namespace == "AWS/EC2" &&
      aws_cloudwatch_metric_alarm.control_plane_recover.metric_name == "StatusCheckFailed_System" &&
      aws_cloudwatch_metric_alarm.control_plane_recover.statistic == "Maximum" &&
      aws_cloudwatch_metric_alarm.control_plane_recover.comparison_operator == "GreaterThanOrEqualToThreshold" &&
      aws_cloudwatch_metric_alarm.control_plane_recover.threshold == 1 &&
      aws_cloudwatch_metric_alarm.control_plane_recover.period == 60 &&
      aws_cloudwatch_metric_alarm.control_plane_recover.evaluation_periods == 2
    )
    error_message = "the recovery alarm must watch the SYSTEM status check at Maximum >= 1 over two 60s periods; the instance status check is the guest's own health, which recovery cannot fix"
  }

}

# THE ARN FOLLOWS THE PARTITION AND THE REGION, and this is the only assertion
# here that can fail against a hard-coded commercial ARN.
#
# `arn:aws:automate:...` is not valid in GovCloud or China, and an alarm carrying
# one is created happily and then does nothing when the controller's hardware
# fails — the failure this whole module exists to survive, in the partitions
# where an operator is least able to go and look.
run "auto_recovery_follows_the_partition" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-gov-west-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
  }

  override_data {
    target = data.aws_partition.this
    values = {
      partition  = "aws-us-gov"
      dns_suffix = "amazonaws.com"
    }
  }
  override_data {
    target = data.aws_region.this
    values = { region = "us-gov-west-1" }
  }

  assert {
    condition     = aws_cloudwatch_metric_alarm.control_plane_recover.alarm_actions == toset(["arn:aws-us-gov:automate:us-gov-west-1:ec2:recover"])
    error_message = "the recover action must follow BOTH the partition and the region; a commercial ARN in GovCloud creates an alarm that silently does nothing"
  }
}

# THE CONTROLLER'S ADDRESS, DECLARED. Four places outside this module repeat it
# -- server.listen, every node's server_addr, the inventory's ansible_host and
# whatever routes a node here -- and until this input existed none of them
# decided it: the address was whatever AWS handed the ENI, and an instance
# replacement changed it silently.
run "a_declared_address_is_the_instances_and_the_wires" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
    private_ip        = "10.0.0.10"
    subnet_cidr       = "10.0.0.0/24"
  }

  assert {
    condition     = aws_instance.control_plane.private_ip == "10.0.0.10"
    error_message = "the declared address must be the one the instance launches with"
  }

  # AND IT IS PLAN-KNOWN, which is the property a consumer needs: the wire
  # address can be written into billet.yaml and an inventory before the apply.
  assert {
    condition     = output.node_wire_address == "10.0.0.10:7717"
    error_message = "the node-wire address must be the declared address and the listen port, known at plan"
  }
}

# AN ADDRESS OUTSIDE THE SUBNET FAILS THE PLAN rather than RunInstances after
# the network is already built. Delete the containment precondition and this
# run passes its plan, which is the mutation it exists to catch.
run "refuses_an_address_outside_the_subnet" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
    private_ip        = "10.0.1.10"
    subnet_cidr       = "10.0.0.0/24"
  }

  expect_failures = [aws_instance.control_plane]
}

# AWS RESERVES THE FIRST FOUR AND THE LAST ADDRESS OF EVERY SUBNET. .1 is the
# VPC router: inside the CIDR, so the containment check alone admits it, and
# RunInstances refuses it.
run "refuses_a_reserved_address" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
    private_ip        = "10.0.0.1"
    subnet_cidr       = "10.0.0.0/24"
  }

  expect_failures = [aws_instance.control_plane]
}

# ...AND THE LAST ADDRESS, which cidrhost(-1) names; a check that listed only
# the first four would let the broadcast address through.
run "refuses_the_broadcast_address" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
    private_ip        = "10.0.0.255"
    subnet_cidr       = "10.0.0.0/24"
  }

  expect_failures = [aws_instance.control_plane]
}

# A MALFORMED ADDRESS IS REFUSED BY ITS VARIABLE, before any precondition
# would try to parse it.
run "refuses_a_malformed_address" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
    private_ip        = "10.0.0.10/32"
  }

  expect_failures = [var.private_ip]
}

# WITHOUT A SUBNET CIDR THE CHECK IS SKIPPED, not failed: a direct consumer who
# does not supply the fact keeps the declaration, the way subnet_in_vpc_ok is a
# claim the caller makes rather than one this child can verify.
run "skips_the_containment_check_without_a_subnet_cidr" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name              = "billet-test"
    vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id         = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone = "us-east-1a"
    vpc_cidr          = "10.0.0.0/16"
    subnet_in_vpc_ok  = true
    private_ip        = "192.0.2.10"
  }

  assert {
    condition     = aws_instance.control_plane.private_ip == "192.0.2.10"
    error_message = "with no subnet_cidr the declared address must be taken on the caller's word"
  }
}

# THE BACKUP GRANT FOLLOWS THE ROLE THE CONTROLLER RUNS WITH. The first version
# attached it only to this child's own role, so the composed shape -- a
# supplied profile, which is what the opinionated root passes -- got a bucket,
# a lifecycle rule and no principal that may write to it: `billet local backup`
# failed at the upload and `billet check` reported the archive stale. Point the
# grant back at the own role and this run fails on the first assertion.
run "composed_backups_land_on_the_supplied_role" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                       = "billet-test"
    vpc_id                     = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id                  = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone          = "us-east-1a"
    vpc_cidr                   = "10.0.0.0/16"
    subnet_in_vpc_ok           = true
    create_instance_profile    = false
    instance_profile_name      = "billet-test-node"
    instance_profile_role_name = "billet-test-node"
    create_backup_bucket       = true
  }

  assert {
    condition     = length(aws_iam_role_policy.backups) == 1 && aws_iam_role_policy.backups[0].role == "billet-test-node"
    error_message = "with a supplied profile the backup grant must attach to the role behind it, not to an own role that does not exist"
  }
  assert {
    condition     = length(aws_iam_role.this) == 0
    error_message = "a supplied profile must still suppress the own identity; the grant moving is not a reason to mint one"
  }
  assert {
    condition     = output.backup_role_name == "billet-test-node"
    error_message = "the role the grant landed on must be reported, so a composing root can prove it through an output"
  }

  # THE DOCUMENT IS UNCHANGED BY WHERE IT LANDS: still billet's own rendering,
  # still no delete.
  assert {
    condition = length([
      for s in jsondecode(aws_iam_role_policy.backups[0].policy).Statement :
      s if length([for a in s.Action : a if strcontains(lower(a), "delete")]) > 0
    ]) == 0
    error_message = "the grant on a supplied role must carry no delete either"
  }
}

# ...AND THE STANDALONE SHAPE IS UNCHANGED: the own role, as before.
run "standalone_backups_land_on_the_own_role" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                 = "billet-test"
    vpc_id               = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id            = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone    = "us-east-1a"
    vpc_cidr             = "10.0.0.0/16"
    subnet_in_vpc_ok     = true
    create_backup_bucket = true
  }

  assert {
    condition     = aws_iam_role_policy.backups[0].role == "billet-test-control-plane" && length(aws_iam_role.this) == 1
    error_message = "a standalone control plane's backup grant must attach to its own role"
  }
  assert {
    condition     = output.backup_role_name == "billet-test-control-plane"
    error_message = "the own role must be the one reported"
  }
}

# BACKUPS BESIDE A SUPPLIED PROFILE WITH NO ROLE NAME IS THE ORIGINAL DEFECT,
# now refused by the variable rather than applied as a bucket nobody may write
# to.
run "refuses_composed_backups_without_the_role_name" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                    = "billet-test"
    vpc_id                  = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id               = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone       = "us-east-1a"
    vpc_cidr                = "10.0.0.0/16"
    subnet_in_vpc_ok        = true
    create_instance_profile = false
    instance_profile_name   = "billet-test-node"
    create_backup_bucket    = true
  }

  expect_failures = [var.instance_profile_role_name]
}

# A ROLE NAME BESIDE THE OWN PROFILE WOULD BE SILENTLY IGNORED, the same rule
# as a profile name beside create_instance_profile = true.
run "refuses_a_role_name_beside_an_own_profile" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                       = "billet-test"
    vpc_id                     = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id                  = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone          = "us-east-1a"
    vpc_cidr                   = "10.0.0.0/16"
    subnet_in_vpc_ok           = true
    instance_profile_role_name = "operator-role"
  }

  expect_failures = [var.instance_profile_role_name]
}

# NO BACKUPS, NO GRANT, whichever identity the controller carries: the composed
# shape without backups must not demand a role name it has nothing to do with.
run "composed_without_backups_needs_no_role_name" {
  command = plan

  module {
    source = "./modules/control-plane-ec2-sqlite"
  }

  variables {
    name                    = "billet-test"
    vpc_id                  = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id               = "subnet-0e0e0e0e0e0e0e0e0"
    availability_zone       = "us-east-1a"
    vpc_cidr                = "10.0.0.0/16"
    subnet_in_vpc_ok        = true
    create_instance_profile = false
    instance_profile_name   = "billet-test-node"
  }

  assert {
    condition     = length(aws_iam_role_policy.backups) == 0 && output.backup_role_name == ""
    error_message = "no backup bucket means no grant and no role to report"
  }
}
