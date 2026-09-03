# THE UPGRADE PROPERTY, in its own file because it needs STATE: the runs below
# apply against the mocked provider and then plan against what that produced,
# which the plan-only suites deliberately never do.
#
# What it protects: a subnet that already exists keeps the zone it was created
# in, even when the module would now choose a different one. Measured live, the
# alternative is not cosmetic — availability_zone forces replacement, replacing
# the subnet replaces the zone-bound ledger volume, and that plan dies on
# prevent_destroy. An operator upgrading a deployment they never meant to move
# would face a plan with no legal outcome, so the zone is a create-time
# decision and ignore_changes is what keeps it one.

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

  mock_data "aws_ec2_instance_type_offerings" {
    defaults = {
      locations = ["us-east-1-atl-1a", "us-east-1f", "us-east-1b", "us-east-1a"]
    }
  }

  mock_data "aws_availability_zones" {
    defaults = {
      names = ["us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d", "us-east-1f"]
    }
  }
}

run "a_deployment_exists_in_a_named_zone" {
  command = apply

  variables {
    name                     = "billet-test"
    subnet_availability_zone = "us-east-1b"
  }

  assert {
    condition     = aws_subnet.this[0].availability_zone == "us-east-1b"
    error_message = "the applied subnet should be in the zone it was created with"
  }
}

# The zone the module would NOW choose differs — from a changed input here, and
# in the real upgrade from a module that never chose one at all. Either way the
# applied subnet is left where it is.
run "a_changed_zone_does_not_move_an_applied_subnet" {
  command = plan

  variables {
    name                     = "billet-test"
    subnet_availability_zone = "us-east-1f"
  }

  assert {
    condition     = aws_subnet.this[0].availability_zone == "us-east-1b"
    error_message = "an applied subnet's zone must survive a changed input: moving it replaces the zone-bound ledger volume, which prevent_destroy refuses"
  }

  # The ledger volume follows the subnet, so proving it stays put proves the
  # refusal cannot be reached.
  assert {
    condition     = module.control_plane.availability_zone == "us-east-1b"
    error_message = "the ledger's zone must not move either: the child creates the volume in the zone it receives"
  }
}
