variable "name" {
  description = "Name prefix for every resource this module creates."
  type        = string

  validation {
    # Lowercase, so the UPPERCASE sentinels in the committed policy renderings can
    # never collide with a real value — the same reason fleet-ec2 validates it.
    condition     = can(regex("^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$", var.name))
    error_message = "name must be 2-32 lowercase alphanumerics or hyphens."
  }
}

variable "project_name" {
  description = <<-EOT
    CodeBuild project billet starts builds in. Empty creates one named "<name>".

    THE PROJECT MUST BE BILLET'S ALONE, and that is not a preference. A CodeBuild
    build cannot be tagged — tags exist on projects and report groups, and
    StartBuild has no field that becomes one — so the project ARN is the entire IAM
    boundary between billet's builds and anybody else's, and billet's own List feeds
    a loop that STOPS builds. Adopting a project you also use for something else is
    how billet comes to stop somebody else's work.
  EOT
  type        = string
  default     = ""
}

variable "existing_build_role_name" {
  description = <<-EOT
    The service role an ADOPTED project already runs its builds as. Required with
    project_name, and refused without it.

    WHY IT IS REQUIRED RATHER THAN OPTIONAL. Adopting a project means this module does
    not create it, so nothing sets its service role — the project keeps whatever role
    it had. Creating a role and attaching billet's policy to that instead produced a
    role nothing used: the adopted project's builds could not read their own
    registration, every job failed to register, and the module reported success. Worse,
    it left whatever permissions the old role happened to hold in place.

    So the module attaches its generated build-role policy to THIS role, and the
    project's own configuration is untouched. Every permission that policy grants is
    one the workflow holds, so read it before pointing it at a role you share with
    something else.
  EOT
  type        = string
  default     = ""

  validation {
    condition     = (var.project_name == "") == (var.existing_build_role_name == "")
    error_message = <<-EOT
      project_name and existing_build_role_name go together. Adopting a project means
      this module does not set its service role, so billet's build-role policy has to
      be attached to the role that project already uses — otherwise the policy lands on
      a role nothing runs as, and every build fails to read its registration while the
      apply reports success. Creating a project sets its role, so neither is needed.
    EOT
  }
}

variable "environment_type" {
  description = <<-EOT
    CodeBuild environment builds run in. It decides the node's guest OS.

    MAC_ARM is AWS-managed Apple silicon and is RESERVED CAPACITY ONLY: on-demand
    CodeBuild does not offer macOS at all, so it requires enable_fleet.
  EOT
  type        = string
  default     = "LINUX_CONTAINER"

  validation {
    condition = contains([
      "LINUX_CONTAINER", "ARM_CONTAINER", "LINUX_GPU_CONTAINER",
      "LINUX_EC2", "ARM_EC2", "MAC_ARM",
    ], var.environment_type)
    error_message = <<-EOT
      environment_type must be one billet can run a GitHub Actions job in.
      Lambda environments offer no container privilege, so docker build, service
      containers and compose all fail; Windows has no billet runner image.
    EOT
  }
}

variable "compute_type" {
  description = "CodeBuild compute type for the project and any fleet."
  type        = string
  default     = "BUILD_GENERAL1_MEDIUM"

  validation {
    condition     = !can(regex("(?i)lambda", var.compute_type))
    error_message = <<-EOT
      Lambda compute cannot run a GitHub Actions job: it offers no container
      privilege, so docker build and service containers fail. A tier on it would be
      admitted and then fail every job.
    EOT
  }
}

variable "image" {
  description = <<-EOT
    Container image or AMI the builds use. Empty leaves CodeBuild's default for the
    environment.

    billet overrides the image per LAUNCH from the tier's launch.codebuild.image, so
    this is the project's fallback rather than what a job gets.
  EOT
  type        = string
  default     = ""
}

variable "enable_fleet" {
  description = <<-EOT
    Create a reserved-capacity fleet. Required for MAC_ARM and for the EC2
    "instance running mode" environments.

    A RESERVED FLEET IS A STANDING COST, not a per-job one: it carries an initial
    per-instance charge and bills for as long as it is provisioned, whether or not
    anything is building. It is also NOT a per-job isolation boundary — AWS
    documents its instances as staying alive between builds and sharing cached data
    with other projects in the account — which is why billet refuses untrusted work
    on this backend outright.
  EOT
  type        = bool
  default     = false
}

