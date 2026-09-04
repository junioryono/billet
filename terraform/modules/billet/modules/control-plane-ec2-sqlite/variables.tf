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
  description = "The subnet the control-plane instance launches in."
  type        = string
}

variable "availability_zone" {
  description = "The subnet's availability zone — the ledger EBS volume must be created in it, because a volume cannot attach across zones."
  type        = string
}

variable "vpc_cidr" {
  description = "The VPC's CIDR, the default source for node-wire ingress when node_ingress_cidrs is empty."
  type        = string
}

variable "subnet_in_vpc_ok" {
  description = "The caller's explicit assertion that subnet_id belongs to vpc_id. NO default, deliberately: this child cannot look the subnet up itself (a created subnet's id is unknown at plan, so a data lookup would defer the read and cascade unknowns through the instance), so the claim must be conscious — the opinionated root resolves it from its adopted-subnet lookup; a direct consumer states their own truth. False fails the plan, because a launch cannot mix a subnet and security groups from different VPCs."
  type        = bool
}

# THE CONTROLLER'S ADDRESS IS DECLARED HERE OR NOWHERE. It is repeated in at
# least four places outside this module -- server.listen (also the certificate
# SAN of a concrete listener), every node's node.server_addr, the Ansible
# ansible_host, and whatever routes a node's path to it -- and none of them
# decides it. Left empty, AWS hands the ENI an address at creation, an instance
# replacement changes it silently, and the first symptom is a node timeout that
# names nothing.
variable "private_ip" {
  description = "The private IPv4 address the control-plane instance takes in subnet_id. Empty leaves the choice to AWS, which is what every deployment before this input did. Declaring the address an APPLIED instance already holds plans no change (the state carries private_ip); any other value REPLACES the instance, because the address is fixed at launch -- a drain first, as the classification says. Checked against subnet_cidr when the caller supplies it."
  type        = string
  default     = ""

  validation {
    condition     = var.private_ip == "" || can(cidrnetmask("${var.private_ip}/32"))
    error_message = "private_ip must be an IPv4 address (no prefix length), or empty to let AWS choose."
  }
}

variable "subnet_cidr" {
  description = "The CIDR of subnet_id, when the caller knows it, so private_ip can be proved to lie inside the subnet and outside the five addresses AWS reserves in every subnet (the first four and the last). Empty skips that check. A separate input for the reason subnet_in_vpc_ok is: this child cannot look the subnet up itself without deferring the read and cascading unknowns through the instance, so the fact is supplied -- the opinionated root resolves it from whichever side is real."
  type        = string
  default     = ""

  validation {
    condition     = var.subnet_cidr == "" || can(cidrnetmask(var.subnet_cidr))
    error_message = "subnet_cidr must be an IPv4 CIDR (address/prefix), or empty to skip the containment check."
  }
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

variable "volume_gib" {
  description = "The dedicated ledger volume's size."
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

# CLOSED BY DEFAULT, AND THAT IS THE FEATURE. Enrollment is the one surface that
# admits a caller with nothing to present, and nothing a running fleet does goes
# through it -- so the port belongs open only while an operator is adding a
# machine. An empty list creates no rule at all.
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
  description = "Create this child's own minimal instance profile (a bare EC2 trust role with no policies). An EXPLICIT bool rather than inferred from instance_profile_name, because count cannot depend on a value known only at apply — a caller passing a computed profile name could otherwise never plan. Set false and supply instance_profile_name to attach an existing profile (the opinionated root passes fleet-ec2's)."
  type        = bool
  default     = true
}

variable "instance_profile_name" {
  description = "The existing instance profile to attach when create_instance_profile is false. MIGRATION NOTE: flipping an applied standalone child to false destroys the own profile while the instance updates to the new one, and AWS refuses to delete a profile still associated with a running instance — apply in two steps (first attach the new profile, then remove the old resources) or `terraform state rm` the old identity deliberately."
  type        = string
  default     = ""

  validation {
    # EXACTLY one mode: a name supplied while create_instance_profile is true
    # would be silently ignored — the caller believes their profile (and its
    # policies) is attached while the bare own identity is, and billet cannot
    # operate against AWS.
    condition     = var.create_instance_profile == (var.instance_profile_name == "")
    error_message = "set exactly one identity source: create_instance_profile = true with no instance_profile_name, or false with the profile to attach — a name alongside the bool would be silently ignored."
  }
}

variable "create_backup_bucket" {
  description = "Create the S3 bucket billet copies its deployment archives to. A backup on the disk it protects is not one (ADR-001), and the archive holds the ledger, the deployment identity, the GitHub App private key and the node-wire authority as one unit. Set false and supply backup_bucket to adopt an existing bucket, or leave both unset for no off-site copy at all — in which case the archive directory is the seam your own tooling picks up."
  type        = bool
  default     = false
}

variable "backup_bucket" {
  description = "An existing bucket to grant the controller access to, when create_backup_bucket is false. This module does not configure a bucket it does not own: versioning, encryption and the lifecycle rule are the adopting operator's to set."
  type        = string
  default     = ""

  validation {
    # EXACTLY ONE SOURCE. A name supplied alongside create_backup_bucket would
    # be silently ignored, and the controller would be granted access to a
    # bucket nobody is uploading to.
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
