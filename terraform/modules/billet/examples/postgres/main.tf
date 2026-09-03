terraform {
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}

# THE COMPOSITION EXAMPLE: YOUR NETWORK, A POSTGRESQL LEDGER, ONE CONTROLLER, AND
# AWS-MANAGED COMPUTE.
#
# Every other example here creates the pieces it uses. This one exists to show
# the ADOPTION BOUNDARIES, because that is the shape a deployment into an
# organisation's existing account actually takes: the network is somebody else's,
# the database might be, the compute is AWS's, and the only thing billet insists
# on owning is its own identity.
#
# WHAT IS ADOPTED HERE:
#
#   the VPC and its subnets   — passed in; nothing below creates a network, a
#                               route, a gateway or a NAT
#   the PostgreSQL ledger     — created by default, but set `ledger_endpoint` and
#                               state-rds-postgres creates NOTHING and grants
#                               instead
#
# WHAT IS CREATED:
#
#   the controller and its security group, its auto-recovery alarm and its backup
#   bucket; the CodeBuild project, its two roles, its log group and its JIT
#   parameter path.
#
# WHAT IS NOT HERE, AND WILL NOT BE: billet.yaml, the mTLS certificates and the
# systemd units. ADR-004 puts those in the configuration layer — the
# junioryono.billet.host Ansible role — and enrollment stays a human fingerprint
# comparison outside `terraform apply`. The outputs at the bottom are the
# non-secret facts that role needs.

provider "aws" {
  region = var.region
}

variable "region" {
  description = "The region everything here lives in."
  type        = string
  default     = "us-west-2"
}

variable "vpc_id" {
  description = "YOUR VPC. Nothing here creates one."
  type        = string
}

variable "vpc_cidr" {
  description = "Its CIDR, which is what the node wire is opened to by default."
  type        = string
}

variable "controller_subnet_id" {
  description = "The subnet the controller launches in. It must be able to reach GitHub — a private subnet needs a NAT gateway you already have."
  type        = string
}

variable "ledger_subnet_ids" {
  description = "At least two subnets in different availability zones for the ledger's subnet group. RDS requires two zones even for a single-AZ instance. Ignored when ledger_endpoint is set."
  type        = list(string)
  default     = []
}

variable "ledger_endpoint" {
  description = "AN EXISTING PostgreSQL endpoint to adopt. Empty creates one. With it set, state-rds-postgres creates nothing at all and your credential story stays yours — which is the boundary this example exists to show."
  type        = string
  default     = ""
}

variable "ledger_dsn_secret_arn" {
  description = "A Secrets Manager secret holding the whole BILLET_STATE_DSN, for an adopted endpoint. The controller is granted read on it. Leave empty for a created database, whose AWS-managed master credential is granted instead."
  type        = string
  default     = ""
}

variable "ssh_ingress_cidrs" {
  description = "Where you converge the controller from. Empty opens nothing, and then you reach it through SSM or a bastion of your own."
  type        = list(string)
  default     = []
}

variable "key_name" {
  description = "EC2 key pair for that SSH; empty attaches none."
  type        = string
  default     = ""
}

# THE LEDGER.
#
# The two modules reference each other in ONE DIRECTION EACH, which is what keeps
# the graph acyclic: the ledger admits the controller's SECURITY GROUP, and the
# controller is granted the ledger's SECRET. Nothing needs the other's address.
module "ledger" {
  source = "../../modules/state-rds-postgres"

  name           = "billet"
  endpoint       = var.ledger_endpoint
  dsn_secret_arn = var.ledger_dsn_secret_arn

  vpc_id     = var.vpc_id
  subnet_ids = var.ledger_subnet_ids
  # KEYED BY A NAME, not by position: the key is the rule's Terraform address, so
  # adding a second client later does not renumber and recreate the controller's
  # own ingress. The VALUE is unknown until apply, which is fine; a `for_each`
  # needs only its keys at plan time.
  client_security_groups = { controller = module.controller.security_group_id }

  tags = { Environment = "ci" }
}

# THE CONTROLLER.
#
# One, and one only. billet takes a claim row in the ledger before it polls
# GitHub, so a second controller is refused at startup — but that catches the
# accident rather than providing failover, and nothing here promotes a standby.
module "controller" {
  source = "../../modules/control-plane-postgres"

  name             = "billet"
  vpc_id           = var.vpc_id
  subnet_id        = var.controller_subnet_id
  vpc_cidr         = var.vpc_cidr
  subnet_in_vpc_ok = true

  ssh_ingress_cidrs = var.ssh_ingress_cidrs
  key_name          = var.key_name

  # THE ONLY THING ON THIS MACHINE THAT CANNOT BE REBUILT is server.identity_dir:
  # the deployment identity, the node-wire CA and its rotation state. There is no
  # ledger volume on this profile — that is the point — so this bucket is the
  # recovery path for it rather than a nicety.
  create_backup_bucket = true

  # AND THE GRANT THAT LETS THE HOST BUILD ITS OWN DSN. The password is never a
  # Terraform value: the module hands over the ARN of AWS's managed credential and
  # the statement that reads it, and the connection string is assembled on the
  # machine.
  #
  # DERIVED FROM THIS EXAMPLE'S OWN INPUTS, NOT FROM THE MODULE'S OUTPUT, and
  # that is forced rather than stylistic. `count` cannot depend on a value known
  # only at apply, and for a CREATED database `secret_read_policy_json` contains
  # the RDS-managed secret's ARN — so the obvious
  # `module.ledger.secret_read_policy_json != ""` is unknown at plan and the
  # example could not plan at all, defeating the explicit plan-known bool the
  # child introduced for exactly this reason. These two variables are known: a
  # created ledger always has a master credential to read, and an adopted one has
  # something to read only if a DSN secret was named.
  create_state_secret_policy = var.ledger_endpoint == "" || var.ledger_dsn_secret_arn != ""
  state_secret_policy_json   = module.ledger.secret_read_policy_json

