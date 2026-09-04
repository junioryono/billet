variable "account_id" {
  description = "The Cloudflare account whose Zero Trust organisation the converge runner enrols in."
  type        = string
}

variable "team_name" {
  description = "The Zero Trust team name: the <team> in <team>.cloudflareaccess.com. A device enrolled with a service token authenticates as non_identity@<team>.cloudflareaccess.com, and that principal is what the Gateway rule is keyed on."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$", var.team_name))
    error_message = "team_name must be the bare team name (lowercase letters, digits, hyphens), not a domain."
  }
}

# BARE ADDRESSES, NOT /32s, and the validation is not fussiness. Gateway
# stores `10.0.0.5/32` as `10.0.0.5` and reads it back that way, so a rule
# written with /32s is a permanent one-line diff on a module that must reach
# `No changes` after every apply (measured on provider v5).
variable "host_addresses" {
  description = "The private IPv4 addresses the converge runner may reach -- each host's Mesh address or its cloudflared-routed address -- as bare addresses with no prefix length. This list is the ENTIRE scope of the shared non_identity@ principal, so it is one rule per fleet, not per token."
  type        = list(string)

  validation {
    condition     = length(var.host_addresses) > 0
    error_message = "host_addresses must name at least one host; a rule that admits nothing is a runner that reaches nothing."
  }

  validation {
    condition     = alltrue([for a in var.host_addresses : !strcontains(a, "/") && can(cidrnetmask("${a}/32"))])
    error_message = "every host address must be a bare IPv4 address with no prefix length: Gateway normalises /32 off a single address, so a rule written with one never converges."
  }
}

# REQUIRED, WITH NO DEFAULT. Gateway precedence is one account-wide sequence
# and the rule has to sort BEFORE the operator's own block rule for the
# private ranges or it never matches; a default number would collide with
# whatever rule already holds it, which on a root applied unattended is a
# silent reshuffle of somebody's access policy.
variable "precedence" {
  description = "The Gateway rule's precedence. It must be lower (earlier) than the rule that blocks the private ranges, and it must not collide with an existing rule: Cloudflare treats precedence as a position in one account-wide sequence."
  type        = number

  validation {
    condition     = var.precedence >= 1 && floor(var.precedence) == var.precedence
    error_message = "precedence must be a positive whole number."
  }
}

variable "name" {
  description = "Name prefix for the service token, the enrolment policy and the Gateway rule."
  type        = string
  default     = "billet-converge"
}

# THE ROTATION INPUTS, because create_before_destroy is not one: replacing the
# token mints a new id, rewires the enrolment policy to it in the same apply,
# and deletes the old one while CI still holds it. A rotation is TWO values
# set together, measured against provider v5: incrementing
# client_secret_version mints a NEW secret under the SAME client id, and the
# provider refuses previous_client_secret_expires_at without it ("Attribute
# client_secret_version must be specified when previous_client_secret_expires_at
# is specified"). Both are optional and computed on the resource, so null
# leaves the provider's own values alone and plans nothing.
variable "client_secret_version" {
  description = "Rotate the service token's secret by setting this to the current version plus one (terraform state shows the current one; a fresh token is 1). Null (the default) rotates nothing. Set it together with previous_client_secret_expires_at."
  type        = number
  default     = null

  validation {
    condition     = var.client_secret_version == null || (var.client_secret_version >= 1 && floor(var.client_secret_version) == var.client_secret_version)
    error_message = "client_secret_version must be a positive whole number."
  }
}

variable "previous_client_secret_expires_at" {
  description = "The RFC 3339 timestamp until which the PREVIOUS client secret stays valid after a rotation, so the enrolment policy stays satisfied while the CI secret is copied. Null (the default) rotates nothing. Set it a day or two out TOGETHER with client_secret_version, apply, copy the new secret from the output into CI, and extend it if you need more time."
  type        = string
  default     = null

  validation {
    condition     = var.previous_client_secret_expires_at == null || can(formatdate("YYYY-MM-DD", var.previous_client_secret_expires_at))
    error_message = "previous_client_secret_expires_at must be an RFC 3339 timestamp, e.g. 2026-10-01T00:00:00Z."
  }

  validation {
    condition     = (var.previous_client_secret_expires_at == null) == (var.client_secret_version == null)
    error_message = "a rotation is client_secret_version AND previous_client_secret_expires_at together: the provider refuses the expiry without the version, and a version without an expiry invalidates the previous secret the moment CI needs it."
  }
}

variable "token_duration" {
  description = "How long the service token is valid. A year by default; put the expiry in a calendar, because terraform plan does not warn on it. Rotate with previous_client_secret_expires_at on the token resource rather than by replacing it, so the enrolment policy stays satisfied while the new secret is copied."
  type        = string
  default     = "8760h"
}
