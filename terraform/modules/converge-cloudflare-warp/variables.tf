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

variable "token_duration" {
  description = "How long the service token is valid. A year by default; put the expiry in a calendar, because terraform plan does not warn on it. Rotate with previous_client_secret_expires_at on the token resource rather than by replacing it, so the enrolment policy stays satisfied while the new secret is copied."
  type        = string
  default     = "8760h"
}
