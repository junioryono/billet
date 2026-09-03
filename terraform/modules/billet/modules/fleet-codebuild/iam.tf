# TWO ROLES, AND THE SPLIT IS THE SECURITY CONTENT OF THIS FILE.
#
# THE NODE ROLE is what billet assumes: it starts and stops builds in one project,
# reads them back, and stages and removes the single-use runner registration.
#
# THE BUILD ROLE is what CodeBuild runs the build AS, which means every permission it
# holds is a permission the workflow holds. It reads the one parameter carrying its own
# registration and writes its own logs. It may NOT start a build — a role that could,
# running inside a build, is how a job launches runners billet never escrowed capacity
# for — and it may NOT delete a parameter, because that would let a job destroy
# another build's registration.
#
# BOTH POLICIES ARE BILLET'S OWN GENERATOR'S OUTPUT. The module commits renderings of
# internal/awspolicy (kept equal by internal/tfpolicy's drift test) and substitutes
# this deployment's account, region, project, fleet, parameter path, key and log group
# for the sentinels. What billet's code performs and what its policy grants cannot
# disagree if only one place decides.

locals {
  # SELECT THE RENDERING BY THE ENABLED FEATURES, so a fleetless or keyless
  # deployment's role carries only what it exercises. The committed files use
  # UPPERCASE sentinels, which cannot appear in any real value substituted here —
  # name, region, project and parameter path are all validated lowercase or
  # path-shaped — so every replace is a single unambiguous rewrite.
  # ALL FOUR COMBINATIONS, because the fleet and the key are independent. The first
  # version had two, and a deployment with a fleet and no customer-managed key fell
  # through to the fleetless rendering — which grants no codebuild:BatchGetFleets, so
  # `billet check` could not report the fleet capacity a macOS tier's macos_vm_limit
  # is supposed to be set from. The plan test is what caught it.
  _node_policy_file = (
    local.fleet_arn != "" && local.enable_kms ? "${path.module}/policy/node-policy-fleet-kms.json" :
    local.fleet_arn != "" ? "${path.module}/policy/node-policy-fleet.json" :
    local.enable_kms ? "${path.module}/policy/node-policy-kms.json" :
    "${path.module}/policy/node-policy.json"
  )

  _build_policy_file = (
    local.enable_kms ? "${path.module}/policy/build-role-policy-kms.json" :
    "${path.module}/policy/build-role-policy.json"
  )

  # THE COMPOSITE ARNs ARE REPLACED FIRST, innermost, so each matches its whole
  # sentinel before the components inside it are substituted individually — the same
  # ordering fleet-ec2's KMS replace uses and for the same reason.
  _node_policy_rendered = replace(replace(replace(replace(replace(replace(replace(replace(
    file(local._node_policy_file),
    "arn:TFPARTITION:codebuild:TFREGION:TFACCOUNT:project/TFPROJECT", local.project_arn),
    "arn:TFPARTITION:codebuild:TFREGION:TFACCOUNT:fleet/TFFLEET", local.fleet_arn),
    "arn:TFPARTITION:kms:TFREGION:TFACCOUNT:key/TFKMSKEY", local.kms_key_arn),
    "/TFPARAMPATH", local.parameter_path),
    "TFPARTITION", local.partition),
    "TFDNSSUFFIX", local.suffix),
    "TFREGION", local.region),
  "TFACCOUNT", local.account_id)

  # TFDNSSUFFIX IS IN BOTH CHAINS. It was missing from this one, which left
  # `kms:ViaService: ssm.<region>.TFDNSSUFFIX` in the build role's policy — a
  # condition no request can ever satisfy, so a build with a customer-managed key
  # could not decrypt its own registration and every job would fail to register. The
  # plan test caught it, which is the argument for asserting on the RENDERED document
  # rather than on the substitution list.
  _build_policy_rendered = replace(replace(replace(replace(replace(replace(replace(
    file(local._build_policy_file),
    "arn:TFPARTITION:logs:TFREGION:TFACCOUNT:log-group:TFLOGGROUP", local.log_group_arn),
    "arn:TFPARTITION:kms:TFREGION:TFACCOUNT:key/TFKMSKEY", local.kms_key_arn),
    "/TFPARAMPATH", local.parameter_path),
    "TFPARTITION", local.partition),
    "TFDNSSUFFIX", local.suffix),
    "TFREGION", local.region),
  "TFACCOUNT", local.account_id)

  # THE CONTROLLER'S SWEEP GRANT, the third principal: list and delete under the path,
  # on the ledger's authority, and nothing else. Rendered from the generator like the
  # other two, with the same sentinels and the same composite-first ordering.
  controller_sweep_policy = replace(replace(replace(replace(
    file("${path.module}/policy/controller-sweep-policy.json"),
    "/TFPARAMPATH", local.parameter_path),
    "TFPARTITION", local.partition),
    "TFREGION", local.region),
  "TFACCOUNT", local.account_id)

  node_policy = var.iam_policy_json != "" ? var.iam_policy_json : local._node_policy_rendered

  # THE ROLE THAT ACTUALLY RUNS THE BUILDS. A created project gets the role this module
  # makes; an adopted one keeps its own, which the operator has to name.
  build_role_name = local.create_project ? aws_iam_role.build[0].name : var.existing_build_role_name
  build_role_arn = (local.create_project
    ? aws_iam_role.build[0].arn
  : "arn:${local.partition}:iam::${local.account_id}:role/${var.existing_build_role_name}")
}

