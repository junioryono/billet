# ADOPT-OR-CREATE. With vpc_id/subnet_id unset the module builds a minimal public
# VPC; with them set it places billet in an existing network and creates nothing
# here. The network stays in the ROOT because both children live in it — the
# security groups themselves belong to the children that own their traffic.

resource "aws_vpc" "this" {
  count = local.create_vpc ? 1 : 0

  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = merge(local.tags, { "Name" = "${var.name}-vpc" })
}

resource "aws_internet_gateway" "this" {
  count = local.create_vpc ? 1 : 0

  vpc_id = aws_vpc.this[0].id
  tags   = merge(local.tags, { "Name" = "${var.name}-igw" })
}

# WHICH ZONE THE CREATED SUBNET LANDS IN — not left to AWS, and not taken from
# the zone list either. A zone reporting "available" need not SELL the instance
# type: measured in us-east-1, all six zones report available while us-east-1e
# offers no t3 at all, so a default deployment whose subnet landed there failed
# at RunInstances with `Unsupported: Your requested instance type (t3.small) is
# not supported in your requested Availability Zone`. This API answers the
# question RunInstances will ask; DescribeAvailabilityZones does not.
#
# It is decided here rather than discovered on the first launch because the
# choice is effectively PERMANENT: the ledger volume is zone-bound and carries
# prevent_destroy, so moving the subnet afterwards is a deliberate migration
# rather than an edit.
data "aws_ec2_instance_type_offerings" "control_plane" {
  count = local.create_subnet ? 1 : 0

  location_type = "availability-zone"

  filter {
    name   = "instance-type"
    values = [var.control_plane_instance_type]
  }
}

# ...AND THE ZONES THIS ACCOUNT CAN ACTUALLY USE, standard zones only. The
# offerings answer is account-scoped (measured: an account opted into no Local
# Zone is offered only the four ordinary us-west-2 zones), but an account that
# HAS opted into one would be offered it — and a Local Zone name sorts BEFORE a
# plain one ("us-west-2-lax-1a" < "us-west-2a"), so an unintersected pick would
# put the control plane and its ledger in a Local Zone on somebody else's
# account and not on ours. zone-type is what excludes local-zone and
# wavelength-zone; the module must not depend on the reader's opt-ins.
data "aws_availability_zones" "usable" {
  count = local.create_subnet ? 1 : 0

  state = "available"

  filter {
    name   = "zone-type"
    values = ["availability-zone"]
  }
}

locals {
  # SORTED, so the pick is stable across plans: an API answer that came back in
  # a different order would otherwise plan a subnet replacement, which forces
  # the zone-bound ledger volume's replacement, which prevent_destroy refuses —
  # an unfixable plan on a deployment nobody meant to move.
  control_plane_azs = local.create_subnet ? sort(tolist(setintersection(
    toset(data.aws_ec2_instance_type_offerings.control_plane[0].locations),
    toset(data.aws_availability_zones.usable[0].names),
  ))) : []

  subnet_availability_zone = (
    var.subnet_availability_zone != ""
    ? var.subnet_availability_zone
    : (length(local.control_plane_azs) > 0 ? local.control_plane_azs[0] : null)
  )
}

resource "aws_subnet" "this" {
  count = local.create_subnet ? 1 : 0

  vpc_id                  = local.vpc_id
  cidr_block              = var.subnet_cidr
  availability_zone       = local.subnet_availability_zone
  map_public_ip_on_launch = true
  tags                    = merge(local.tags, { "Name" = "${var.name}-subnet" })

  lifecycle {
    # THE ZONE IS A CREATE-TIME DECISION, and this is what keeps that true for a
    # subnet that already exists. A deployment applied before this module chose
    # zones has whatever zone AWS picked recorded in state; without this, the
    # derived value would differ from it, and availability_zone forces
    # replacement — measured, that cascades to the zone-bound ledger volume and
    # the plan dies on prevent_destroy, an unfixable upgrade for a deployment
    # nobody meant to move. Moving zones is a migration (snapshot the ledger,
    # replace deliberately), never an edit, so an applied zone is left alone.
    ignore_changes = [availability_zone]

    precondition {
      condition     = length(local.control_plane_azs) > 0
      error_message = "No availability zone in this region offers ${var.control_plane_instance_type}. Set control_plane_instance_type to a shape this region sells, or adopt an existing subnet with subnet_id."
    }

    # A named zone is checked against the same authority, so an unsellable
    # choice fails the PLAN rather than the launch.
    precondition {
      condition     = var.subnet_availability_zone == "" || contains(local.control_plane_azs, var.subnet_availability_zone)
      error_message = "subnet_availability_zone ${var.subnet_availability_zone} does not offer ${var.control_plane_instance_type}, so RunInstances would refuse it. Zones that do: ${join(", ", local.control_plane_azs)}. On a deployment that is already applied this value no longer places anything — clear it to plan again, and move zones by migrating the ledger deliberately."
    }
  }
}

resource "aws_route_table" "this" {
  count = local.create_vpc ? 1 : 0

  vpc_id = aws_vpc.this[0].id
  tags   = merge(local.tags, { "Name" = "${var.name}-rt" })
}

resource "aws_route" "internet" {
  count = local.create_vpc ? 1 : 0

  route_table_id         = aws_route_table.this[0].id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this[0].id
}

resource "aws_route_table_association" "this" {
  count = local.create_vpc && local.create_subnet ? 1 : 0

  subnet_id      = aws_subnet.this[0].id
  route_table_id = aws_route_table.this[0].id
}
