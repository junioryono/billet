# THE MODULE WRITES NO BILLET CONFIGURATION. It outputs the non-secret facts
# billet.yaml needs, which the junioryono.billet.host Ansible role renders — the seam
# ADR-004 sets and fleet-ec2 already follows. Certificates and the App key are
# installed by the configuration layer, never by Terraform.

output "project_name" {
  description = "node.codebuild.project"
  value       = local.project_name
}

output "project_arn" {
  description = "The project ARN both roles are scoped to."
  value       = local.project_arn
}

output "fleet_arn" {
  description = "node.codebuild.fleet_arn, or empty for on-demand compute."
  value       = local.fleet_arn
}

output "environment_type" {
  description = "node.codebuild.environment_type"
  value       = var.environment_type
}

output "compute_type" {
  description = <<-EOT
    The compute type to declare in node.codebuild.compute_types.

    BILLET NEEDS ITS vCPU, MEMORY AND PRICE TOO, and this module deliberately does
    not supply them: billet ships no table of compute types, so a shape it may buy is
    declared along with what it holds — which keeps the fleet's cost surface in the
    operator's own file, where a spending decision belongs. See the README for the
    published numbers.
  EOT
  value       = var.compute_type
}

output "parameter_path" {
  description = "node.codebuild.jit_parameter_path"
  value       = local.parameter_path
}

output "kms_key_arn" {
  description = "node.codebuild.jit_kms_key_id, or empty for the account's aws/ssm key."
  value       = local.kms_key_arn
}

output "log_group_name" {
  description = "node.codebuild.log_group"
  value       = local.log_group_name
}

output "node_instance_profile" {
  description = <<-EOT
    Instance profile for the machine running `billet node`.

    It is how billet reads AWS credentials from IMDS, which is why node.codebuild has
    no credential fields: a long-lived access key in a unit file is a credential that
    never expires, on a host whose whole job is launching compute.
  EOT
  value       = aws_iam_instance_profile.node.name
}

output "node_role_arn" {
  description = "ARN of the node role, for attaching further policies."
  value       = aws_iam_role.node.arn
}

output "build_role_arn" {
  description = <<-EOT
    ARN of the role the BUILD runs as — the one this module created, or the adopted
    project's own.

    Every permission it holds is a permission the workflow holds, so anything attached
    to it is attached to arbitrary job code. It reads one parameter and writes logs, and
    the module deliberately puts NO networking permissions on it: a VPC configuration
    requires a fleet, whose separate service role carries those instead.
  EOT
  value       = local.build_role_arn
}

output "controller_sweep_policy_json" {
  description = <<-EOT
    The policy the CONTROL PLANE's role needs to sweep staged runner registrations a
    dead node left under this fleet's parameter path: `ssm:GetParametersByPath` and
    `ssm:DeleteParameter` on the path, and nothing else.

    Attached for you when controller_role_name is set; rendered here for a controller
    role terraform does not own. It belongs on the machine running `billet server` —
    never on the node's role and never on the build's.
  EOT
  value       = local.controller_sweep_policy
}

output "fleet_capacity" {
  description = <<-EOT
    Concurrent builds a created fleet can run, or 0 when none was created.

    THIS IS THE NUMBER A macOS TIER'S nodes[].macos_vm_limit SHOULD BE. billet refuses
    to assume Apple's per-host allowance applies to a fleet AWS operates under its own
    agreement, so it asks — and this is the answer.
  EOT
  value       = local.create_fleet ? var.fleet_capacity : 0
}

output "external_ceilings" {
  description = <<-EOT
    The two limits every job on this fleet inherits, which billet cannot lift.

    Rendered as an output rather than left in a README because billet requires them
    visible before work is admitted, and an operator reading a plan is exactly then.
  EOT
  value = {
    build_timeout_minutes  = var.build_timeout_minutes
    queued_timeout_minutes = var.queued_timeout_minutes
    note = join(" ", [
      "A build is capped at ${var.build_timeout_minutes} minutes and a build still",
      "waiting for capacity is FAILED after ${var.queued_timeout_minutes}.",
      "Both are CodeBuild's own limits. Work that can exceed either belongs on owned",
      "EC2 or Mac capacity, where billet imposes no job limit.",
      "Set node.codebuild.accept_external_build_ceiling to true to acknowledge them;",
      "billet refuses a node that has not.",
    ])
  }
}
