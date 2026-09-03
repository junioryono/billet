# THE AWS SIDE OF A BILLET CODEBUILD FLEET: the project billet starts builds in, an
# optional reserved-capacity fleet, the two roles, the log group, the parameter path
# and an optional key.
#
# TERRAFORM OWNS AWS INFRASTRUCTURE; BILLET OWNS JOBS. This module creates nothing
# that has a lifecycle tied to a job — no builds, no parameters — and it writes no
# billet configuration. It OUTPUTS the non-secret facts billet.yaml needs, the same
# seam ADR-004 sets for fleet-ec2.
#
# AND IT DOES NOT CREATE A WEBHOOK. CodeBuild's own GitHub Actions runner integration
# is webhook-driven, and a WORKFLOW_JOB_QUEUED webhook on a billet-owned project means
# two schedulers acquiring one job — which produces duplicate runners and reads as
# GitHub misbehaving. `billet check --provider codebuild` treats one as a FATAL
# finding; this module never adds one.

data "aws_partition" "this" {}
data "aws_region" "this" {}
data "aws_caller_identity" "this" {}

locals {
  region     = data.aws_region.this.region
  account_id = data.aws_caller_identity.this.account_id
  partition  = data.aws_partition.this.partition
  suffix     = data.aws_partition.this.dns_suffix

  tags = merge(var.tags, { "sh.billet.module" = "fleet-codebuild" })

  # ADOPT OR CREATE, for every resource that can be either — the rule the root
  # module already follows. The backend's design asks for it explicitly: an operator who already has
  # a project, a fleet, a VPC or a log group must be able to place billet in it.
  create_project = var.project_name == ""
  project_name   = local.create_project ? var.name : var.project_name
  project_arn    = "arn:${local.partition}:codebuild:${local.region}:${local.account_id}:project/${local.project_name}"

  # A fleet is created when asked for and not adopted.
  create_fleet = var.enable_fleet && var.fleet_arn == ""
  fleet_arn    = var.fleet_arn != "" ? var.fleet_arn : (local.create_fleet ? aws_codebuild_fleet.this[0].arn : "")

  parameter_path = var.parameter_path != "" ? var.parameter_path : "/billet/${var.name}/jit"

  create_log_group = var.log_group_name == ""
  log_group_name   = local.create_log_group ? "/aws/codebuild/${local.project_name}" : var.log_group_name
  log_group_arn    = "arn:${local.partition}:logs:${local.region}:${local.account_id}:log-group:${local.log_group_name}"

  enable_kms  = var.enable_kms || var.kms_key_arn != ""
  create_kms  = var.enable_kms && var.kms_key_arn == ""
  kms_key_arn = var.kms_key_arn != "" ? var.kms_key_arn : (var.enable_kms ? aws_kms_key.registrations[0].arn : "")

  use_vpc = var.vpc_id != "" && length(var.subnet_ids) > 0

  # ONE SELECTOR FOR THE FLEET'S NETWORK ROLE AND ITS POLICY, because the destroy
  # guard keys on it: written out twice as `create_fleet && use_vpc`, removing the
  # VPC configuration destroyed both under a queued build with the guard untouched.
  create_fleet_network_role = local.create_fleet && local.use_vpc

  # A CONTAINER ENVIRONMENT NEEDS PRIVILEGED MODE FOR DOCKER, and an EC2 or macOS
  # environment IS the machine — so there is nothing to privilege there and setting
  # it is refused rather than ignored. A GitHub Actions job routinely runs
  # `docker build`, so a container tier without this fails on its first Docker step.
  privileged = can(regex("CONTAINER$", var.environment_type))
}

