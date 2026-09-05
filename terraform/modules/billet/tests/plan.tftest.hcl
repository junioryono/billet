# The ROOT's plan tests: the composition — network create-vs-adopt, the one
# cross-child rule, and the plan-known outputs. Child INTERNALS are asserted in
# fleet.tftest.hcl and control_plane.tftest.hcl, which make each child the
# configuration under test — a root-level test reaches a child only through
# outputs. Mocked provider throughout: no credentials, no AWS calls, nothing
# created. Run from this directory:
#   terraform init && terraform test
# Live behavior is validated separately (see the module README).

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

  # The adopted-network runs read these. The cidr must parse as a CIDR and the
  # subnet's vpc_id must equal the vpc_id those runs pass, or the module's own
  # subnet-belongs-to-vpc precondition (correctly) fails the plan.
  mock_data "aws_vpc" {
    defaults = {
      cidr_block = "10.0.0.0/16"
    }
  }

  mock_data "aws_subnet" {
    defaults = {
      vpc_id            = "vpc-0f0f0f0f0f0f0f0f0"
      cidr_block        = "10.0.0.0/24"
      availability_zone = "us-east-1a"
    }
  }

  # DELIBERATELY UNSORTED, so the module's sort() is what makes the pick
  # stable rather than the order this fixture happens to use. us-east-1e is
  # absent on purpose: that is the real shape of the answer — measured, every
  # us-east-1 zone reports available while 1e offers no t3.
  # DELIBERATELY UNSORTED, so the module's sort() is what makes the pick stable
  # rather than the order this fixture happens to use. us-east-1e is absent on
  # purpose: that is the real shape of the answer — measured, every us-east-1
  # zone reports available while 1e offers no t3. The Local-Zone-shaped name is
  # here because it SORTS FIRST ("us-east-1-atl-1a" < "us-east-1a"), so only
  # intersecting with the usable zones below keeps it from being chosen.
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

run "creates_a_network_and_composes_by_default" {
  command = plan

  override_resource {
    target          = module.control_plane.aws_security_group.control_plane
    override_during = plan
    values          = { id = "sg-0cp00000000000001" }
  }
  override_resource {
    target          = module.fleet.aws_security_group.runner
    override_during = plan
    values          = { id = "sg-0runner0000000001" }
  }

  variables {
    name = "billet-test"
    # A NON-DEFAULT port, so the assertion proves the variable is WIRED to the
    # rule — asserting the default would pass with both ports hard-coded.
    cache_listen_port = 10443
  }

  assert {
    condition     = length(aws_vpc.this) == 1
    error_message = "an unset vpc_id should create a VPC"
  }

  assert {
    condition     = length(aws_subnet.this) == 1
    error_message = "an unset subnet_id should create a subnet"
  }

  assert {
    condition     = length(aws_internet_gateway.this) == 1 && length(aws_route.internet) == 1
    error_message = "a created VPC needs its gateway and default route"
  }

  # THE ZONE IS CHOSEN, NOT LEFT TO AWS, and chosen from the zones that offer
  # the control-plane shape: sorted-first of the (unsorted) offerings answer.
  # Left unset, AWS picked a zone selling no t3 and the launch failed.
  assert {
    condition     = aws_subnet.this[0].availability_zone == "us-east-1a"
    error_message = "the created subnet must take the first zone that offers control_plane_instance_type"
  }

  # The offerings lookup is a QUERY the mock answers whatever it is asked, so
  # its text is pinned: a wrong location_type or a filter naming the wrong
  # shape would otherwise be invisible until a live plan.
  assert {
    condition     = data.aws_ec2_instance_type_offerings.control_plane[0].location_type == "availability-zone"
    error_message = "the offerings lookup must ask per availability zone"
  }
  assert {
    condition = anytrue([
      for f in data.aws_ec2_instance_type_offerings.control_plane[0].filter :
      f.name == "instance-type" && tolist(f.values) == tolist(["t3.small"])
    ])
    error_message = "the offerings lookup must filter on exactly the configured control-plane shape"
  }
  assert {
    condition = anytrue([
      for f in data.aws_availability_zones.usable[0].filter :
      f.name == "zone-type" && tolist(f.values) == tolist(["availability-zone"])
    ])
    error_message = "the usable-zone lookup must exclude local and wavelength zones"
  }

  # The composition's plan-known outputs: the derived bucket name proves the
  # fleet child received the account and name, and the disabled-spot outputs
  # stay empty rather than unknown.
  assert {
    condition     = output.cache_bucket == "billet-test-cache-123456789012"
    error_message = "the cache bucket name should derive from name and account"
  }
  assert {
    condition     = output.interruption_queue_url == "" && output.spot_node_name == "" && length(output.interruption_queue_urls) == 0
    error_message = "spot outputs must be empty when spot is off"
  }
  assert {
    condition     = output.cache_prefix == "billet-cache"
    error_message = "the cache prefix default should surface"
  }

  # The cross-child cache rule exists by default (the cache is on) and joins
  # the two children's groups.
  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.cache) == 1
    error_message = "the cache endpoint rule should exist when the cache is enabled"
  }
  # The rule's CONTENT is the split-specific wiring this root owns: the two SG
  # ends are overridden to known ids (they are plan-unknown under the mock), so
  # a rule pointing at the wrong group — or open to a CIDR instead — fails.
  assert {
    condition = (
      aws_vpc_security_group_ingress_rule.cache[0].security_group_id == "sg-0cp00000000000001" &&
      aws_vpc_security_group_ingress_rule.cache[0].referenced_security_group_id == "sg-0runner0000000001" &&
      aws_vpc_security_group_ingress_rule.cache[0].from_port == 10443 &&
      aws_vpc_security_group_ingress_rule.cache[0].to_port == 10443 &&
      aws_vpc_security_group_ingress_rule.cache[0].ip_protocol == "tcp"
    )
    error_message = "the cache rule must join the control plane's SG to the runner SG on exactly cache_listen_port/tcp"
  }

  # THE CO-LOCATED OPINION IS OBSERVABLE: the controller's profile is fleet's
  # node profile, not the child's own minimal identity.
  assert {
    condition     = output.control_plane_instance_profile == "billet-test-node"
    error_message = "the root must attach fleet-ec2's node profile to the controller"
  }

  # The created-side cidr resolution.
  assert {
    condition     = output.vpc_cidr == "10.60.0.0/16"
    error_message = "a created VPC's cidr must be var.vpc_cidr's default"
  }
}

