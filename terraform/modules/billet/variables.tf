variable "name" {
  description = "A name prefix for the resources this module creates."
  type        = string
  default     = "billet"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.name))
    error_message = "name must be 2-31 lowercase letters, digits or hyphens starting with a letter."
  }
}

# ADOPT-OR-CREATE NETWORKING. Leave vpc_id/subnet_id empty to have the module
# create a minimal VPC and subnet; set them to place billet in an existing network.
variable "vpc_id" {
  description = "An existing VPC to place billet in. Empty creates one. Adopt-or-create is all-or-nothing: set vpc_id and subnet_id together, or neither (validated on subnet_id)."
  type        = string
  default     = ""
}

variable "subnet_id" {
  description = "An existing subnet the control plane and runners launch in. Empty creates one in the module's VPC. Its route to GitHub is yours to arrange (a NAT gateway for a private subnet)."
  type        = string
  default     = ""

  validation {
    # Both-or-neither: adopt an existing vpc_id AND subnet_id together, or create
    # both. A subnet without its vpc can't use the module's VPC/security groups; a
    # vpc without a subnet would have the module create one with its own CIDR inside
    # a foreign VPC, colliding or riding an unmanaged route table.
    condition     = (var.subnet_id == "") == (var.vpc_id == "")
    error_message = "set vpc_id and subnet_id together (adopt an existing network) or leave both empty (let the module create one) — not one without the other."
  }
}

variable "vpc_cidr" {
  description = "CIDR for the VPC the module creates (ignored when vpc_id is set)."
  type        = string
  default     = "10.60.0.0/16"
}

variable "subnet_cidr" {
  description = "CIDR for the subnet the module creates (ignored when subnet_id is set)."
  type        = string
  default     = "10.60.0.0/20"
}

variable "subnet_availability_zone" {
  description = "Availability zone for the subnet the module creates (ignored when subnet_id is set). Empty picks the first zone that actually OFFERS control_plane_instance_type, because a zone reporting available need not sell every shape. The ledger volume is zone-bound and carries prevent_destroy, so this takes effect at CREATION only: an applied subnet keeps its zone and changing this does nothing, because moving zones means replacing that volume — a deliberate migration rather than an edit."
  type        = string
  default     = ""
}

variable "control_plane_private_ip" {
  description = "The controller's private IPv4 address, DECLARED rather than left to AWS. It is repeated in server.listen (the certificate SAN of a concrete listener), every node's node.server_addr, the inventory's ansible_host and whatever routes a node's path here, and none of those decides it — so left empty an instance replacement changes it silently. Declaring the address an applied instance already holds plans no change; any other value replaces the instance (a draining change). Checked at plan against the subnet's CIDR, created or adopted."
  type        = string
  default     = ""

  # THE CHILD'S EXACT RULE, canonical rather than parseable: a leading-zero
  # octet parses, passes containment, and misses the reserved-address check.
  validation {
    condition     = var.control_plane_private_ip == "" || try(cidrhost("${var.control_plane_private_ip}/32", 0), "") == var.control_plane_private_ip
    error_message = "control_plane_private_ip must be a canonical IPv4 address (no prefix length, no leading zeros), or empty to let AWS choose."
  }
}

variable "control_plane_instance_type" {
  description = "The control-plane instance shape. ADR-001 sizes this small: it long-polls GitHub and owns a SQLite ledger, not a fleet's worth of compute."
  type        = string
  default     = "t3.small"
}

variable "control_plane_ami" {
  description = "The AMI for the control-plane instance (an Ubuntu image the junioryono.billet.host Ansible role supports). Empty resolves the latest Ubuntu 24.04 LTS for the architecture."
  type        = string
  default     = ""
}

variable "control_plane_architecture" {
  description = "CPU architecture of the control-plane AMI resolved when control_plane_ami is empty: x86_64 or arm64. Must match control_plane_instance_type."
  type        = string
  default     = "x86_64"

  validation {
    condition     = contains(["x86_64", "arm64"], var.control_plane_architecture)
    error_message = "control_plane_architecture must be x86_64 or arm64."
  }
}

variable "control_plane_volume_gib" {
  description = "Size (GiB) of the dedicated gp3 LEDGER volume that holds the SQLite state (the root is OS-only). ADR-001 keeps SQLite on a retained EBS volume that survives the instance."
  type        = number
  default     = 20
}

variable "control_plane_listen_port" {
  description = "The port billet server listens on for the node wire. Nodes dial it over mTLS; a non-loopback listen is required for a multi-host deployment."
  type        = number
  default     = 7717

  validation {
    condition     = var.control_plane_listen_port >= 1 && var.control_plane_listen_port <= 65535
    error_message = "control_plane_listen_port must be a TCP port in 1-65535."
  }
}

