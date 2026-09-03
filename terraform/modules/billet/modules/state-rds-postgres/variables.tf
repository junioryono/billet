variable "name" {
  description = "Prefix for every named resource this module creates."
  type        = string

  validation {
    # THE ROOT'S EXACT RULE, duplicated on purpose: this child is an exported
    # entry point of its own, and a rule enforced at only one of two entry
    # points is a second entry point that does not enforce it.
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.name))
    error_message = "name must be 2-31 lowercase letters, digits or hyphens starting with a letter."
  }
}

variable "endpoint" {
  description = "An EXISTING PostgreSQL endpoint to adopt — an RDS instance, an Aurora cluster writer, or a database you operate yourself. Empty creates one. Adoption is a first-class shape rather than an afterthought: with this set the module creates nothing at all, and the credential story stays yours."
  type        = string
  default     = ""
}

variable "vpc_id" {
  description = "The VPC the created database's security group lives in. Ignored when endpoint is set. This child never creates a network."
  type        = string
  default     = ""

  validation {
    condition     = var.endpoint != "" || var.vpc_id != ""
    error_message = "vpc_id is required when this module creates the database; it is where the ledger's own security group lives."
  }
}

variable "subnet_ids" {
  description = "At least two subnets, IN DIFFERENT AVAILABILITY ZONES, for the created database's subnet group — RDS requires two zones even for a single-AZ instance. Ignored when endpoint is set."
  type        = list(string)
  default     = []

  validation {
    # WHAT THIS CHECKS IS THE COUNT, AND THE MESSAGE SAYS SO. The rule that
    # matters is two ZONES, and a module cannot see a subnet's zone: a data
    # lookup on a subnet the same apply is creating is unknown at plan, so
    # deferring the read would cascade unknowns through the instance. Writing
    # the error as though the zones had been checked would be a refusal claiming
    # to have established something it never asked about — so it names the half
    # that was checked and the half that is yours.
    condition     = var.endpoint != "" || length(var.subnet_ids) >= 2
    error_message = "subnet_ids needs at least two subnets, and they must be in different availability zones. Only the COUNT is checked here — this module cannot resolve a subnet's zone at plan time — so two subnets in one zone pass this and are refused by RDS when the subnet group is created."
  }
}

variable "client_security_groups" {
  description = "The security groups permitted to reach the created database on 5432 — the control plane's, and nothing else. A DATABASE HOLDING A CAPACITY LEDGER IS NOT A SHARED SERVICE: every row in it authorises compute, so the ingress is by group identity rather than by CIDR, which is a promise about addressing rather than about who is asking. A MAP, keyed by a name you choose (`{ controller = module.controller.security_group_id }`): the key becomes the rule's Terraform address and its description, so adding a second client later does not renumber and recreate the first one's rule — and a map's keys are known at plan even when a created group's id is not, which a set of ids would not be."
  type        = map(string)
  default     = {}

  validation {
    condition     = var.endpoint != "" || length(var.client_security_groups) > 0
    error_message = "client_security_groups must name at least one group when this module creates the database, or nothing can reach it and the control plane cannot start."
  }
}

variable "port" {
  description = "The port an ADOPTED endpoint listens on. A created instance reports its own."
  type        = number
  default     = 5432
}

variable "database" {
  description = "The database name. For an adopted endpoint this is the database billet's ledger lives in and must already exist; for a created instance it is the one to create."
  type        = string
  default     = "billet"

  validation {
    condition     = can(regex("^[a-z][a-z0-9_]{0,62}$", var.database))
    error_message = "database must be 1-63 lowercase letters, digits or underscores starting with a letter."
  }
}

variable "username" {
  description = "The master username for a created instance. Its password is generated and held by AWS in Secrets Manager rather than passed through Terraform, so it never reaches a plan, a state file or a shell history."
  type        = string
  default     = "billet"

  validation {
    condition     = can(regex("^[a-z][a-z0-9_]{0,62}$", var.username))
    error_message = "username must be 1-63 lowercase letters, digits or underscores starting with a letter."
  }
}

variable "engine_version" {
  description = "The PostgreSQL major (or major.minor) to create. Minor upgrades are applied by AWS in the maintenance window and are deliberately ignored by the plan, so an ordinary apply does not restart a database the control plane is mid-transaction against."
  type        = string
  default     = "18"
}

variable "instance_class" {
  description = "The created instance class. billet's ledger is small and hot — a capacity record, node registrations and job history — so this is sized like the controller rather than like a data warehouse."
  type        = string
  default     = "db.t4g.micro"
}