# AN OPERATOR'S OWN CHOICE WINS over the derived one — but only a zone that
# actually sells the shape.
run "an_explicit_zone_is_used" {
  command = plan

  variables {
    name                     = "billet-test"
    subnet_availability_zone = "us-east-1b"
  }

  assert {
    condition     = aws_subnet.this[0].availability_zone == "us-east-1b"
    error_message = "an explicitly named zone must be the one the subnet uses"
  }

  # ...and that it CROSSES the module call: the ledger volume is created in the
  # child's zone, so a root that resolved correctly but passed something else
  # would strand the ledger in another zone with this otherwise green.
  assert {
    condition     = output.availability_zone == "us-east-1b"
    error_message = "the resolved zone must reach the control-plane child, which creates the ledger volume in it"
  }
}

# ...and a zone that does not sell it fails the PLAN, rather than surviving to
# a RunInstances refusal after the network is already built.
run "refuses_a_zone_that_cannot_run_the_shape" {
  command = plan

  variables {
    name                     = "billet-test"
    subnet_availability_zone = "us-east-1e"
  }

  expect_failures = [aws_subnet.this]
}

run "adopts_an_existing_network" {
  command = plan

  # Deliberately synthetic ids (the mock never resolves them): a real-looking id
  # here reads as a live dependency and invites someone to "fix" the tests by
  # pointing them at a real account.
  variables {
    name      = "billet-test"
    vpc_id    = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id = "subnet-0e0e0e0e0e0e0e0e0"
  }

  assert {
    condition     = length(aws_vpc.this) == 0 && length(aws_subnet.this) == 0
    error_message = "a supplied vpc_id/subnet_id should adopt, not create"
  }
  assert {
    condition     = length(aws_internet_gateway.this) == 0 && length(aws_route_table.this) == 0
    error_message = "an adopted network must gain no gateway or route table"
  }

  # An adopted subnet's zone is the operator's; the module must not even ask
  # which zones sell the shape, let alone act on the answer.
  assert {
    condition     = length(data.aws_ec2_instance_type_offerings.control_plane) == 0 && length(data.aws_availability_zones.usable) == 0
    error_message = "adoption must make no zone-selection query at all"
  }
  assert {
    condition     = output.vpc_id == "vpc-0f0f0f0f0f0f0f0f0" && output.subnet_id == "subnet-0e0e0e0e0e0e0e0e0"
    error_message = "the adopted ids must surface unchanged"
  }
  assert {
    condition     = output.availability_zone == "us-east-1a"
    error_message = "the adopted subnet's zone must surface for node.ebs_s3"
  }

  # THE ADOPTED-SIDE CIDR RESOLUTION, only observable through this output: a
  # regression to var.vpc_cidr here opens the node wire to a range no node is
  # in, on every adopted-network deployment.
  assert {
    condition     = output.vpc_cidr == "10.0.0.0/16"
    error_message = "the node-wire default must resolve to the ADOPTED vpc's cidr, not the create-side default"
  }
}

