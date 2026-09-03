variable "name" {
  description = "Prefix for every named resource this module creates."
  type        = string

  validation {
    # THE ROOT'S EXACT RULE: this child is an exported entry point of its own,
    # and the name lands in IAM, SQS and S3 names.
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.name))
    error_message = "name must be 2-31 lowercase letters, digits or hyphens starting with a letter."
  }
}

variable "vpc_id" {
  description = "The VPC the runner security group lives in. This child never creates a network; the opinionated root (or the operator) supplies one."
  type        = string
}

variable "enable_cache" {
  description = "Create the S3 cache bucket and grant the node role the cache statements."
  type        = bool
  default     = true
}

variable "cache_bucket" {
  description = "Cache bucket name; empty derives <name>-cache-<account>. Must be dot-free (it is used as a TLS host)."
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
    # THE ROOT'S EXACT RULE, duplicated on purpose: this child is an exported
    # entry point of its own, and a rule enforced at only one of the two is a
    # second entry point that does not enforce it. Mirrors billet's own
    # node.ebs_s3.prefix rules; the charset also blocks what would widen or
    # break the IAM grant it is spliced into.
    condition = (
      can(regex("^[a-z0-9._-]([a-z0-9._/-]*[a-z0-9._-])?$", var.cache_prefix)) &&
      !strcontains(var.cache_prefix, "//") &&
      !can(regex("(^|/)[.]{1,2}(/|$)", var.cache_prefix))
    )
    error_message = "cache_prefix must be a relative object prefix of non-empty segments (lowercase letters, digits, dot, hyphen, underscore, slash), no segment being '.' or '..', and no leading or trailing slash — the same rule billet applies to node.ebs_s3.prefix."
  }
}

variable "enable_kms" {
  description = "Encrypt the cache's EBS volumes with a customer-managed KMS key (created here) instead of the account's EBS default key. Requires enable_cache."
  type        = bool
  default     = false

  validation {
    condition     = !var.enable_kms || var.enable_cache
    error_message = "enable_kms requires enable_cache: the KMS key encrypts the cache's EBS volumes, which only exist when the cache does."
  }
}

variable "enable_spot" {
  description = "Create the spot interruption queue, the tag-scoped router, and the node role's queue-scoped grant."
  type        = bool
  default     = false
}

variable "iam_policy_json" {
  description = "Override the node role's policy document entirely (the output of `billet init iam`). Empty renders the committed generator output for the enabled features."
  type        = string
  default     = ""
}

variable "job_instance_profile_role_arn" {
  description = "The exact IAM role ARN trusted JOB instances receive (node.ec2.instance_profile's role); grants the node role iam:PassRole on it alone. Empty grants no PassRole."
  type        = string
  default     = ""

  validation {
    condition     = var.job_instance_profile_role_arn == "" || can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/[a-zA-Z0-9+=,.@_/-]+$", var.job_instance_profile_role_arn))
    error_message = "job_instance_profile_role_arn must be an IAM role ARN (arn:<partition>:iam::<account>:role/<name>), the exact role PassRole is scoped to."
  }
}

variable "tags" {
  description = "Tags merged onto every resource."
  type        = map(string)
  default     = {}
}