variable "fleet_arn" {
  description = "Existing reserved-capacity fleet to adopt instead of creating one."
  type        = string
  default     = ""

  # macOS AND THE EC2 ENVIRONMENTS EXIST ONLY ON RESERVED CAPACITY, so naming one
  # without a fleet describes builds AWS refuses PER JOB — which reads as a transient
  # failure rather than as a configuration that can never work. This turns it into one
  # plan error, on the variable an operator would set to fix it.
  #
  # A CROSS-VARIABLE validation, which Terraform 1.9 permits and which the module's
  # required_version already floors at. The alternative was a zero-count resource
  # carrying a lifecycle precondition, which needs a provider for a resource that is
  # never created.
  validation {
    condition = (
      !contains(["MAC_ARM", "LINUX_EC2", "ARM_EC2"], var.environment_type) ||
      var.enable_fleet || var.fleet_arn != ""
    )
    error_message = <<-EOT
      environment_type ${var.environment_type} exists only on reserved capacity —
      on-demand CodeBuild does not offer it, and does not offer macOS at all. Set
      enable_fleet to create a fleet, or fleet_arn to adopt one.
    EOT
  }
}

variable "fleet_capacity" {
  description = <<-EOT
    Concurrent builds a created fleet can run.

    THIS IS THE NUMBER A macOS TIER'S nodes[].macos_vm_limit SHOULD BE. billet
    refuses to assume Apple's per-host allowance of 2 applies to a fleet AWS
    operates under its own agreement, so it asks for the capacity instead — and this
    is where the answer comes from. The default account quota for Mac ARM fleets is
    ONE concurrently running instance; raise it in Service Quotas first.
  EOT
  type        = number
  default     = 1

  validation {
    condition     = var.fleet_capacity >= 1
    error_message = "fleet_capacity must be at least 1."
  }
}

variable "fleet_overflow" {
  description = <<-EOT
    What a created fleet does when demand exceeds its capacity: QUEUE or ON_DEMAND.

    QUEUE is the default and the safe one for cost. Note that a queued build FAILS
    after queued_timeout_minutes — CodeBuild's ceiling is 8 hours — so a fleet that
    queues under sustained load turns into failed jobs rather than slow ones.
    ON_DEMAND bills overflow separately and does not support macOS.
  EOT
  type        = string
  default     = "QUEUE"

  validation {
    condition     = contains(["QUEUE", "ON_DEMAND"], var.fleet_overflow)
    error_message = "fleet_overflow must be QUEUE or ON_DEMAND."
  }
}

variable "build_timeout_minutes" {
  description = <<-EOT
    Build timeout, 5 to 2160 (36 hours). This is CodeBuild's own ceiling and billet
    cannot lift it; work that can exceed it belongs on owned EC2 or Mac capacity.

    It also sizes billet's inventory walk: CodeBuild offers no way to list only
    active builds, so reconciliation walks recent history and stops once every build
    it sees is older than the service could still be running. A tighter ceiling is a
    cheaper sweep.
  EOT
  type        = number
  default     = 2160

  validation {
    condition     = var.build_timeout_minutes >= 5 && var.build_timeout_minutes <= 2160
    error_message = "build_timeout_minutes must be between 5 and 2160 (CodeBuild's own limits)."
  }
}

variable "queued_timeout_minutes" {
  description = <<-EOT
    How long a build may wait for capacity before CodeBuild FAILS it, 5 to 480.

    The ceiling an operator does not expect. On a fleet at capacity with QUEUE
    overflow, this is what kills a job that never got a machine.
  EOT
  type        = number
  default     = 480

  validation {
    condition     = var.queued_timeout_minutes >= 5 && var.queued_timeout_minutes <= 480
    error_message = "queued_timeout_minutes must be between 5 and 480 (CodeBuild's own limits)."
  }
}