run "no_cache_rule_without_the_cache" {
  command = plan

  variables {
    name         = "billet-test"
    enable_cache = false
  }

  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.cache) == 0
    error_message = "no cache endpoint rule without the cache"
  }
  assert {
    condition     = output.cache_bucket == "" && output.cache_kms_key_arn == ""
    error_message = "cache outputs must be empty when the cache is off"
  }
}

# THE TWO OUTPUTS THAT DESCRIBE A PLAN RATHER THAN THE INFRASTRUCTURE.
#
# `terraform plan` says create/update/replace/destroy and cannot say which of
# those stops a machine running somebody's build — ADR-004 keeps live billet
# nodes outside Terraform on purpose. These render the missing half so an
# operator's own tooling can read it, and scripts/tfclassify joins the same table
# to a plan to gate an apply.
run "the_plan_carries_its_own_classification_and_cost_inputs" {
  command = plan

  variables {
    name = "billet-test"
  }

  # THE CLASSIFICATION CROSSES INTO THE OUTPUT, asserted on the two entries whose
  # classes carry the most: an operator who reads "the controller is in_place"
  # applies a plan that stops the process holding the ledger.
  assert {
    condition     = output.operation_classes["aws_instance.control_plane"].class == "draining"
    error_message = "replacing the controller must be reported as draining; it is the process polling GitHub and holding the capacity ledger"
  }
  assert {
    condition     = output.operation_classes["aws_ebs_volume.ledger"].class == "destructive"
    error_message = "the ledger volume must be reported as destructive; it holds the ledger, the deployment identity and the node-wire CA"
  }

  # AND THE TABLE'S OWN PROSE IS NOT AN ENTRY. `_comment` explains the file; a
  # reader iterating the output must not meet a resource called `_comment`.
  assert {
    condition     = !contains(keys(output.operation_classes), "_comment")
    error_message = "the classification's own prose is being rendered as though it were a resource"
  }

  # THE COST INPUTS ARE THE MODULE'S OWN DECISIONS, not a price table. billet
  # ships no prices on purpose — a stale one understates exposure — so what this
  # carries is the always-on shape and an explicit statement of what it does NOT
  # bound.
  assert {
    condition = (
      output.cost_inputs.always_on.control_plane_instances == 1 &&
      output.cost_inputs.always_on.control_plane_instance_type == "t3.small" &&
      output.cost_inputs.always_on.ledger_volume_gib == 20
    )
    error_message = "the always-on cost inputs must describe the one controller this module creates and its retained ledger volume"
  }
  assert {
    condition     = length(output.cost_inputs.per_job_compute_bounded_by) == 3
    error_message = "the cost inputs must say what bounds per-job compute; this module does not, and leaving that to inference is how an operator reads a controller's cost as the deployment's"
  }
  assert {
    condition     = output.cost_inputs.created_when_enabled.spot_interruption == false
    error_message = "the optional resources must follow the variables that create them"
  }

  # A CREATED VPC ROUTES THROUGH AN INTERNET GATEWAY, WHICH IS FREE. The NAT
  # gateway is the charge operators most often meet unexpectedly, and this module
  # never creates one — said out loud rather than left to be discovered.
  assert {
    condition     = strcontains(output.cost_inputs.notes, "internet gateway")
    error_message = "a created network's egress path should be named, since it is the difference between a free gateway and a NAT gateway's standing charge"
  }
}

