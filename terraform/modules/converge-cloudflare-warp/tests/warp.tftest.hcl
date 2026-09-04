# Plan tests for the three things that decide whether this module admits the
# runner to the fleet or to nothing -- or to everything.
#
# WHY THEY EXIST. Each of the properties below was learned on a real
# deployment and none is a syntax error: an `allow` decision on the enrolment
# policy plans and applies and never matches; a `/32` in the Gateway rule
# applies and then diffs forever; a rule without the identity clause admits
# every WARP identity in the account. A mocked provider sees the documents
# exactly as they would be sent.

mock_provider "cloudflare" {}

variables {
  account_id     = "0123456789abcdef0123456789abcdef"
  team_name      = "example"
  host_addresses = ["100.96.0.14", "10.60.0.10"]
  precedence     = 3
}

# APPLY, NOT PLAN, so the token's id is a value rather than an unknown and the
# policy can be proved to reference THIS token: a plan-time assertion could
# only count the includes, and a policy naming somebody else's token counts
# the same.
run "the_enrolment_policy_is_service_auth_for_this_token" {
  command = apply

  # `non_identity` is the API spelling of Service Auth. An `allow` decision
  # with a service-token include is a policy that never matches, because a
  # service token presents no identity for allow to admit -- so the device
  # would fail to enrol while the module looked configured.
  assert {
    condition     = cloudflare_zero_trust_access_policy.enrol.decision == "non_identity"
    error_message = "the enrolment policy must be a Service Auth (non_identity) decision; an allow decision never matches a service token"
  }

  assert {
    condition     = length(cloudflare_zero_trust_access_policy.enrol.include) == 1
    error_message = "the enrolment policy must include exactly one thing"
  }

  # `include` is a SET, so the token id is proved across every element rather
  # than by index: exactly one element, and it names this token.
  assert {
    condition = alltrue([
      for i in cloudflare_zero_trust_access_policy.enrol.include :
      try(i.service_token.token_id, "") == cloudflare_zero_trust_access_service_token.ci.id
    ])
    error_message = "the enrolment policy must include THIS module's token; another token's id would leave CI unable to enrol while the module looked configured"
  }

  # NO ROTATION UNLESS ASKED: the attribute is null, so an ordinary apply
  # mints nothing new.
  assert {
    condition     = cloudflare_zero_trust_access_service_token.ci.previous_client_secret_expires_at == null
    error_message = "a token that was not asked to rotate must not carry a previous-secret expiry"
  }
}

# ROTATION IS TWO INPUTS, PASSED THROUGH TOGETHER: the version that mints the
# new secret and the window the previous one stays valid for, which is the
# only way CI keeps an enrolment while the secret changes. The provider
# refuses the expiry without the version (measured on v5), so the module
# refuses either one alone before the provider can.
run "rotation_passes_the_version_and_the_expiry_to_the_token" {
  command = plan

  variables {
    client_secret_version             = 2
    previous_client_secret_expires_at = "2026-10-01T00:00:00Z"
  }

  assert {
    condition = (
      cloudflare_zero_trust_access_service_token.ci.client_secret_version == 2 &&
      cloudflare_zero_trust_access_service_token.ci.previous_client_secret_expires_at == "2026-10-01T00:00:00Z"
    )
    error_message = "the rotation's version and window must both reach the token resource"
  }
}

run "refuses_a_rotation_that_is_not_a_timestamp" {
  command = plan

  variables {
    client_secret_version             = 2
    previous_client_secret_expires_at = "tomorrow"
  }

  expect_failures = [var.previous_client_secret_expires_at]
}

run "refuses_an_expiry_without_a_version" {
  command = plan

  variables {
    previous_client_secret_expires_at = "2026-10-01T00:00:00Z"
  }

  expect_failures = [var.previous_client_secret_expires_at]
}

run "refuses_a_version_without_an_expiry" {
  command = plan

  variables {
    client_secret_version = 2
  }

  expect_failures = [var.previous_client_secret_expires_at]
}

run "the_gateway_rule_admits_exactly_the_fleet" {
  command = plan

  # BARE ADDRESSES, JOINED BY SPACES, IN THE OPERATOR'S ORDER. Gateway
  # normalises /32 off a single address, so a rule written with /32s is a
  # permanent diff; asserted as the whole string so an appended range or a
  # dropped host fails.
  assert {
    condition     = cloudflare_zero_trust_gateway_policy.allow.traffic == "net.dst.ip in {100.96.0.14 10.60.0.10}"
    error_message = "the Gateway rule must name exactly the given hosts as bare addresses"
  }

  # THE IDENTITY CLAUSE IS WHAT SCOPES THE RULE TO THE RUNNER. Without it the
  # rule admits every WARP identity in the account to the fleet.
  assert {
    condition     = cloudflare_zero_trust_gateway_policy.allow.identity == "identity.email == \"non_identity@example.cloudflareaccess.com\""
    error_message = "the Gateway rule must be keyed on the non_identity principal of this team"
  }

  assert {
    condition     = output.principal == "non_identity@example.cloudflareaccess.com"
    error_message = "the principal output must be what the rule is keyed on, so the operator can find it in Gateway's logs"
  }

  # L4 AND ALLOW. An http filter would never see SSH; a block would be the
  # opposite of the module's purpose.
  assert {
    condition = (
      tolist(cloudflare_zero_trust_gateway_policy.allow.filters) == tolist(["l4"]) &&
      cloudflare_zero_trust_gateway_policy.allow.action == "allow" &&
      cloudflare_zero_trust_gateway_policy.allow.enabled == true &&
      cloudflare_zero_trust_gateway_policy.allow.precedence == 3
    )
    error_message = "the Gateway rule must be an enabled L4 allow at the given precedence"
  }
}

# A /32 IS REFUSED BY THE VARIABLE. It would apply, and then never converge.
run "refuses_a_prefixed_address" {
  command = plan

  variables {
    host_addresses = ["100.96.0.14/32"]
  }

  expect_failures = [var.host_addresses]
}

run "refuses_a_hostname" {
  command = plan

  variables {
    host_addresses = ["billet-host-1.example.com"]
  }

  expect_failures = [var.host_addresses]
}

run "refuses_an_empty_fleet" {
  command = plan

  variables {
    host_addresses = []
  }

  expect_failures = [var.host_addresses]
}

# THE TEAM NAME IS THE BARE NAME, not the domain: the principal is composed
# from it, and a domain here would produce non_identity@example.cloudflareaccess.com.cloudflareaccess.com.
run "refuses_a_team_domain" {
  command = plan

  variables {
    team_name = "example.cloudflareaccess.com"
  }

  expect_failures = [var.team_name]
}
