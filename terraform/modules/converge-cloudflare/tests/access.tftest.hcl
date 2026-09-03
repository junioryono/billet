# Plan tests for the two things that decide whether this module is a working door
# or an inert one.
#
# WHY THEY EXIST. Both of this module's defects were ABSENCES -- a policy created
# but never attached to its application, and a tunnel with no ingress. Neither is
# a syntax error, so terraform validate, tflint and trivy all passed over both,
# and the module would have applied cleanly and reached nothing. An absence is
# exactly what a plan assertion catches and a scanner cannot.

mock_provider "cloudflare" {}
mock_provider "random" {}

variables {
  account_id = "0123456789abcdef0123456789abcdef"
  zone_id    = "fedcba9876543210fedcba9876543210"
  hostname   = "billet-host-1.example.com"
}

run "the_application_carries_the_policy" {
  command = apply

  # ATTACHED, NOT MERELY CREATED. A reusable account policy that no application
  # references protects nothing, and Access is deny-by-default -- so the failure
  # is that NOTHING reaches the host, CI included, while the module looks
  # configured. That is harder to notice than an open door.
  assert {
    condition     = length(cloudflare_zero_trust_access_application.converge.policies) == 1
    error_message = "the Access application has no policy attached; Access is deny-by-default, so the tunnel would be inert while appearing configured"
  }

  assert {
    condition     = cloudflare_zero_trust_access_application.converge.policies[0].id == cloudflare_zero_trust_access_policy.ci.id
    error_message = "the application does not reference this module's own CI policy"
  }

  # A machine-to-machine flow. Leaving an identity provider enabled would let a
  # human browser session reach the host too.
  assert {
    condition     = cloudflare_zero_trust_access_policy.ci.decision == "non_identity"
    error_message = "the policy admits an identity decision rather than a service token only"
  }
}

run "the_tunnel_publishes_an_ssh_origin" {
  command = apply

  # Without ingress a connected cloudflared serves nothing: the hostname
  # resolves, Access admits the caller, and there is no origin behind it.
  assert {
    condition = anytrue([
      for rule in cloudflare_zero_trust_tunnel_cloudflared_config.converge.config.ingress :
      try(rule.hostname, "") == var.hostname && strcontains(try(rule.service, ""), "ssh://")
    ])
    error_message = "the tunnel does not map the hostname to an SSH origin, so a connected cloudflared publishes nothing"
  }

  # An ingress list whose last rule names a hostname is rejected by cloudflared.
  assert {
    condition     = try(one([for r in [element(cloudflare_zero_trust_tunnel_cloudflared_config.converge.config.ingress, length(cloudflare_zero_trust_tunnel_cloudflared_config.converge.config.ingress) - 1)] : r]).hostname, null) == null
    error_message = "the ingress list does not end in a catch-all rule, which cloudflared refuses"
  }
}