# A MINIMAL PLACEHOLDER BUILDSPEC, and that is deliberate: billet sends its own
# buildspec as buildspecOverride on every launch, for the reason ADR-002 gives about
# not shipping a Packer template — the contract between billet and its runner is a few
# lines of shell, and a project-owned buildspec is a second place for them to
# disagree. CreateProject still needs one to validate, so this is it.
#
# NO_SOURCE, because billet clones nothing: the runner does the checkout inside the
# job. billet also pins sourceTypeOverride on every launch, so a project that later
# acquired a source cannot make CodeBuild fetch a repository before the runner starts.
resource "aws_codebuild_project" "this" {
  count = local.create_project ? 1 : 0

  name           = local.project_name
  description    = "billet: GitHub Actions runners started through the CodeBuild API"
  service_role   = aws_iam_role.build[0].arn
  build_timeout  = var.build_timeout_minutes
  queued_timeout = var.queued_timeout_minutes

  concurrent_build_limit = var.concurrent_build_limit > 0 ? var.concurrent_build_limit : null

  artifacts {
    type = "NO_ARTIFACTS"
  }

  source {
    type = "NO_SOURCE"
    buildspec = yamlencode({
      version = "0.2"
      phases = {
        build = {
          commands = [
            "echo 'billet replaces this buildspec on every launch; a build running it started outside billet'",
          ]
        }
      }
    })
  }

  environment {
    type            = var.environment_type
    compute_type    = var.compute_type
    image           = var.image != "" ? var.image : local.default_image
    privileged_mode = local.privileged

    dynamic "fleet" {
      for_each = local.fleet_arn != "" ? [local.fleet_arn] : []

      content {
        fleet_arn = fleet.value
      }
    }
  }

  logs_config {
    cloudwatch_logs {
      status     = "ENABLED"
      group_name = local.log_group_name
    }
  }

  dynamic "vpc_config" {
    for_each = local.use_vpc ? [1] : []

    content {
      vpc_id             = var.vpc_id
      subnets            = var.subnet_ids
      security_group_ids = var.security_group_ids
    }
  }

  # THE OWNER TAG IS HALF THE OWNERSHIP BOUNDARY. A build cannot be tagged, so the
  # project's tag plus the per-build environment markers are what tell billet's
  # compute from anybody else's — and List feeds a loop that stops builds. The
  # deployment identity is minted on the control plane's first run, so it is usually
  # unknown at apply; `billet check` warns when it is absent.
  tags = merge(local.tags, var.deployment_id != "" ? {
    "sh.billet.owner" = var.deployment_id
  } : {})
}

locals {
  # A DEFAULT IMAGE PER ENVIRONMENT, because CreateProject requires one and the right
  # answer differs per environment. billet overrides it per launch from the tier, so
  # this is the project's fallback rather than what a job gets.
  default_image = lookup({
    LINUX_CONTAINER     = "aws/codebuild/amazonlinux-x86_64-standard:5.0"
    ARM_CONTAINER       = "aws/codebuild/amazonlinux-aarch64-standard:3.0"
    LINUX_GPU_CONTAINER = "aws/codebuild/amazonlinux-x86_64-gpu-standard:5.0"
    LINUX_EC2           = "aws/codebuild/amazonlinux-x86_64-standard:5.0"
    ARM_EC2             = "aws/codebuild/amazonlinux-aarch64-standard:3.0"
    MAC_ARM             = "aws/codebuild/macos-arm-base:14"
  }, var.environment_type, "aws/codebuild/amazonlinux-x86_64-standard:5.0")
}

# THE RESERVED FLEET. A standing cost rather than a per-job one: it carries an initial
# per-instance charge and bills while provisioned, whether or not anything is building.
#
# ITS VPC CONFIGURATION IS THE ONE THAT GOVERNS. A fleetOverride on StartBuild makes
# CodeBuild ignore the PROJECT's VPC config, so a network reviewed on the project
# proves nothing about a build that named a fleet — which is why the same settings are
# applied here.
resource "aws_codebuild_fleet" "this" {
  count = local.create_fleet ? 1 : 0

  name              = "${var.name}-fleet"
  base_capacity     = var.fleet_capacity
  compute_type      = var.compute_type
  environment_type  = var.environment_type
  overflow_behavior = var.fleet_overflow

  # AWS REQUIRES THE PAIR: a fleet with a VPC configuration must also name a service
  # role, because it is the role AWS assumes to create network interfaces for the
  # fleet's instances. `terraform validate` is what said so — "all of
  # `fleet_service_role,vpc_config` must be specified" — which is the argument for
  # running it rather than reading the provider docs.
  fleet_service_role = local.use_vpc ? aws_iam_role.fleet[0].arn : null

  dynamic "vpc_config" {
    for_each = local.use_vpc ? [1] : []

    content {
      vpc_id             = var.vpc_id
      subnets            = var.subnet_ids
      security_group_ids = var.security_group_ids
    }
  }

  tags = local.tags
}