variable "cache_listen_port" {
  description = "The port the EC2 node.cache HTTPS listener serves guests on. When enable_cache is set, runners are allowed to reach it on the control plane; it must match the port in node.cache.listen/guest_endpoint, and must differ from control_plane_listen_port so the node wire is not exposed to guests."
  type        = number
  default     = 9443

  validation {
    condition     = var.cache_listen_port >= 1 && var.cache_listen_port <= 65535
    error_message = "cache_listen_port must be a TCP port in 1-65535."
  }

  validation {
    condition     = var.cache_listen_port != var.control_plane_listen_port
    error_message = "cache_listen_port must differ from control_plane_listen_port — sharing one port would expose the node wire to guests (via the cache rule) and the two listeners could not both bind it."
  }
}

variable "node_ingress_cidrs" {
  description = "CIDRs allowed to reach the control plane's node-wire port. Defaults to the VPC's PRIMARY CIDR, so nodes in that range can register; set this explicitly for nodes in a secondary VPC CIDR or another VPC."
  type        = list(string)
  default     = []
}

variable "control_plane_bootstrap_port" {
  description = "The port billet server serves enrollment on (server.bootstrap_listen). It carries the two routes a machine with no certificate needs, so it is a separate listener from the node wire and must be a separate port."
  type        = number
  default     = 7718

  validation {
    condition     = var.control_plane_bootstrap_port >= 1 && var.control_plane_bootstrap_port <= 65535
    error_message = "control_plane_bootstrap_port must be a TCP port in 1-65535."
  }

  validation {
    condition     = var.control_plane_bootstrap_port != var.control_plane_listen_port
    error_message = "control_plane_bootstrap_port must differ from control_plane_listen_port: the node wire demands a client certificate in the handshake and enrollment cannot, so one port cannot serve both."
  }

  validation {
    condition     = var.control_plane_bootstrap_port != var.cache_listen_port
    error_message = "control_plane_bootstrap_port must differ from cache_listen_port; the two listeners could not both bind it."
  }
}

variable "bootstrap_ingress_cidrs" {
  description = "CIDRs allowed to reach the control plane's ENROLLMENT port. EMPTY OPENS NOTHING, which is the intended steady state: a node that has enrolled never dials it again, and it is the one surface that admits a caller with no certificate. Open it while adding a machine, then close it."
  type        = list(string)
  default     = []
}

variable "ssh_ingress_cidrs" {
  description = "CIDRs allowed SSH to the control plane, for the Ansible host role to converge it. Empty opens no SSH (use SSM instead)."
  type        = list(string)
  default     = []
}

variable "key_name" {
  description = "An EC2 key pair for SSH to the control plane. Empty attaches none."
  type        = string
  default     = ""
}

# THE CACHE STORE. EBS carries encrypted block generations and S3 the fenced
# pointer. Created only when enable_cache is true.
variable "enable_cache" {
  description = "Create the S3 cache bucket (and optional KMS key) an EBS-S3 site uses."
  type        = bool
  default     = true
}

variable "cache_bucket" {
  description = "Name of the S3 cache bucket. Empty derives one from name and account. Must be dot-free (it is used as a TLS host)."
  type        = string
  default     = ""

  validation {
    condition     = var.cache_bucket == "" || can(regex("^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$", var.cache_bucket))
    error_message = "cache_bucket must be a dot-free S3 bucket name (lowercase letters, digits, hyphens); it is spliced into an IAM policy and used as a TLS host."
  }
}

variable "cache_prefix" {
  description = "Object prefix isolating this deployment inside the cache bucket."
  type        = string
  default     = "billet-cache"

  validation {
    # Mirrors billet's own node.ebs_s3.prefix rules: a relative object prefix of
    # non-empty segments, none of them "." or "..", no leading/trailing slash. The
    # charset also blocks the characters that would widen or break the IAM grant it
    # is spliced into (a wildcard, an IAM policy variable $${...}, or a quote).
    condition = (
      can(regex("^[a-z0-9._-]([a-z0-9._/-]*[a-z0-9._-])?$", var.cache_prefix)) &&
      !strcontains(var.cache_prefix, "//") &&
      !can(regex("(^|/)[.]{1,2}(/|$)", var.cache_prefix))
    )
    error_message = "cache_prefix must be a relative object prefix of non-empty segments (lowercase letters, digits, dot, hyphen, underscore, slash), no segment being '.' or '..', and no leading or trailing slash — the same rule billet applies to node.ebs_s3.prefix."
  }
}

variable "enable_kms" {
  description = "Create a customer-managed KMS key for the cache's EBS volumes. Off uses the account's default EBS key. Requires enable_cache."
  type        = bool
  default     = false

  validation {
    condition     = !var.enable_kms || var.enable_cache
    error_message = "enable_kms requires enable_cache: the KMS key encrypts the cache's EBS volumes, which only exist when the cache does."
  }
}

