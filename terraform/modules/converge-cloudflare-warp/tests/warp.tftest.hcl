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

run "the_enrolment_policy_is_service_auth_for_this_token" {
  command = plan

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
    error_message = "the enrolment policy must include exactly this module's token and nothing else"
  }
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