# THE DESTROY GUARD. `terraform destroy` removes the node role, the build role and the
# log group a RUNNING build depends on, and AWS will not stop it for you: DeleteProject
# succeeds while a build is in progress and the build carries on to completion — measured
# 2026-09-02 (docs/reference/records/aws-acceptance.md), against an earlier claim here that AWS refused.
# GitHub does not requeue a job whose runner vanished mid-execution, so a destroy under
# a live build is somebody's failed build, reported as a green apply.
#
# This resource DEPENDS ON everything a build needs, so on destroy Terraform tears it
# down FIRST — dependents go before their dependencies — and its destroy-time provisioner
# refuses while the project has any build that is not terminal. The refusal aborts the
# plan with the project, both roles and the log group intact.
#
# `triggers_replace` IS THE PROJECT AND THE REGION, AND `input` ALONE WOULD NOT DO.
# A terraform_data's `input` updates IN PLACE — only `triggers_replace` replaces it —
# so with the project name in `input` only, renaming the project would replace the
# PROJECT (destroying the old one) while merely updating the guard, and the guard's
# destroy provisioner would never run: an ordinary apply removing infrastructure under
# a live build, which is the case this resource exists for. A review caught that. With
# the same two facts in `triggers_replace`, a project or region change REPLACES the
# guard first, and replacing it runs the destroy provisioner against the OLD project
# (the values still in `input` at that moment), which is exactly the project about to
# retargeted away from: a module-created project is itself replaced by those changes,
# and an adopted one keeps running while the roles and log group its builds depend
# on move to the new name. The module's OTHER build-facing identities are in the set
# too — the name that the roles carry, the parameter path the build role may read
# and the log group it may write — so changing any of them under a running build
# makes the apply FAIL at the guard rather than complete silently.
#
# THE OWNERSHIP SELECTORS ARE IN THE SET TOO, because a name can stay the same while
# the resource behind it goes: setting project_name to the name the module was
# already using flips the project from created to adopted — count 1 to 0, the old
# project DESTROYED — with local.project_name unchanged, and a guard keyed on the
# name alone would not have run. A review caught that; the next round caught the
# same for the fleet, the key and an adopted project's build role, which is the
# name its inline policy is attached to. So the rule is the SET, not the list:
# EVERY created-or-adopted selector this module has is in triggers_replace, and a
# new one belongs there too.
#
# THE GUARD IS A SNAPSHOT, AND `billet drain --wait` IS THE PRECONDITION IT CHECKS
# FOR, NOT A REPLACEMENT FOR IT. Between the script's last listing and the destroy
# that follows, a control plane that is still admitting work can escrow a job and
# start a build, which then loses its roles and log group underneath it. Nothing a
# provisioner can ask AWS proves that admission is sealed, and an environment
# variable asserting it proves nothing either — it is the same assertion the waiver
# already is. So the documented order is drain, then destroy, and the guard is what
# catches a destroy attempted without the drain.
#
# AND ONE THING NO RESOURCE HERE CAN GUARD: Terraform runs a destroy-time provisioner
# only while its configuration is still present, so deleting this module's block
# from a root and applying destroys everything with no guard at all. The supported
# removal is `terraform destroy -target=module.<name>` first, then deleting the
# block; the README says so.
#
# WHAT THAT DOES AND DOES NOT ORDER, said plainly: Terraform destroys the old guard
# before it destroys or replaces the resources the guard depends on, and promises
# nothing about IN-PLACE updates on the same apply — a changed policy document may
# already have landed when the guard refuses. So a rename is caught, not prevented,
# and the guard is a gate on DESTROY and on replacement; reconfiguring what a live
# build depends on is what `billet drain --wait` is for, and the README says so. A
# timeout, a tag or an image change never touches the guard at all.
#
# THE ESCAPE IS AN ENVIRONMENT VARIABLE read by the script rather than a module
# variable, because a destroy-time provisioner sees the values IN STATE, not the ones
# on the command line: a `-var` at `terraform destroy` time would not reach it, and a
# module variable would need its own apply first. The window is CodeBuild's service
# ceilings rather than the module's timeouts because they are sound for ANY project —
# an adopted one keeps whatever timeouts it had, which this module never sets.
#
# A destroy-time provisioner may reference only `self` and `path.module`, which is why
# the two facts travel through `input` and the script lives beside this file. It needs
# the aws CLI on the operator's PATH, with credentials that may list and describe
# builds in the project; the module's README says so.
resource "terraform_data" "active_build_guard" {
  input = {
    project = local.project_name
    region  = local.region
  }

  triggers_replace = [
    local.project_name,
    local.region,
    var.name,
    local.parameter_path,
    local.log_group_name,
    local.build_role_name,
    local.create_project,
    local.create_log_group,
    local.create_fleet,
    local.create_fleet_network_role,
    local.create_kms,
  ]

  depends_on = [
    aws_codebuild_project.this,
    aws_codebuild_fleet.this,
    aws_kms_key.registrations,
    aws_iam_role_policy.node,
    aws_iam_instance_profile.node,
    aws_iam_role_policy.build,
    aws_iam_role_policy.fleet,
    aws_cloudwatch_log_group.this,
  ]

  # THE SCRIPT PATH GOES THROUGH THE ENVIRONMENT AND IS QUOTED, because local-exec
  # hands `command` to /bin/sh -c and a copied module under a path with a space in
  # it would be word-split — failing closed, which makes a legitimate teardown
  # impossible rather than unsafe.
  provisioner "local-exec" {
    when    = destroy
    command = "\"$BILLET_GUARD_SCRIPT\""

    environment = {
      BILLET_GUARD_SCRIPT  = "${path.module}/scripts/refuse-active-builds.sh"
      BILLET_GUARD_PROJECT = self.input.project
      BILLET_GUARD_REGION  = self.input.region
    }
  }
}

