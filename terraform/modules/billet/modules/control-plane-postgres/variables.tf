variable "name" {
  description = "Prefix for every named resource this module creates."
  type        = string

  validation {
    # THE ROOT'S EXACT RULE: this child is an exported entry point of its own.
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.name))
    error_message = "name must be 2-31 lowercase letters, digits or hyphens starting with a letter."
  }
}

variable "vpc_id" {
  description = "The VPC the control plane's security group lives in. This child never creates a network."
  type        = string
}

variable "subnet_id" {
  description = "The subnet the control-plane instance launches in. It must reach the ledger endpoint; nothing here creates a route."
  type        = string
}

variable "vpc_cidr" {
  description = "The VPC's CIDR, the default source for node-wire ingress when node_ingress_cidrs is empty."
  type        = string
}

variable "subnet_in_vpc_ok" {
  description = "The caller's explicit assertion that subnet_id belongs to vpc_id. NO default, deliberately: this child cannot look the subnet up itself (a created subnet's id is unknown at plan, so a data lookup would defer the read and cascade unknowns through the instance), so the claim must be conscious. False fails the plan, because a launch cannot mix a subnet and security groups from different VPCs."
  type        = bool
}

variable "instance_type" {
  description = "The control-plane instance type."
  type        = string
  default     = "t4g.small"
}

variable "ami" {
  description = "The control-plane AMI; empty resolves the latest Canonical Ubuntu 24.04 for the architecture."
  type        = string
  default     = ""
}

variable "architecture" {
  description = "The instance architecture the AMI lookup filters on."
  type        = string
  default     = "arm64"

  validation {
    condition     = contains(["arm64", "x86_64"], var.architecture)
    error_message = "architecture must be arm64 or x86_64."
  }
}

variable "root_volume_gib" {
  description = "The root volume's size. It carries the OS AND server.identity_dir — the deployment identity, the node-wire CA and its rotation state — because this profile deliberately has no second volume: one would pin the instance to an availability zone and undo the replaceability that moving the ledger to PostgreSQL bought. Back the identity directory up (see create_backup_bucket); it is not large."
  type        = number
  default     = 20
}

variable "listen_port" {
  description = "The node-wire port the control plane listens on."
  type        = number
  default     = 7717

  validation {
    condition     = var.listen_port >= 1 && var.listen_port <= 65535
    error_message = "listen_port must be a TCP port in 1-65535."
  }
}

variable "node_ingress_cidrs" {
  description = "CIDRs that may dial the node wire; empty defaults to the VPC's CIDR."
  type        = list(string)
  default     = []
}

variable "bootstrap_port" {
  description = "The port billet server serves enrollment on (server.bootstrap_listen), when it serves it at all."
  type        = number
  default     = 7718

  validation {
    condition     = var.bootstrap_port >= 1 && var.bootstrap_port <= 65535
    error_message = "bootstrap_port must be a TCP port in 1-65535."
  }

  validation {
    condition     = var.bootstrap_port != var.listen_port
    error_message = "bootstrap_port must differ from listen_port: the node wire demands a client certificate in the handshake and enrollment cannot, so one port cannot serve both."
  }
}

variable "bootstrap_ingress_cidrs" {
  description = "CIDRs that may reach the enrollment port. EMPTY OPENS NOTHING, which is the intended steady state: enrollment is a human-paced operation, and a node that has already enrolled never uses this port. Open it while adding a machine and close it again."
  type        = list(string)
  default     = []
}

variable "ssh_ingress_cidrs" {
  description = "CIDRs that may SSH to the control plane (for the Ansible role); empty opens nothing."
  type        = list(string)
  default     = []
}

variable "key_name" {
  description = "EC2 key pair for SSH; empty attaches none."
  type        = string
  default     = ""
}

variable "create_instance_profile" {
  description = "Create this child's own minimal instance profile (a bare EC2 trust role with no policies). An EXPLICIT bool rather than inferred from instance_profile_name, because count cannot depend on a value known only at apply. Set false and supply instance_profile_name to attach an existing profile — but note that the backup grant and the state-secret grant below are attached to THIS child's role, so they are created only when it owns one."
  type        = bool
  default     = true
}