run "spot_surfaces_the_node_name" {
  command = plan

  variables {
    name        = "billet-test"
    enable_spot = true
    # THE ROUTER'S ALARM ACTION IS SETTABLE FROM THE ROOT. A plan is refused for an
    # undeclared variable, so this proves the root declares it and accepts an ARN;
    # what the child does with it is asserted in fleet.tftest.hcl, because a test
    # run against the root cannot address a child module's resources.
    spot_router_alarm_actions = ["arn:aws:sns:us-east-1:123456789012:billet-ops"]
    # A SECOND SPOT NODE, named from the root. The map output is the witness that
    # the list crossed the module call and the child's queues came back through
    # it; the queue, the grants and the router's environment are asserted in
    # fleet.tftest.hcl for the same reason as the alarm action.
    spot_node_names = ["build-1"]
  }

  assert {
    condition     = output.spot_node_name == "billet-test-spot-interruptions"
    error_message = "the spot node name must be the queue basename"
  }
  assert {
    condition     = toset(keys(output.interruption_queue_urls)) == toset(["billet-test-spot-interruptions", "build-1"])
    error_message = "interruption_queue_urls must carry the primary and every spot_node_names entry, keyed by node name, through the root"
  }
}

# THE ROOT ENFORCES THE CHILD'S RULES ITSELF, so the refusal names the input the
# operator typed rather than surfacing from a module they did not write. Each
# run expects the failure on the ROOT's variable: the child's refusal is
# module.fleet.var.spot_node_names, a different address, so a root that lost a
# rule fails these rather than being covered by the child. One run per rule,
# because a single refusal run would be satisfied by whichever rule survived.
run "spot_node_names_without_spot_is_refused_at_the_root" {
  command = plan

  variables {
    name            = "billet-test"
    enable_spot     = false
    spot_node_names = ["build-1"]
  }

  expect_failures = [var.spot_node_names]
}

run "a_dotted_spot_node_name_is_refused_at_the_root" {
  command = plan

  variables {
    name            = "billet-test"
    enable_spot     = true
    spot_node_names = ["build.1"]
  }

  expect_failures = [var.spot_node_names]
}

run "a_repeated_spot_node_name_is_refused_at_the_root" {
  command = plan

  variables {
    name            = "billet-test"
    enable_spot     = true
    spot_node_names = ["build-1", "build-1"]
  }

  expect_failures = [var.spot_node_names]
}

run "the_primary_queue_as_a_spot_node_name_is_refused_at_the_root" {
  command = plan

  variables {
    name            = "billet-test"
    enable_spot     = true
    spot_node_names = ["billet-test-spot-interruptions"]
  }

  expect_failures = [var.spot_node_names]
}

run "an_overlong_spot_node_name_is_refused_at_the_root" {
  command = plan

  variables {
    name            = "billet-test"
    enable_spot     = true
    spot_node_names = ["abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghij"]
  }

  expect_failures = [var.spot_node_names]
}

run "a_seventeenth_spot_node_name_is_refused_at_the_root" {
  command = plan

  variables {
    name            = "billet-test"
    enable_spot     = true
    spot_node_names = [for i in range(17) : "build-${i}"]
  }

  expect_failures = [var.spot_node_names]
}

# ...AND THE ROOT ADMITS EVERYTHING THE CHILD ADMITS: a root rule tightened past
# the child's would refuse at the entry point operators actually use, with every
# child test still green. Sixteen names covering every edge of the rule — the
# 64-character ceiling, an underscore, an uppercase letter, a leading digit and
# the one-character floor — all reach the output.
run "the_root_admits_the_edges_of_the_name_rule" {
  command = plan

  variables {
    name        = "billet-test"
    enable_spot = true
    spot_node_names = concat(
      ["abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghi", "build_1", "A", "0"],
      [for i in range(12) : "build-${i}"],
    )
  }

  assert {
    condition = length(output.interruption_queue_urls) == 17 && alltrue([
      for n in ["abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghi", "build_1", "A", "0"] :
      contains(keys(output.interruption_queue_urls), n)
    ])
    error_message = "sixteen further names, the longest, an underscored, an uppercase, a digit-first and a one-character one among them, must all cross the root into interruption_queue_urls"
  }
}

# ...and a value that is not an ARN is refused at PLAN, not silently ignored by
# CloudWatch, which would leave an alarm reading as covered while it notifies
# nobody.
run "a_router_alarm_action_that_is_not_an_arn_is_refused" {
  command = plan

  variables {
    name                      = "billet-test"
    enable_spot               = true
    spot_router_alarm_actions = ["billet-ops"]
  }

  expect_failures = [var.spot_router_alarm_actions]
}

