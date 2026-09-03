variable "name" {
  description = "Name prefix for every resource this module creates."
  type        = string

  validation {
    # 47, NOT 50: this prefixes "-managed-instance" (17 characters), and IAM
    # refuses a role name over 64.
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,45}[a-z0-9]$", var.name))
    error_message = "name must be 3-47 lowercase alphanumerics or hyphens, not starting or ending with a hyphen: it prefixes an S3 bucket name and an IAM role name that appends -managed-instance."
  }
}

variable "github_repository" {
  description = "The owner/repo allowed to assume the CI role through GitHub's OIDC provider. A converge can install a GitHub App private key, so this must name one repository rather than an organisation-wide wildcard."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "github_repository must be exactly owner/repo. A wildcard would let any repository in the org assume a role that can read the transfer bucket."
  }
}

variable "github_branch" {
  description = "The branch a workflow must run on to assume the CI role. The trust policy matches the full subject repo:<repository>:ref:refs/heads/<this>, which a pull-request job CANNOT produce -- a PR job's subject is either repo:<repository>:pull_request or, if it declares an environment, repo:<repository>:environment:<name>. Neither is a ref subject."
  type        = string
  default     = "main"
}

variable "github_subject" {
  description = "Override the exact OIDC subject to trust. REQUIRED for repositories created after 2026-07-15 or opted into immutable subjects, whose subjects carry ids (repo:org@OWNER_ID/repo@REPO_ID:ref:refs/heads/main) and which the name-based default cannot authenticate. Also the way to trust an environment subject, having read the note on github_branch about what that does and does not exclude. Empty uses repo:<repository>:ref:refs/heads/<branch>. Matched with StringEquals either way; never a wildcard."
  type        = string
  default     = ""
}

variable "github_oidc_provider_arn" {
  description = "ARN of an existing GitHub OIDC provider in this account. Empty creates one; set it when the account already has a provider, because a second one for the same issuer is refused by IAM."
  type        = string
  default     = ""
}

variable "transfer_object_expiry_days" {
  description = "How long an object in the transfer bucket survives before lifecycle expiry. Ansible deletes its own objects at the end of a run; this bounds what an interrupted run leaves behind, and those objects can contain secrets from a task's arguments."
  type        = number
  default     = 1

  validation {
    condition     = var.transfer_object_expiry_days >= 1 && var.transfer_object_expiry_days <= 30
    error_message = "transfer_object_expiry_days must be between 1 and 30: these objects can carry a GitHub App private key, and keeping them longer than a debugging window has no upside."
  }
}

variable "tags" {
  description = "Tags applied to every resource that accepts them."
  type        = map(string)
  default     = {}
}