variable "instance_profile_name" {
  description = "The existing instance profile to attach when create_instance_profile is false."
  type        = string
  default     = ""

  validation {
    # EXACTLY one mode: a name supplied while create_instance_profile is true
    # would be silently ignored — the caller believes their profile (and its
    # policies) is attached while the bare own identity is.
    condition     = var.create_instance_profile == (var.instance_profile_name == "")
    error_message = "set exactly one identity source: create_instance_profile = true with no instance_profile_name, or false with the profile to attach — a name alongside the bool would be silently ignored."
  }
}

variable "create_state_secret_policy" {
  description = "Attach state_secret_policy_json to this child's role, so the host can read the ledger credential and assemble BILLET_STATE_DSN itself. An EXPLICIT bool for the reason create_instance_profile is one: the document names a secret ARN that does not exist until apply, so count cannot be derived from whether the string is empty. Leave it false when you deliver the DSN some other way — the module has no opinion about which, only that the password is not in a plan."
  type        = bool
  default     = false
}

variable "state_secret_policy_json" {
  description = "The IAM policy granting read on the ledger's credential — state-rds-postgres emits exactly this as secret_read_policy_json. Ignored unless create_state_secret_policy is true."
  type        = string
  default     = ""

  validation {
    condition     = !var.create_state_secret_policy || var.state_secret_policy_json != ""
    error_message = "create_state_secret_policy = true needs state_secret_policy_json; state-rds-postgres emits it as secret_read_policy_json, and that output is EMPTY for an adopted endpoint with no dsn_secret_arn — there is nothing to grant in that case, so leave the bool false."
  }
}

variable "create_backup_bucket" {
  description = "Create the S3 bucket this deployment's identity copy belongs in: the deployment identity, the node-wire CA and its rotation state, and the GitHub App private key. There is no retained volume beside the instance on this profile, so an off-site copy is the ONLY recovery path for any of it. NOTE that `billet local backup` does not work on a PostgreSQL deployment yet — it refuses the ledger deliberately and has no identity-only archive — so today that copy is one you make (docs/deploying/postgres-and-active-passive.md has the command) and this is where it goes. Set false and supply backup_bucket to adopt an existing one."
  type        = bool
  default     = false
}

variable "backup_bucket" {
  description = "An existing bucket to grant the controller access to, when create_backup_bucket is false. This module does not configure a bucket it does not own: versioning, encryption and the lifecycle rule are the adopting operator's to set."
  type        = string
  default     = ""

  validation {
    condition     = !(var.create_backup_bucket && var.backup_bucket != "")
    error_message = "set create_backup_bucket = true OR pass backup_bucket, not both — a name alongside the bool would be silently ignored."
  }
}

variable "backup_prefix" {
  description = "The object prefix archives land under; billet writes <prefix>/<deployment-id>/<taken-at>/. It is what the IAM grant is scoped to, so it must be literal — a wildcard would widen the grant to every sibling prefix, and every sibling prefix is another deployment's App key. Set backup.s3.prefix in billet.yaml to the same value."
  type        = string
  default     = "billet-backups"

  validation {
    condition     = !can(regex("[*?]", var.backup_prefix)) && !startswith(var.backup_prefix, "/") && var.backup_prefix != ""
    error_message = "backup_prefix must be a non-empty literal prefix with no wildcard and no leading slash: it lands in an IAM Resource ARN."
  }
}

variable "backup_kms_key_arn" {
  description = "A customer-managed key to encrypt the backup bucket with. Empty uses SSE-S3 (AES256). A full key ARN, not an alias or a bare id: IAM resource scoping needs one, and the controller's grant is conditioned on kms:ViaService for S3 so the key cannot be called directly."
  type        = string
  default     = ""

  validation {
    condition     = var.backup_kms_key_arn == "" || can(regex("^arn:[^:]+:kms:[^:]+:[0-9]{12}:key/", var.backup_kms_key_arn))
    error_message = "backup_kms_key_arn must be a full KMS key ARN (arn:PARTITION:kms:REGION:ACCOUNT:key/ID)."
  }
}

variable "backup_retention_days" {
  description = "How long a NONCURRENT archive version is kept. The CURRENT object of every archive is kept forever: billet has no delete, and a lifecycle rule that expired current objects would remove backups on a timer. Zero creates no lifecycle rule at all."
  type        = number
  default     = 90

  validation {
    condition     = var.backup_retention_days >= 0
    error_message = "backup_retention_days cannot be negative."
  }
}

variable "tags" {
  description = "Tags merged onto every resource."
  type        = map(string)
  default     = {}
}
