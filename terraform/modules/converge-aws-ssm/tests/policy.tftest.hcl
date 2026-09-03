# Plan tests for the two policies that decide who reaches this deployment's hosts.
#
# WHY THESE EXIST. Both of this module's P0 defects were in rendered JSON that
# every other gate reads as an opaque string: a trust policy that admitted
# pull-request jobs, and a session grant on every managed node in the account.
# terraform validate, tflint and trivy all passed over both. A lint finding proves
# the gate ran; only an assertion on the rendered document proves what it says.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }

  # THE POLICY INTERPOLATES THESE ARNS, so without known values the whole
  # rendered document is unknown at plan time and every assertion below is
  # unevaluable rather than false -- which reads as a passing test only if
  # nobody checks the summary.
  mock_resource "aws_s3_bucket" {
    defaults = {
      arn = "arn:aws:s3:::billet-converge-123456789012"
    }
  }

  mock_resource "aws_kms_key" {
    defaults = {
      arn = "arn:aws:kms:us-east-1:123456789012:key/00000000-0000-0000-0000-000000000000"
    }
  }
}

variables {
  name              = "billet-converge"
  github_repository = "an-org/an-infra-repo"

  # SUPPLIED SO THE DOCUMENT IS KNOWABLE AT PLAN TIME. Left empty the module
  # creates the provider, and its ARN is unknown until apply -- which makes the
  # whole rendered trust policy unknown and every assertion below unevaluable.
  github_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/token.actions.githubusercontent.com"
}

run "the_trust_policy_cannot_be_assumed_by_a_pull_request" {
  command = plan

  # A REF SUBJECT IS THE SHAPE A PR JOB CANNOT MINT. Its subject is either
  # ...:pull_request or, when it names one, ...:environment:NAME -- so trusting a
  # ref excludes both. An environment subject does NOT, which is what the second
  # version of this module got wrong.
  assert {
    condition     = strcontains(aws_iam_role.ci.assume_role_policy, "repo:an-org/an-infra-repo:ref:refs/heads/main")
    error_message = "the CI role does not trust an exact ref subject"
  }

  # StringLike ANYWHERE in this document is a wildcard subject, which is how the
  # first version admitted every PR job in the repository.
  assert {
    condition     = !strcontains(aws_iam_role.ci.assume_role_policy, "StringLike")
    error_message = "the trust policy matches a subject pattern rather than an exact subject; a wildcard admits pull-request jobs, and this role reads a bucket a GitHub App private key transits"
  }

  assert {
    condition     = !strcontains(aws_iam_role.ci.assume_role_policy, ":pull_request")
    error_message = "the trust policy names the pull_request subject"
  }
}

run "sessions_are_scoped_to_this_deployments_own_nodes" {
  # APPLY, NOT PLAN. A resource that has not been created has unknown computed
  # attributes at plan time whatever the mocks say, so the interpolated ARNs make
  # the rendered policy unknown -- and an unknown condition is an ERROR, not a
  # failure, which is a different thing to read in a summary. Nothing is created:
  # the provider is mocked.
  command = apply

  # EVERY STATEMENT MUST EARN ITS EXEMPTION, rather than the assertion selecting
  # the ones it expects to find.
  #
  # Two earlier versions of this were vacuous, each in a different way, and the
  # second is the one worth remembering. A strcontains for the tag condition
  # passed while StartSession was granted on every node in the account, because
  # the same condition appears on GetConnectionStatus. Decoding fixed that and
  # introduced a worse bug: the filter selected statements whose Resource
  # contained ":instance/" or ":managed-instance/", and `Resource = "*"` -- the
  # ACTUAL original defect -- contains neither. So it selected nothing,
  # alltrue([]) is true, and the assertion was blind to the broadest grant while
  # catching only narrower ones. My own mutation missed it because the mutant I
  # wrote kept the instance ARNs and removed the condition, which is a variant
  # nobody would ship rather than the defect that shipped.
  #
  # Inverted: COUNT the statements that grant a session-opening action without
  # the tag condition, and require zero. A wildcard action or resource now falls
  # INSIDE the selector instead of outside it.
  assert {
    condition = length([
      for st in jsondecode(aws_iam_role_policy.ci.policy).Statement : st
      if anytrue([for a in try(tolist(st.Action), [st.Action]) :
      a == "*" || a == "ssm:*" || a == "ssm:StartSession"])
      # The session-document grant is the one legitimate unconditioned
      # StartSession: a document is not a node.
      && length([for r in try(tolist(st.Resource), [st.Resource]) : r if strcontains(r, ":document/")]) == 0
      && try(st.Condition.StringEquals["ssm:resourceTag/sh.billet.converge"], null) == null
    ]) == 0
    error_message = "a statement grants a session-opening action without the activation's tag condition, so the CI role can open a session on any managed node in the account"
  }

  # ANTI-VACUITY. The assertion above is satisfied by a policy that grants
  # nothing at all, which is how the version before it passed. This one requires
  # the tag-scoped grant to actually exist.
  assert {
    condition = length([
      for st in jsondecode(aws_iam_role_policy.ci.policy).Statement : st
      if contains(try(tolist(st.Action), [st.Action]), "ssm:StartSession")
      && try(st.Condition.StringEquals["ssm:resourceTag/sh.billet.converge"], null) != null
    ]) == 1
    error_message = "no tag-scoped StartSession statement exists, so the check above passed over an empty set"
  }

  # The document Ansible actually uses. Granting AWS-StartSSHSession instead
  # denies every legitimate converge while looking deliberate.
  assert {
    condition     = strcontains(aws_iam_role_policy.ci.policy, "document/SSM-SessionManagerRunShell")
    error_message = "the policy does not grant the shell session document the amazon.aws.aws_ssm plugin opens"
  }

  # Without the data channel a session connects and carries nothing.
  assert {
    condition     = strcontains(aws_iam_role_policy.ci.policy, "ssmmessages:OpenDataChannel")
    error_message = "the policy omits the data channel, so a session would open and then stall"
  }

  # DECODED, FOR THE SAME REASON AS ABOVE. This was a strcontains, and the string
  # also appears on the OpenDataChannel statement -- so widening terminate/resume
  # to Resource "*" left it present and the assertion passed. Exactly the bug the
  # comment above describes, sitting six lines below it.
  assert {
    condition = alltrue([
      for st in jsondecode(aws_iam_role_policy.ci.policy).Statement :
      alltrue([for r in try(tolist(st.Resource), [st.Resource]) : strcontains(r, ":session/")])
      if contains(try(tolist(st.Action), [st.Action]), "ssm:TerminateSession")
    ])
    error_message = "terminate/resume is not scoped to this caller's own sessions, so the role has authority over other callers' sessions"
  }
}

run "the_transfer_bucket_does_not_retain_a_deleted_key" {
  command = apply

  # SUSPENDED, NOT ENABLED. Ansible deletes its transfer objects; with versioning
  # enabled that delete leaves a non-current version, so a GitHub App private key
  # would persist in history behind a listing that looks empty.
  assert {
    condition     = aws_s3_bucket_versioning.transfer.versioning_configuration[0].status == "Suspended"
    error_message = "bucket versioning is not Suspended, so Ansible's own cleanup would leave a recoverable copy of anything that transited it"
  }

  assert {
    condition     = aws_s3_bucket_public_access_block.transfer.block_public_policy && aws_s3_bucket_public_access_block.transfer.restrict_public_buckets
    error_message = "the transfer bucket is not fully closed to public access"
  }
}
