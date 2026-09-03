terraform {
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}

# AWS-MANAGED COMPUTE BEHIND AN EXISTING BILLET CONTROL PLANE.
#
# Linux on demand is the inexpensive default and is created unconditionally.
# macOS reserved capacity is an EXPLICIT OPT-IN, because it is a standing cost
# rather than a per-job one — see the `enable_macos` variable.
#
# This example creates NO control plane. It is the half of a deployment that adds
# compute; `examples/co-located` is the whole thing on one host. Point the node at
# an existing control plane with `billet ca issue <node>` and node.server_addr.

provider "aws" {
  # us-west-2 is one of the five regions offering reserved-capacity macOS Medium
  # fleets, so the same region serves both halves of this example.
  region = "us-west-2"
}

variable "enable_macos" {
  description = <<-EOT
    Create the reserved-capacity macOS fleet.

    OFF BY DEFAULT, AND THAT IS ABOUT COST RATHER THAN COMPLEXITY. A reserved fleet
    carries an initial per-instance charge and bills for as long as it is
    provisioned, whether or not anything is building — unlike the Linux fleet below,
    which bills per build minute. Check the current numbers on
    https://aws.amazon.com/codebuild/pricing/ before turning it on, and destroy the
    fleet when you are done with it.

    THE ACCOUNT QUOTA WILL STOP YOU FIRST. Mac ARM fleets default to ONE
    concurrently running instance per region; raise it in Service Quotas before
    setting fleet_capacity above 1.
  EOT
  type        = bool
  default     = false
}

variable "macos_compute_type" {
  description = <<-EOT
    Which Apple silicon shape the macOS fleet runs.

      BUILD_GENERAL1_MEDIUM   Apple M2, 24 GB, 8 vCPU
      BUILD_GENERAL1_LARGE    Apple M2, 32 GB, 12 vCPU

    REGION AVAILABILITY DIFFERS BETWEEN THEM, which is the trap: Medium is offered
    in us-east-1, us-east-2, us-west-2, ap-southeast-2 and eu-central-1, while Large
    is offered in the first four only. A shape that is not offered in the provider's
    region fails at fleet creation.
  EOT
  type        = string
  default     = "BUILD_GENERAL1_MEDIUM"

  validation {
    condition = contains([
      "BUILD_GENERAL1_MEDIUM", "BUILD_GENERAL1_LARGE",
    ], var.macos_compute_type)
    error_message = "macOS reserved capacity offers only BUILD_GENERAL1_MEDIUM and BUILD_GENERAL1_LARGE."
  }
}

# LINUX ON DEMAND: the inexpensive default.
#
# On-demand compute is billed per build minute and the machine is destroyed when the
# build finishes. No fleet, so no standing cost and nothing to remember to delete.
module "linux" {
  source = "../../modules/fleet-codebuild"

  name             = "billet-linux"
  environment_type = "LINUX_CONTAINER"
  compute_type     = "BUILD_GENERAL1_MEDIUM"

  # A PER-DEPLOYMENT KEY FOR THE STAGED RUNNER REGISTRATIONS. Without one they are
  # encrypted under the account's aws/ssm key, which authorizes any principal in the
  # account that can reach Parameter Store — so a neighbouring deployment's role
  # could read this one's. Worth the key on any shared account.
  enable_kms = true

  # A TIGHTER CEILING THAN THE SERVICE MAXIMUM, and it is a real choice rather than
  # tidiness. It caps how long a job may run — CodeBuild's own maximum is 2160
  # minutes and billet cannot lift it — and it also sizes billet's inventory walk,
  # because CodeBuild offers no way to list only active builds. Six hours matches
  # GitHub's own hosted-runner limit and makes each sweep cheap. Raise it for longer
  # jobs, or put them on owned EC2 or Mac capacity where billet imposes no limit.
  build_timeout_minutes  = 360
  queued_timeout_minutes = 60

  tags = { Environment = "ci" }
}

# macOS ON RESERVED CAPACITY: an explicit opt-in.
module "macos" {
  count = var.enable_macos ? 1 : 0

  source = "../../modules/fleet-codebuild"

  name             = "billet-macos"
  environment_type = "MAC_ARM"
  compute_type     = var.macos_compute_type

  # RESERVED CAPACITY IS NOT OPTIONAL HERE. On-demand CodeBuild does not offer macOS
  # at all, so the module refuses the pairing at plan time rather than letting AWS
  # refuse it per job.
  enable_fleet   = true
  fleet_capacity = 1

  # QUEUE rather than ON_DEMAND, because ON_DEMAND does not support macOS and would
  # bill overflow separately besides. Note that a queued build FAILS after
  # queued_timeout_minutes, so a fleet at capacity turns into failed jobs rather than
  # slow ones — which is the argument for raising fleet_capacity rather than the
  # queued timeout.
  fleet_overflow = "QUEUE"

  enable_kms = true

  build_timeout_minutes  = 360
  queued_timeout_minutes = 60

  tags = { Environment = "ci" }
}