# jsonencode rather than aws_iam_policy_document, deliberately: this document is fully
# known at plan, and rendering it in Terraform (not in the provider) keeps it
# assertable under the mocked test provider. A hard-coded amazonaws.com here would
# render a trust policy no partition but the commercial one can assume.
resource "aws_iam_role" "node" {
  name = "${var.name}-cb-node"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "ec2.${local.suffix}" }
    }]
  })

  tags = local.tags
}

resource "aws_iam_role_policy" "node" {
  name   = "${var.name}-cb-node"
  role   = aws_iam_role.node.id
  policy = local.node_policy
}

resource "aws_iam_instance_profile" "node" {
  name = "${var.name}-cb-node"
  role = aws_iam_role.node.name
  tags = local.tags
}

# THE CONTROL PLANE'S SWEEP, on whichever role `billet server` runs under.
#
# NEVER ON THE NODE ROLE AND NEVER ON THE BUILD ROLE. The node already deletes the
# registrations it staged; what this grants is the LISTING, and a node that could list
# its own path gains nothing while a build that could would be able to enumerate every
# concurrent job's registration name. The controller is the one principal that holds
# the ledger, which is the only thing that can say a registration is dead.
resource "aws_iam_role_policy" "controller_sweep" {
  count = var.controller_role_name != "" ? 1 : 0

  name   = "${var.name}-cb-controller-sweep"
  role   = var.controller_role_name
  policy = local.controller_sweep_policy
}

# THE BUILD'S OWN SERVICE ROLE. Assumed by CodeBuild, not by a machine billet runs on.
#
# CREATED ONLY ALONGSIDE A CREATED PROJECT, because only a created project has its
# service role set by this module. Adopting a project and creating a role produced a
# role NOTHING USED: the adopted project kept whatever role it had, so its builds could
# not read their own registration, every job failed to register, and the apply reported
# success — while the old role's permissions, whatever they were, stayed in place. An
# adopted project names its real role in existing_build_role_name and the policy is
# attached to that.
resource "aws_iam_role" "build" {
  count = local.create_project ? 1 : 0

  name = "${var.name}-cb-build"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "codebuild.${local.suffix}" }
    }]
  })

  tags = local.tags
}

# THE POLICY GOES ON WHICHEVER ROLE ACTUALLY RUNS THE BUILDS — the created one, or the
# adopted project's own. There is exactly one of these either way, which is what makes
# the assembled-role assertions in the plan tests possible.
resource "aws_iam_role_policy" "build" {
  name   = "${var.name}-cb-build"
  role   = local.build_role_name
  policy = local._build_policy_rendered
}

# THE NETWORKING PERMISSIONS GO ON THE FLEET'S ROLE, NEVER ON THE BUILD'S, and this
# is the one place in the module where getting it wrong is a security hole rather than
# an inconvenience.
#
# A CODEBUILD SERVICE ROLE'S CREDENTIALS ARE REACHABLE FROM INSIDE THE BUILD. So
# anything attached to the build role is attached to arbitrary workflow code — and a
# VPC-connected project's service role needs `ec2:CreateNetworkInterface` and
# `ec2:DeleteNetworkInterface`, which cannot be resource-scoped. Attaching those to the
# build role hands every job the ability to exhaust the account's ENI quota or delete
# interfaces belonging to something else entirely.
#
# THE FIRST VERSION OF THIS MODULE DID EXACTLY THAT, and the test that was supposed to
# forbid it examined only the GENERATED rendering — so a supplemental policy written in
# HCL was invisible to it. That is the "proving the mechanism is not proving it is
# used" failure: the assertion was about the wrong artifact. It now reads the assembled
# role.
#
# SO A VPC REQUIRES A FLEET. A reserved fleet has its OWN service role, which AWS
# assumes to manage the fleet's instances rather than to run a build — the credentials
# never enter the build environment. And it costs nothing to insist on: billet sends
# `fleetOverride` on every launch when a fleet is configured, and a fleetOverride makes
# CodeBuild IGNORE the project's VPC configuration anyway, so a project-level VPC
# without a fleet is the only case this refuses and it is the only unsafe one.
resource "aws_iam_role" "fleet" {
  count = local.create_fleet_network_role ? 1 : 0

  name = "${var.name}-cb-fleet"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "codebuild.${local.suffix}" }
    }]
  })

  tags = local.tags
}

# THE ENI PERMISSIONS, on the one principal whose credentials a build cannot reach.
#
# The creates and describes cannot be resource-scoped — AWS's own documented fleet
# service-role policy is exactly this shape — which is precisely why they must not sit
# on a role the workflow can use.
resource "aws_iam_role_policy" "fleet" {
  count = local.create_fleet_network_role ? 1 : 0

  name = "${var.name}-cb-fleet"
  role = aws_iam_role.fleet[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid    = "BilletFleetNetworkInterfaces"
      Effect = "Allow"
      Action = [
        "ec2:CreateNetworkInterface",
        "ec2:DescribeNetworkInterfaces",
        "ec2:DeleteNetworkInterface",
        "ec2:DescribeSubnets",
        "ec2:DescribeSecurityGroups",
        "ec2:DescribeVpcs",
        "ec2:DescribeDhcpOptions",
      ]
      Resource = "*"
    }]
  })
}