variable "concurrent_build_limit" {
  description = <<-EOT
    Project-level cap on concurrent builds. Zero leaves it unset.

    NOT A SUBSTITUTE FOR THE ACCOUNT QUOTA, which is per environment and compute
    type and defaults to ONE. This bounds what one project can do; the quota bounds
    what the account can.
  EOT
  type        = number
  default     = 0
}

variable "parameter_path" {
  description = <<-EOT
    Parameter Store path prefix the single-use runner registration is staged under.
    Empty derives "/billet/<name>/jit".

    A LITERAL, NEVER A WILDCARD. It lands in an IAM Resource ARN, so a `*` or `?`
    widens both roles' grants to sibling paths — which on a shared account are other
    deployments' runner registrations.
  EOT
  type        = string
  default     = ""

  validation {
    condition     = var.parameter_path == "" || can(regex("^(/[A-Za-z0-9_.-]+)+$", var.parameter_path))
    error_message = <<-EOT
      parameter_path must be an absolute Parameter Store path with no trailing slash
      and no wildcard. It lands in an IAM Resource arn.
    EOT
  }
}

variable "enable_kms" {
  description = <<-EOT
    Create a customer-managed key for the staged runner registrations.

    Empty uses the account's aws/ssm key, which authorizes any principal in the
    account that can reach Parameter Store. A per-deployment key is what keeps one
    deployment from reading another's staged registrations, and its grants are scoped
    to Parameter Store by kms:ViaService.
  EOT
  type        = bool
  default     = false
}

variable "kms_key_arn" {
  description = "Existing KMS key to encrypt staged registrations with, instead of creating one."
  type        = string
  default     = ""
}

variable "log_group_name" {
  description = "CloudWatch log group for builds. Empty creates \"/aws/codebuild/<project>\"."
  type        = string
  default     = ""
}

variable "log_retention_days" {
  description = "Retention for a created log group."
  type        = number
  default     = 30
}

variable "vpc_id" {
  description = <<-EOT
    VPC the builds run in. Empty leaves them on CodeBuild's own network.

    A fleetOverride on StartBuild makes CodeBuild IGNORE the project's VPC
    configuration — the fleet's own network governs — so with enable_fleet the VPC
    settings here are applied to the FLEET, not just the project.
  EOT
  type        = string
  default     = ""
}

variable "subnet_ids" {
  description = <<-EOT
    Subnets for VPC-connected builds. A reserved fleet supports exactly ONE subnet
    in ONE availability zone, and a VPC-connected build needs a NAT gateway to reach
    GitHub.
  EOT
  type        = list(string)
  default     = []

  # ONE SUBNET, IN ONE AVAILABILITY ZONE, and a NAT gateway for egress. AWS's own
  # documentation for reserved capacity, recorded in ADR-007 — refused here rather than
  # by the API partway through an apply.
  validation {
    condition     = length(var.subnet_ids) <= 1
    error_message = <<-EOT
      A reserved-capacity fleet takes exactly one subnet, in one availability zone, and
      reaches the internet through a NAT gateway. Pass a single subnet id.
    EOT
  }
}