# TEARING DOWN. `terraform destroy` under a running build would remove the roles and
# log group that build depends on, and AWS does not refuse it (DeleteProject succeeds
# and the build runs on; measured). The module's destroy-time guard refuses instead,
# through the aws CLI on your PATH, until every build in the project is terminal — so
# drain with `billet drain --wait` first, and see the module README for the waiver.
# TO REMOVE ONE MODULE, `terraform destroy -target=module.macos` BEFORE deleting its
# block: a destroy-time provisioner runs only while its configuration is present, so
# deleting the block and applying skips the guard entirely (Terraform's rule).

# WHAT TO PUT IN billet.yaml.
#
# The module writes no billet configuration — Terraform owns AWS infrastructure and
# the junioryono.billet.host Ansible role owns billet.yaml. These are the non-secret
# facts that role's billet_config needs.
#
# ONE BILLET NODE PER FLEET. A reserved fleet's capacity is shared, so two nodes
# naming it would each advertise all of it and the deployment would promise GitHub
# more concurrent jobs than AWS will run. The control plane refuses the second one.
output "linux_node_config" {
  description = "node.codebuild for the Linux orchestrator."
  value = {
    region             = "us-west-2"
    project            = module.linux.project_name
    environment_type   = module.linux.environment_type
    jit_parameter_path = module.linux.parameter_path
    jit_kms_key_id     = module.linux.kms_key_arn
    log_group          = module.linux.log_group_name

    # NO DEFAULT ON THIS KEY, and its absence is a refusal. Every job on this node
    # inherits CodeBuild's ceilings; setting it is how an operator says they have
    # read that.
    accept_external_build_ceiling = true

    build_timeout_minutes  = 360
    queued_timeout_minutes = 60

    # PRIVILEGED MODE IS WHAT MAKES DOCKER WORK in a container environment, and a
    # real Actions workflow reaches `docker build` almost immediately.
    privileged_mode = true

    # THE PRICE IS YOURS TO AUDIT. billet ships no table of compute types, so a shape
    # it may buy is declared along with what it holds — which keeps the cost surface
    # in your own file. Replace the rate with the current published one.
    compute_types = [{
      type               = module.linux.compute_type
      vcpu               = 4
      memory             = "7GiB"
      price_usd_per_hour = 0.01
    }]
  }
}

output "macos_node_config" {
  description = "node.codebuild for the macOS orchestrator, when enabled."
  value = var.enable_macos ? {
    region             = "us-west-2"
    project            = module.macos[0].project_name
    fleet_arn          = module.macos[0].fleet_arn
    environment_type   = module.macos[0].environment_type
    jit_parameter_path = module.macos[0].parameter_path
    jit_kms_key_id     = module.macos[0].kms_key_arn
    log_group          = module.macos[0].log_group_name

    accept_external_build_ceiling = true

    build_timeout_minutes  = 360
    queued_timeout_minutes = 60

    # NOT SET, and refused if it were: a macOS environment IS the machine, so there
    # is no container privilege to grant.
    privileged_mode = false

    compute_types = [{
      type               = module.macos[0].compute_type
      vcpu               = 8
      memory             = "24GiB"
      price_usd_per_hour = 0.10
    }]
  } : null
}

output "macos_node_policy" {
  description = <<-EOT
    The nodes[] entry a macOS tier's host needs.

    macos_vm_limit IS REQUIRED for a macOS tier on a managed fleet. billet will not
    assume Apple's per-host allowance of 2 applies to a fleet AWS operates under its
    own agreement, so it asks for the number — and this is the fleet's capacity.
  EOT
  value = var.enable_macos ? {
    name           = "aws-cb-macos"
    provider       = "codebuild"
    guest_os       = ["macos"]
    macos_vm_limit = module.macos[0].fleet_capacity
  } : null
}

output "node_instance_profiles" {
  description = <<-EOT
    Instance profiles for the machines running `billet node`.

    They are how billet reads AWS credentials from IMDS, which is why node.codebuild
    has no credential fields: a long-lived access key in a unit file is a credential
    that never expires, on a host whose whole job is launching compute.
  EOT
  value = {
    linux = module.linux.node_instance_profile
    macos = var.enable_macos ? module.macos[0].node_instance_profile : null
  }
}

output "controller_sweep_policies" {
  description = <<-EOT
    The grant the CONTROL PLANE's role needs over each fleet's parameter path, so it can
    remove the staged runner registrations a dead node left behind.

    Pass controller_role_name to each module instead when terraform owns the controller
    role (in the root module's co-located topology that is fleet-ec2's node role), and
    these are attached for you. It belongs on the machine running `billet server` and
    nowhere else: it lists and deletes under the path, on the ledger's authority.
  EOT
  value = {
    linux = module.linux.controller_sweep_policy_json
    macos = var.enable_macos ? module.macos[0].controller_sweep_policy_json : null
  }
}

output "external_ceilings" {
  description = "The limits every job on these fleets inherits, which billet cannot lift."
  value = {
    linux = module.linux.external_ceilings
    macos = var.enable_macos ? module.macos[0].external_ceilings : null
  }
}