resource "aws_cloudwatch_log_group" "this" {
  count = local.create_log_group ? 1 : 0

  name              = local.log_group_name
  retention_in_days = var.log_retention_days
  tags              = local.tags
}

# A PER-DEPLOYMENT KEY FOR THE STAGED REGISTRATIONS.
#
# Without one, a SecureString is encrypted under the account's aws/ssm key, which
# authorizes any principal in the account that can reach Parameter Store — so a
# neighbouring deployment's role could read this one's registrations. With one, both
# roles' KMS grants are scoped to exactly this key AND to Parameter Store by
# kms:ViaService, so the grant cannot be used to decrypt anything else it protects.
#
# The key policy delegates to IAM, which is what makes the identity-policy scoping
# above decisive — a key whose own policy admits foreign roles reopens the boundary on
# a side no identity policy can see.
resource "aws_kms_key" "registrations" {
  count = local.create_kms ? 1 : 0

  description             = "billet ${var.name}: staged GitHub Actions runner registrations"
  deletion_window_in_days = 7
  enable_key_rotation     = true
  tags                    = local.tags
}

resource "aws_kms_alias" "registrations" {
  count = local.create_kms ? 1 : 0

  name          = "alias/${var.name}-billet-registrations"
  target_key_id = aws_kms_key.registrations[0].key_id
}