variable "security_group_ids" {
  description = "Security groups for VPC-connected builds. Must allow outbound."
  type        = list(string)
  default     = []

  # A VPC REQUIRES A FLEET, AND THAT IS A SECURITY REFUSAL RATHER THAN A LIMITATION.
  #
  # A VPC-connected CodeBuild project's SERVICE ROLE needs ec2:CreateNetworkInterface
  # and ec2:DeleteNetworkInterface, which cannot be resource-scoped — and a service
  # role's credentials are reachable from inside the build, so attaching those to the
  # role a workflow runs as hands every job the ability to exhaust the account's ENI
  # quota or delete interfaces belonging to something else.
  #
  # A reserved fleet has its own service role for exactly this, and its credentials
  # never enter the build environment. Insisting on one costs nothing besides: billet
  # sends fleetOverride on every launch when a fleet is configured, and a fleetOverride
  # makes CodeBuild ignore the project's VPC configuration anyway — so this refuses the
  # only case that would have been unsafe.
  validation {
    condition = (
      (var.vpc_id == "" && length(var.subnet_ids) == 0 && length(var.security_group_ids) == 0) ||
      (var.enable_fleet && var.fleet_arn == "")
    )
    error_message = <<-EOT
      A VPC configuration requires a reserved-capacity fleet THIS MODULE CREATES
      (enable_fleet). Two separate reasons, and the second one is why an adopted
      fleet_arn is not enough.

      A VPC-connected project's service role needs unscopable
      ec2:CreateNetworkInterface and ec2:DeleteNetworkInterface, and that role's
      credentials are reachable from inside the build — so on-demand VPC builds would
      hand every workflow those permissions. A fleet has its own service role, whose
      credentials the build never sees.

      And with fleet_arn the module configures nothing: a fleet's VPC settings live on
      the fleet, this module cannot edit a fleet it does not own, and billet sends
      fleetOverride on every launch — which makes CodeBuild ignore the project's VPC
      configuration entirely. So the apply would succeed, the inputs would go nowhere,
      and the builds would run on whatever network that fleet already has. Configure
      the network on the adopted fleet itself and leave these unset.

      This also refuses enable_fleet TOGETHER WITH fleet_arn, which is the same hole
      through a narrower door: create_fleet is `enable_fleet && fleet_arn == ""`, so
      the adopted fleet wins, no fleet resource is created, and the VPC inputs reach
      nothing at all. The predicate has to match the one that decides whether a fleet
      exists, not the flag that asks for one.
    EOT
  }

  # AND THE INPUTS HAVE TO BE COMPLETE, because a partial set was a silent no-op:
  # `use_vpc` requires both a vpc_id and a subnet, so naming one without the other
  # produced an apply that succeeded and a fleet with no network configuration at all.
  validation {
    condition = (
      (var.vpc_id == "" && length(var.subnet_ids) == 0 && length(var.security_group_ids) == 0) ||
      (var.vpc_id != "" && length(var.subnet_ids) > 0 && length(var.security_group_ids) > 0)
    )
    error_message = <<-EOT
      A VPC configuration needs vpc_id, subnet_ids AND security_group_ids together, or
      all three left unset. A partial set is accepted by AWS and applied by nothing:
      the fleet is created without a network configuration and the apply reports
      success. Security groups are required rather than defaulted for the reason
      billet's ec2 backend gives — a VPC's default group allows all traffic between its
      members, so falling back to it is a network nobody reviewed.
    EOT
  }
}

variable "iam_policy_json" {
  description = <<-EOT
    Override the node role's policy entirely, e.g. with `billet init iam` output.
    Empty uses the module's committed rendering of billet's own generator.
  EOT
  type        = string
  default     = ""
}

variable "controller_role_name" {
  description = <<-EOT
    The IAM role the machine running `billet server` assumes, when this module should
    grant it the sweep of staged runner registrations under this fleet's parameter path.

    A THIRD PRINCIPAL. A codebuild node that dies between staging a build's registration
    and removing it leaks one parameter, and only the control plane can authorise the
    delete — the lease is terminal in its ledger, and has been for longer than any build
    could still be running. So the controller lists names under the path and deletes the
    ones the ledger has proved dead, and that needs `ssm:GetParametersByPath` and
    `ssm:DeleteParameter` on this path, on the CONTROLLER's role. Nothing else: no read
    of a registration, no staging, no key, no CodeBuild action.

    Empty attaches nothing; controller_sweep_policy_json carries the same document for
    a controller role terraform does not own. In the root module's co-located topology
    the controller runs under fleet-ec2's node role, so pass that role's name.
  EOT
  type        = string
  default     = ""
}

variable "tags" {
  description = "Tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}

variable "deployment_id" {
  description = <<-EOT
    Billet deployment identity to tag the project with, when it is already known.

    THE PROJECT IS HALF THE OWNERSHIP BOUNDARY, because a build cannot be tagged.
    The identity is minted on the control plane's first run, so it is usually unknown
    at apply time; `billet check --provider codebuild` warns when the tag is absent
    or names a different deployment. Set it once you have the id.
  EOT
  type        = string
  default     = ""
}
