# Reaching Billet hosts from CI as a WARP client enrolled with a service token.
#
# OPTIONAL, AND NOT PART OF THE billet ROOT MODULE. Billet needs no inbound
# connectivity to run jobs: nodes always dial outbound. This exists for a
# deployment whose hosts are ALREADY reachable on private addresses inside the
# account's Zero Trust network -- a Mesh node, a cloudflared route to a VPC --
# and whose converge runs from CI. The runner joins that network as a headless
# WARP client and connects over plain SSH; there is no per-host Access
# application, no SSH proxy in the path, and no dependency on the Access
# proxy engaging.
#
# WHAT IT CREATES, AND THE ONE THING IT DOES NOT. The service token, the Service
# Auth policy that lets a device holding it enrol, and one Gateway L4 rule that
# is the whole of what the enrolled device may reach. It does NOT touch the
# account's WARP enrolment application: that application is the one thing
# between every employee and their own network, adopting it means pinning every
# live field (in provider v5 an omitted optional plans to null on an adopted
# resource, and `policies` is one of them), and a module cannot know those
# values. The operator attaches this module's policy to it -- one action, in
# the dashboard or in their own root -- and the README says how.
#
# WHY A SERVICE TOKEN AND NOT A CONNECTOR TOKEN. A WARP Connector authenticates
# as warp_connector@<team>, ONE account-wide identity shared by every connector
# because Gateway exposes no per-connector field, so a connector token in CI
# would inherit every grant that identity already holds. non_identity@ is also
# a shared principal -- said plainly -- but it holds no grant until one is
# written, so the rule below is its entire reach.

locals {
  principal = "non_identity@${var.team_name}.cloudflareaccess.com"
}

# The client_id is a username; the client_secret is bearer material and lands
# in state, the same class as a tunnel token. Both halves are read from
# `terraform output` once and set as CI secrets.
#
# ROTATION IS A SECRET ROTATION, NOT A REPLACEMENT: set
# previous_client_secret_expires_at a day or two out and apply, and Cloudflare
# mints a new secret under the same client_id while the old one stays valid
# until then. A -replace mints a new id, rewires the policy to it in the same
# apply, and leaves the old token authorised for nothing before the CI secret
# can be updated.
resource "cloudflare_zero_trust_access_service_token" "ci" {
  account_id = var.account_id
  name       = "${var.name} (CI)"
  duration   = var.token_duration

  lifecycle {
    create_before_destroy = true
  }
}

# "THIS TOKEN MAY ENROL A DEVICE". `non_identity` is the API spelling of the
# dashboard's Service Auth action, and it is required: an `allow` decision with
# a service-token include is a policy that never matches, because a service
# token presents no identity for `allow` to admit.
#
# REUSABLE, AND NOT ATTACHED HERE. Attaching it is a write to the enrolment
# application, which this module refuses to own (see the header). Until the
# operator attaches it the token enrols nothing, which is the failure that is
# easy to see; the opposite -- an omitted optional removing the employee rule
# under an unattended apply -- is the one that is not.
resource "cloudflare_zero_trust_access_policy" "enrol" {
  account_id = var.account_id
  name       = "${var.name}: service-token device enrolment"
  decision   = "non_identity"

  include = [
    {
      service_token = {
        token_id = cloudflare_zero_trust_access_service_token.ci.id
      }
    }
  ]
}

# WHAT THE ENROLLED RUNNER MAY REACH: exactly these addresses, on any port.
# Keyed on the non_identity@ principal, which Cloudflare's own pre-login
# deployment documentation names as the identity a service-token device
# carries. Gateway's network log names the rule that matched, which is where
# to look when a device enrols and the first SSH still times out.
resource "cloudflare_zero_trust_gateway_policy" "allow" {
  account_id  = var.account_id
  name        = "${var.name}: allow the converge runner to reach the fleet"
  description = "A WARP device enrolled with the ${var.name} service token (${local.principal}) may reach the fleet's hosts, and nothing else"
  precedence  = var.precedence
  action      = "allow"
  enabled     = true

  traffic  = "net.dst.ip in {${join(" ", var.host_addresses)}}"
  identity = "identity.email == \"${local.principal}\""

  filters = ["l4"]
}
