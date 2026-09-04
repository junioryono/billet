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

# SEVERAL SPOT NODES ARE SEVERAL QUEUES, AND ONE INPUT MAKES THEM. billet needs one
# interruption queue per spot node, its basename equal to that node's node.name;
# the queue enable_spot creates serves one node. Each name here creates a queue
# named exactly it, and the node role's consumer grant, the router's forwarding
# grant and the set of names the router is told it serves all widen from this one
# list — so telling the router about a queue, granting it, and granting the node
# are one operation rather than three an operator can half-do. The half-done shape
# is what issue #66 was: a queue granted by hand but never named to the router
# had its warnings DROPPED while the grant propagated, because the router could
# not prove the queue was its own.
variable "spot_node_names" {
  description = "Further spot nodes beside the one enable_spot creates. One interruption queue is created per entry, named exactly it (billet requires the queue basename to equal the node's node.name), and the node grant, the router grant and the router's served set widen to every queue from this one input. Queue names are account-wide per region in SQS. Requires enable_spot."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.spot_node_names) == 0 || var.enable_spot
    error_message = "spot_node_names requires enable_spot: the router and the primary queue it serves only exist with spot enabled, and a queue nothing forwards to receives no warning."
  }

  validation {
    # THE INTERSECTION OF TWO RULES, both of which the name has to satisfy:
    # billet's node name (^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$, config.ValidateNodeName)
    # and SQS's queue name ([A-Za-z0-9_-]{1,80}). A dot is the case worth spelling
    # out: legal for billet, refused by SQS, and refusing it here is a plan
    # failure rather than an apply that dies after the first queue was created.
    condition     = alltrue([for n in var.spot_node_names : can(regex("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$", n))])
    error_message = "every spot_node_names entry must be a name both billet and SQS accept: 1 to 64 letters, digits, hyphens or underscores, starting with a letter or digit, and no dot (billet allows one in a node name; SQS refuses it in a queue name)."
  }

  validation {
    # toset() would collapse a duplicate silently, and two nodes named alike would
    # then share one queue — the shared-consumer shape billet refuses.
    condition     = length(distinct(var.spot_node_names)) == length(var.spot_node_names)
    error_message = "spot_node_names must not repeat a name: two spot nodes cannot share one interruption queue."
  }

  validation {
    condition     = !contains(var.spot_node_names, local.spot_queue_name)
    error_message = "spot_node_names must not name the primary queue enable_spot already creates (<name>-spot-interruptions); that node is served by spot_node_name."
  }

  validation {
    # BOUNDED, because every queue's ARN is repeated in two inline policies and
    # its name in the Lambda's environment, and AWS refuses each of those past a
    # quota Terraform cannot see at plan. A partial apply is the #66 shape again:
    # queues that exist with neither a grant nor a served name. The binding
    # quota is IAM's 10,240 characters for ALL of a role's inline policies
    # combined, which the node role shares between its generated rendering
    # (about 3 KB with sentinels, more with real values, up to 5 KB for an
    # override), the builder grant (1.4 KB), PassRole and the root's backup
    # grant; a worst-case queue ARN with a 64-character name is 113 characters,
    # so 17 queues cost about 2.1 KB and fit with room to spare. Lambda's 4 KB
    # environment would admit about 60 names and is not the limit.
    condition     = length(var.spot_node_names) <= 16
    error_message = "spot_node_names admits at most 16 further spot nodes: every queue's ARN is repeated in the node role's and the router's inline policies, and IAM caps a role's inline policies at 10,240 characters combined. More spot nodes than that are several fleet-ec2 instances."
  }
}

variable "spot_router_alarm_actions" {
  description = "ARNs notified when the spot interruption router fails an invocation — a warning it could not place and re-raised for Lambda to retry. Usually an SNS topic; this module creates none, so an operator supplies their own. Empty leaves the alarm with no action: its state is still visible in the console and to DescribeAlarms, but nothing is sent."
  type        = list(string)
  default     = []

  validation {
    # An action is an ARN or it is silently ignored by CloudWatch, and an alarm
    # whose action goes nowhere is worse than no alarm: it reads as covered.
    condition     = alltrue([for action in var.spot_router_alarm_actions : can(regex("^arn:", action))])
    error_message = "every spot_router_alarm_actions entry must be an ARN (an SNS topic's, usually)."
  }
}

# AN OVERRIDE SUPPLIES THE WHOLE GRANT, BUILDER AND PAYLOAD INCLUDED.
#
# There are exactly two supported shapes, and mixing them is refused on `builder`
# below:
#
#   1. No override. The module renders billet's own generator output and, with
#      `builder = true`, attaches its builder document beside it. Both are
#      ACCOUNT-WIDE, because the module has no deployment id at apply time — the
#      control plane mints one on its first run — and they agree with each other.
#
#   2. An override, with `builder = false`. You generate one document carrying
#      everything, and it can be VALUE-scoped because you know the id by then.
#
# Mixing them puts a narrow node policy beside a wide builder policy, and IAM
# unions allows: the role gets the wide one, and the isolation the override was
# chosen for is gone (issue #56).
variable "iam_policy_json" {
  description = "Override the node role's policy document entirely. Empty renders the committed generator output for the enabled features. Cannot be combined with builder = true — generate one document carrying every grant instead, with `billet init iam --deployment <id> --builder --payload-bucket <bucket>`, and leave the two builder inputs at their defaults."
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
# admitted because this module's rendering is presence-mode and the builder tags
# its instances with the same owner key. That is also why it cannot be combined
# with iam_policy_json, which is refused on the variable below.
variable "builder" {
  description = "Grant the node role what `billet ami build` needs: ec2:CreateImage on a builder-tagged instance and on the image and snapshots it makes, its own TerminateInstances for cleanup, GetConsoleOutput to read the verifier's report, and the CreateTags that stamps a verified image. Off by default — it widens the identity every job's instance is launched by, and a deployment that builds its AMI elsewhere should not carry it. Refused beside iam_policy_json: see that variable."
  type        = bool
  default     = false

  validation {
    # A BOOLEAN AND AN EMPTINESS, never a reading of the document. An earlier
    # attempt at this rule searched the override for a literal and was wrong in
    # both directions; whether an arbitrary IAM document admits the builder is
    # not a question terraform can answer, and this one does not ask it.
    condition     = !var.builder || var.iam_policy_json == ""
    error_message = "builder = true cannot be combined with iam_policy_json. This module's builder rendering is account-wide, because the module has no deployment id at apply time, and IAM unions allows — so beside a value-scoped override it hands the role account-wide reach over every deployment's builders in the account. Generate the override with `billet init iam --deployment <id> --builder --payload-bucket <bucket>` instead, so one document carries the node grant, the builder grant and the payload statement together, and leave builder = false. The payload bucket is not optional: `billet ami build` requires --payload-bucket, so an override without that statement fails at the staging step."
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
