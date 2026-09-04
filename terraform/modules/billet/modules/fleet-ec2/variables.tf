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

# THE AMI BUILDER'S GRANT, OFF BY DEFAULT AND ADDITIVE.
#
# `billet ami build` provisions an instance, images it, boots the image it made
# and reads the verifier's report off the console before stamping the contract
# tag. None of that is in the node's own policy, so without this the command is
# run from a workstation with an operator's own credentials — a second machine
# to keep trustworthy for one step, on a deployment whose controller may be
# reachable only through a tunnel.
#
# A SEPARATE INLINE POLICY rather than a variant of the node rendering, so the
# node's grant is byte-identical whether or not a deployment builds images, and
# so an operator can read in one document exactly what turning this on added. It
# is ADDITIVE: the builder's launches ride the node policy's own RunInstances,
# admitted because the module's rendering is presence-mode and the builder tags
# its instances with the same owner key. Passing a VALUE-scoped iam_policy_json
# instead (`billet init iam --deployment <id>`) needs `--builder` on that command
# too, or the runtime statements will not admit the builder's own tag.
variable "builder" {
  description = "Grant the node role what `billet ami build` needs: ec2:CreateImage on a builder-tagged instance and on the image and snapshots it makes, its own TerminateInstances for cleanup, GetConsoleOutput to read the verifier's report, and the CreateTags that stamps a verified image. Off by default — it widens the identity every job's instance is launched by, and a deployment that builds its AMI elsewhere should not carry it."
  type        = bool
  default     = false

  validation {
    # AN OVERRIDE HAS TO ADMIT THE BUILDER'S OWN TAG, and only whoever generated
    # it can make it do so.
    #
    # iam_policy_json REPLACES the node policy outright, and this builder
    # document is ADDITIVE: it grants CreateImage, the console read, the
    # promotion tag and the create-time tag for CreateImage, and nothing that
    # launches an instance. The launch rides the node policy's own RunInstances,
    # which a PRESENCE-mode rendering admits whatever the owner tag says and a
    # VALUE-mode one does not — `billet ami build` tags its builder
    # billet-ami-build-<image>, never the deployment id. So a value-scoped
    # override generated without --builder denies the very first call while the
    # apply reports the builder grant enabled, which is worse than not having it.
    #
    # A substring is the only test available here and it is the right one:
    # `billet init iam --builder` is what puts that prefix into the conditions
    # that admit the builder's launch and its create-time tag.
    condition     = var.iam_policy_json == "" || !var.builder || can(regex("billet-ami-build-", var.iam_policy_json))
    error_message = "builder = true beside an iam_policy_json override needs that override to admit the builder's own owner tag. iam_policy_json REPLACES the node policy; the builder document is additive and grants nothing that launches an instance; and a value-scoped policy admits only the deployment's exact owner value, while `billet ami build` tags its builder billet-ami-build-<image>. Regenerate the override with `billet init iam --deployment <id> --builder`, which adds the billet-ami-build-* conditions this looks for."
  }
}

variable "builder_payload_bucket" {
  description = "The S3 bucket `billet ami build --payload-bucket` stages its shared installers in, when they no longer fit EC2's 16384-byte user-data limit. Empty grants nothing on S3. The grant is scoped to the object names billet writes (billet-payload-*) at the bucket root, so anything else kept in that bucket is out of reach. Requires builder."
  type        = string
  default     = ""

  validation {
    condition     = var.builder_payload_bucket == "" || var.builder
    error_message = "builder_payload_bucket is only read when builder = true; nothing but `billet ami build` stages objects there, so granting it to a role that does not build would widen it for a command it never runs."
  }

  validation {
    # THE SAME RULE THE STAGER ENFORCES, and NO DOTS, which is the case worth
    # spelling out: a dot is legal in S3 and unusable here, because the
    # virtual-hosted host it produces is not covered by S3's wildcard
    # certificate and the fetch fails TLS verification. Accepting one would
    # apply cleanly, render a policy that looks right, and be refused by
    # `billet ami build` against the bucket it was pointed at. A wildcard would
    # widen the grant to every bucket sharing the prefix, and a slash names a
    # key rather than a bucket, so the grant would match nothing at all.
    condition     = var.builder_payload_bucket == "" || can(regex("^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$", var.builder_payload_bucket))
    error_message = "builder_payload_bucket must be a bucket name billet can sign for: 3 to 63 lowercase letters, digits and hyphens, starting and ending with a letter or digit. No dots (their virtual-hosted host is not covered by S3's wildcard certificate, so the build's fetch fails TLS verification), no wildcard, no slash."
  }
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