variable "allocated_gib" {
  description = "Initial storage for a created instance. RDS's gp3 minimum for PostgreSQL is 20 GiB (measured: describe-orderable-db-instance-options), which is also the default here."
  type        = number
  default     = 20

  validation {
    condition     = var.allocated_gib >= 20
    error_message = "allocated_gib must be at least 20: that is RDS's gp3 minimum for PostgreSQL, and a smaller value is refused by the API after everything else has applied."
  }
}

variable "max_allocated_gib" {
  description = "The autoscaling ceiling for a created instance's storage. Zero disables autoscaling, which means a full disk stops the control plane writing rather than costing a few dollars."
  type        = number
  default     = 100

  validation {
    # A CEILING BELOW THE FLOOR IS REFUSED BY THE API, and refusing it here is
    # the difference between a plan that fails and an apply that fails partway.
    condition     = var.max_allocated_gib == 0 || var.max_allocated_gib >= var.allocated_gib
    error_message = "max_allocated_gib is the autoscaling CEILING, so it cannot be below allocated_gib; use 0 to disable autoscaling entirely."
  }
}

variable "multi_az" {
  description = "Run the created instance across two availability zones. It is NOT high availability for billet — exactly one controller may make scheduling decisions either way (ADR-008) — it is durability for the ledger, and it roughly doubles the database's cost."
  type        = bool
  default     = false
}

variable "backup_retention_days" {
  description = "Automated backup retention for a created instance. ZERO DISABLES BACKUPS ENTIRELY, which is the state an operator discovers on the day they need one — so the minimum here is one, and the default is a week."
  type        = number
  default     = 7

  validation {
    condition     = var.backup_retention_days >= 1
    error_message = "backup_retention_days must be at least 1: zero disables automated backups, and this database is the deployment's capacity record."
  }
}

variable "deletion_protection" {
  description = "Refuse to delete the created instance. ON by default, for the reason prevent_destroy is on the SQLite ledger volume: a `terraform destroy` run to clean something else up must not take the deployment's ledger with it."
  type        = bool
  default     = true
}

variable "kms_key_arn" {
  description = "A customer-managed KMS key for the created instance's storage and its DSN secret. Empty uses the account's default keys; storage is encrypted either way."
  type        = string
  default     = ""
}

variable "performance_insights" {
  description = "Performance Insights on a created instance, at the free seven-day retention. ON by default: the first version of this module had it off, justified by the default burstable class not offering it, and that was measured to be false (`describe-orderable-db-instance-options --db-instance-class db.t4g.micro --engine postgres` reports SupportsPerformanceInsights: true). Set it false for an instance class that genuinely does not offer it."
  type        = bool
  default     = true
}

variable "sslmode" {
  description = "The TLS mode dsn_template asks for. verify-full is the default and is what a connection carrying a capacity ledger across a network should want: it authenticates the server as well as encrypting. Lower it only against an endpoint whose certificate you cannot verify, and know that `require` encrypts without proving who is on the other end."
  type        = string
  default     = "verify-full"

  validation {
    condition     = contains(["require", "verify-ca", "verify-full"], var.sslmode)
    error_message = "sslmode must be require, verify-ca or verify-full: a ledger connection is never unencrypted."
  }
}

variable "ssl_root_cert_path" {
  description = "Where the controller keeps the RDS certificate bundle; dsn_template names it as sslrootcert. WITHOUT IT verify-full CANNOT SUCCEED: RDS server certificates are issued by self-signed per-region 'Amazon RDS ... Root CA' authorities (measured against the published bundle), which are in no operating system trust store — so the connection has nothing to verify against and fails on every stock host. ssl_bundle_url is where the file comes from, and the junioryono.billet.host role installs it from billet_state_ca_bundle_src."
  type        = string
  default     = "/etc/billet/rds-ca-bundle.pem"

  validation {
    condition     = startswith(var.ssl_root_cert_path, "/")
    error_message = "ssl_root_cert_path must be absolute: it lands in a DSN read by a service whose working directory is not yours."
  }
}

variable "dsn_secret_arn" {
  description = "An EXISTING Secrets Manager secret holding a complete BILLET_STATE_DSN. This module never creates one: writing a DSN means knowing the password, and a password Terraform knows is a password in the state file — which is what billet's dsn_env indirection exists to prevent. Supply one you manage and it is included in secret_read_policy_json; leave it empty and a host assembles the DSN from master_secret_arn instead."
  type        = string
  default     = ""

  validation {
    condition     = var.dsn_secret_arn == "" || can(regex("^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:", var.dsn_secret_arn))
    error_message = "dsn_secret_arn must be a full Secrets Manager secret ARN (arn:PARTITION:secretsmanager:REGION:ACCOUNT:secret:NAME-SUFFIX)."
  }
}

variable "tags" {
  description = "Tags merged onto every resource."
  type        = map(string)
  default     = {}
}