# SPOT INTERRUPTIONS. enable_spot creates the queue a spot node consumes and the
# EventBridge-to-Lambda router that a tag-scoped delivery needs, since EventBridge
# cannot match on the instance tag that says which node owns the warning.
variable "enable_spot" {
  description = "Create the SQS interruption queue a spot deployment consumes, and the tag-scoped router that fills it."
  type        = bool
  default     = false
}

# THE ROOT'S EXACT RULE, duplicated on purpose: this root and fleet-ec2 are two
# entry points, and a rule enforced at only one of them is one that is not enforced.
variable "spot_router_alarm_actions" {
  description = "ARNs notified when the spot interruption router fails an invocation — a warning it could not place and re-raised for Lambda to retry. Usually an SNS topic; this module creates none, so an operator supplies their own. Empty leaves the alarm with no action: its state is still visible in the console and to DescribeAlarms, but nothing is sent."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for action in var.spot_router_alarm_actions : can(regex("^arn:", action))])
    error_message = "every spot_router_alarm_actions entry must be an ARN (an SNS topic's, usually)."
  }
}

# THE OFF-SITE COPY. Passed through to the control-plane child, which owns the
# bucket and the grant; the root's part is to hand the grant fleet-ec2's role,
# because the co-located controller runs with THAT profile and a grant on the
# child's own identity would protect nothing here. The validations restate the
# child's, because this root is an exported entry point of its own.
variable "create_backup_bucket" {
  description = "Create the S3 bucket billet copies its deployment archives to (versioned, encrypted, public access blocked, prevent_destroy). A backup on the disk it protects is not one (ADR-001), and the archive holds the ledger, the deployment identity, the GitHub App private key and the node-wire authority as one unit. Set false and pass backup_bucket to adopt an existing bucket, or leave both unset for no off-site copy at all."
  type        = bool
  default     = false
}

variable "backup_bucket" {
  description = "An existing bucket to grant the controller access to, when create_backup_bucket is false. The module configures nothing on a bucket it does not own."
  type        = string
  default     = ""

  validation {
    condition     = !(var.create_backup_bucket && var.backup_bucket != "")
    error_message = "set create_backup_bucket = true OR pass backup_bucket, not both — a name alongside the bool would be silently ignored."
  }
}

variable "backup_prefix" {
  description = "The object prefix archives land under; billet writes <prefix>/<deployment-id>/<taken-at>/. The IAM grant is scoped to it literally, so set backup.s3.prefix in billet.yaml to the same value."
  type        = string
  default     = "billet-backups"

  validation {
    condition     = !can(regex("[*?]", var.backup_prefix)) && !startswith(var.backup_prefix, "/") && var.backup_prefix != ""
    error_message = "backup_prefix must be a non-empty literal prefix with no wildcard and no leading slash: it lands in an IAM Resource ARN."
  }
}

variable "backup_kms_key_arn" {
  description = "A customer-managed key to encrypt the backup bucket with; empty uses SSE-S3. A full key ARN, because the grant is resource-scoped to it."
  type        = string
  default     = ""

  validation {
    condition     = var.backup_kms_key_arn == "" || can(regex("^arn:[^:]+:kms:[^:]+:[0-9]{12}:key/", var.backup_kms_key_arn))
    error_message = "backup_kms_key_arn must be a full KMS key ARN (arn:PARTITION:kms:REGION:ACCOUNT:key/ID)."
  }
}

variable "backup_retention_days" {
  description = "How long a NONCURRENT archive version is kept; the current object of every archive is kept forever, because billet has no delete. Zero creates no lifecycle rule."
  type        = number
  default     = 90

  validation {
    condition     = var.backup_retention_days >= 0
    error_message = "backup_retention_days cannot be negative."
  }
}

variable "iam_policy_json" {
  description = "Override the node role's IAM policy. Empty uses the module's committed rendering of billet's own generator (billet init iam), substituted for this deployment's resources. Paste `billet init iam` output to scope it to an exact config."
  type        = string
  default     = ""
}

variable "job_instance_profile_role_arn" {
  description = "A role ARN the launched job instances receive (node.ec2.instance_profile). When set, the node role is granted iam:PassRole for exactly it. Empty grants no PassRole."
  type        = string
  default     = ""

  validation {
    condition     = var.job_instance_profile_role_arn == "" || can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/[a-zA-Z0-9+=,.@_/-]+$", var.job_instance_profile_role_arn))
    error_message = "job_instance_profile_role_arn must be an IAM role ARN (arn:<partition>:iam::<account>:role/<name>), the exact role PassRole is scoped to."
  }
}

variable "tags" {
  description = "Tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