  tags = { Environment = "ci" }
}

# THE COMPUTE: AWS-MANAGED, ON DEMAND, BILLED PER BUILD MINUTE.
#
# Deliberately NOT VPC-connected. A VPC-connected CodeBuild project needs a
# reserved fleet (fleet-codebuild refuses the pairing, because a VPC-connected
# project's service role needs unscopeable ec2:*NetworkInterface permissions and
# that role's credentials are reachable from inside somebody's build), a NAT
# gateway for egress, and a standing per-instance charge. None of that is
# necessary to run a job, so the example does not spend it — see
# examples/codebuild for the fleet shape, including macOS.
module "compute" {
  source = "../../modules/fleet-codebuild"

  name             = "billet-linux"
  environment_type = "LINUX_CONTAINER"
  compute_type     = "BUILD_GENERAL1_MEDIUM"

  # A per-deployment key for the staged runner registrations. Without one they are
  # encrypted under the account's aws/ssm key, which authorizes any principal in
  # the account that can reach Parameter Store.
  enable_kms = true

  build_timeout_minutes  = 360
  queued_timeout_minutes = 60

  tags = { Environment = "ci" }
}

# ─────────────────────────────────────────────────────────────────────────────
# WHAT THE CONFIGURATION LAYER NEEDS
# ─────────────────────────────────────────────────────────────────────────────

output "server_config" {
  description = <<-EOT
    The `server:` block for billet.yaml.

    NOTE `identity_dir` RATHER THAN `state_dir`, and that they are mutually
    exclusive: a config carrying both is refused at load. Only the ledger moved to
    PostgreSQL — the deployment identity, the node-wire CA and its rotation state,
    the process lock and the maintenance fence are still files, in this directory,
    on this machine.
  EOT
  value = {
    identity_dir = "/var/lib/billet/server"
    listen       = "0.0.0.0:7717"
    state = {
      backend = "postgres"
      postgres = {
        # NAMED, NOT WRITTEN. billet reads the connection string from this
        # environment variable because a DSN holds a password, and a secret in a
        # config file ends up in a backup and eventually a support thread.
        dsn_env = "BILLET_STATE_DSN"
      }
    }
  }
}

output "state_dsn_template" {
  description = <<-EOT
    The DSN to put in that variable, with the password left as a placeholder.

    THE PASSWORD IS DELIBERATELY NOT HERE. Rendering it would mean Terraform
    knowing it, which means it is in the state file — the single thing billet's
    dsn_env indirection exists to prevent. Substitute it on the controller from
    `state_master_secret_arn` below, which the instance profile can already read:

      read -r user pass < <(aws secretsmanager get-secret-value \
        --secret-id "$ARN" --query SecretString --output text \
        | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["username"], d["password"])')

    The junioryono.billet.host role writes that into /etc/billet/server.env from
    `billet_server_environment` and puts EnvironmentFile= in the unit it renders —
    never a systemd drop-in, because the transactional host upgrade refuses
    effective drop-ins it cannot replace and recover.
  EOT
  value       = module.ledger.dsn_template
}

output "state_master_secret_arn" {
  description = "The AWS-managed credential for a created ledger, or empty for an adopted endpoint. The controller's role is granted read on it above."
  value       = module.ledger.master_secret_arn
}

output "ledger_connection" {
  description = "Where the ledger is, and whether billet made it. `adopted` says which side is real, so you need not infer it from which other outputs are empty."
  value       = module.ledger.connection
}

output "node_wire_address" {
  description = "What every node dials — server.listen on the controller, node.server_addr on each node. billet requires host:port."
  value       = module.controller.node_wire_address
}

output "backup" {
  description = "The `backup.s3` block. On this profile the archive is the identity directory, the node-wire CA and the GitHub App private key: `billet local backup` REFUSES the ledger, because a consistent copy of a PostgreSQL database is pg_dump or a provider snapshot and a half-measure would produce an archive that looks like a backup and is not."
  value = {
    bucket = module.controller.backup_bucket
    prefix = module.controller.backup_prefix
    region = var.region
  }
}

output "node_config" {
  description = "node.codebuild for the orchestrator. Run `billet node` on the controller or on any machine that can reach both AWS and the node wire; the compute appears in the region, so this host is an orchestrator rather than a place work runs."
  value = {
    region             = var.region
    project            = module.compute.project_name
    environment_type   = module.compute.environment_type
    jit_parameter_path = module.compute.parameter_path
    jit_kms_key_id     = module.compute.kms_key_arn
    log_group          = module.compute.log_group_name

    # NO DEFAULT ON THIS KEY, and its absence is a refusal. Every job here
    # inherits CodeBuild's 36-hour build ceiling and 8-hour queued ceiling, which
    # billet cannot lift; setting it is how an operator says they have read that.
    accept_external_build_ceiling = true

    build_timeout_minutes  = 360
    queued_timeout_minutes = 60
    privileged_mode        = true

    # THE PRICE IS YOURS TO AUDIT. billet ships no table of compute types, so a
    # shape it may buy is declared along with what it holds. Replace the rate with
    # the current published one.
    compute_types = [{
      type               = module.compute.compute_type
      vcpu               = 4
      memory             = "7GiB"
      price_usd_per_hour = 0.01
    }]
  }
}

output "node_instance_profile" {
  description = "The instance profile the machine running `billet node` needs. It is how billet reads AWS credentials from IMDS, which is why node.codebuild has no credential fields: a long-lived access key in a unit file is a credential that never expires, on a host whose whole job is launching compute."
  value       = module.compute.node_instance_profile
}