# THE CONTROLLER'S ADDRESS CROSSES THE MODULE CALL, on both sides of adopt-or-
# create. A declared address is the whole point of control_plane_private_ip,
# and the CIDR it is checked against has to be the subnet that is real.
run "a_declared_address_reaches_the_created_subnet" {
  command = plan

  variables {
    name                     = "billet-test"
    control_plane_private_ip = "10.60.0.10"
  }

  assert {
    condition     = output.control_plane_private_ip == "10.60.0.10"
    error_message = "the declared address must reach the child and surface plan-known"
  }
  assert {
    condition     = output.node_wire_address == "10.60.0.10:7717"
    error_message = "the node-wire address must be the declared address, known at plan"
  }
}

# AN ADOPTED SUBNET'S CIDR IS THE ONE THE ADDRESS IS CHECKED AGAINST: an address
# in the created-side default range fails against the adopted 10.0.0.0/24 the
# mock answers with, which proves the adopted CIDR reached the child rather than
# var.subnet_cidr's default.
run "an_adopted_subnet_checks_the_declared_address" {
  command = plan

  variables {
    name                     = "billet-test"
    vpc_id                   = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id                = "subnet-0e0e0e0e0e0e0e0e0"
    control_plane_private_ip = "10.0.0.10"
  }

  assert {
    condition     = output.control_plane_private_ip == "10.0.0.10"
    error_message = "an address inside the adopted subnet must be accepted"
  }

  # THE CIDR THE CHILD CHECKED AGAINST, read back through it: a regression to
  # var.subnet_cidr's default here would check every adopted deployment's
  # address against a range it is not in. A refusal cannot be expected from a
  # child resource in a root test, so the fact is proved through the output.
  assert {
    condition     = output.subnet_cidr == "10.0.0.0/24"
    error_message = "the adopted subnet's cidr must be the one the child checks the declared address against"
  }
}

# ...and the created side resolves to the subnet this module makes.
run "a_created_subnet_is_the_cidr_the_address_is_checked_against" {
  command = plan

  variables {
    name = "billet-test"
  }

  assert {
    condition     = output.subnet_cidr == "10.60.0.0/20"
    error_message = "a created subnet's cidr must be var.subnet_cidr's default"
  }
}

# THE CO-LOCATED CONTROLLER'S BACKUP GRANT LANDS ON THE FLEET ROLE. This root
# hands the controller fleet-ec2's profile, so a grant on the child's own
# identity would protect nothing here; the wiring is observable only through
# the child's output, which is why that output exists. The bucket name is
# composed rather than read back, so it is plan-known too.
run "root_backups_grant_the_fleet_role" {
  command = plan

  variables {
    name                 = "billet-test"
    create_backup_bucket = true
  }

  assert {
    condition     = output.backup_role_name == "billet-test-node"
    error_message = "the root must attach the backup grant to fleet-ec2's node role, the identity the co-located controller runs with"
  }
  assert {
    condition     = output.backup_bucket == "billet-test-backups" && output.backup_prefix == "billet-backups"
    error_message = "the bucket and prefix billet.yaml must name should surface from the child"
  }
  assert {
    condition     = output.cost_inputs.created_when_enabled.backup_bucket == true
    error_message = "the cost inputs must follow the variable that creates the bucket"
  }
}

# AND NOTHING WITHOUT ASKING: the default root creates no bucket and grants
# nothing, so an existing deployment upgrading to this version plans no change.
run "root_creates_no_backups_by_default" {
  command = plan

  variables {
    name = "billet-test"
  }

  assert {
    condition     = output.backup_bucket == "" && output.backup_role_name == ""
    error_message = "a root that asked for no backup bucket must report neither a bucket nor a grant"
  }
  assert {
    condition     = output.cost_inputs.created_when_enabled.backup_bucket == false
    error_message = "the cost inputs must say the bucket is off by default"
  }
}

# THE ROOT REFUSES A BUILDER BESIDE AN OVERRIDE, at the layer an operator typed
# both into.
#
# The child refuses it too, and must, being an exported entry point of its own —
# but a refusal that surfaces from a module the operator did not write names
# inputs they cannot see. The reason is the child's: the builder grant this
# module attaches is account-wide, because there is no deployment id at apply
# time, and IAM unions allows, so beside a value-scoped override it would hand
# the node role account-wide reach over every deployment's builders.
run "root_refuses_a_builder_beside_an_override" {
  command = plan

  variables {
    name            = "billet-test"
    builder         = true
    iam_policy_json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
  }

  expect_failures = [var.builder]
}
