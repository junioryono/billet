# fleet-codebuild's plan tests, against the child DIRECTLY: tftest's per-run module
# override makes the child the configuration under test, so its resources stay
# assertable — a root-level test can reach a child only through outputs. Same mocked
# provider as the root suite: no credentials, nothing created.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }

  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }
}

# EVERY SENTINEL MUST BE SUBSTITUTED. A leftover TFPARTITION or TFPARAMPATH is an IAM
# document that grants nothing and denies everything, and it fails at the first launch
# rather than at apply — which is the whole reason the renderings carry sentinels no
# real value can contain.
run "node_policy_sentinels_fully_substituted" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name = "billet-test"
  }

  assert {
    condition     = !can(regex("TF[A-Z]+", aws_iam_role_policy.node.policy))
    error_message = "the node policy still carries an unsubstituted sentinel"
  }

  assert {
    condition     = !can(regex("TF[A-Z]+", aws_iam_role_policy.build.policy))
    error_message = "the build role policy still carries an unsubstituted sentinel"
  }

  # THE PROJECT ARN IS THE ENTIRE OWNERSHIP BOUNDARY, because a build cannot be
  # tagged — so a policy scoped to "*" would let this node stop every build in the
  # account.
  assert {
    condition = strcontains(
      aws_iam_role_policy.node.policy,
      "arn:aws:codebuild:us-east-1:123456789012:project/billet-test",
    )
    error_message = "the node policy is not scoped to this deployment's project"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.node.policy, "\"codebuild:*\"")
    error_message = "the node policy grants every codebuild action"
  }

  # AND THE PARAMETER PATH IS THE OTHER ONE. On a shared account the sibling paths a
  # wildcard admits are other deployments' runner registrations.
  #
  # THE ACCOUNT IS NAMED HERE TOO, and it used to be a `*` while the project beside it
  # named one — harmless in practice, since a role acts only in its own account, and
  # exactly the scoped-looking-but-not shape every other ARN in this module refuses.
  assert {
    condition = strcontains(
      aws_iam_role_policy.node.policy,
      "arn:aws:ssm:us-east-1:123456789012:parameter/billet/billet-test/jit/*",
    )
    error_message = "the node policy is not scoped to this deployment's parameter path"
  }

  # AND NO SENTINEL SURVIVED into either policy, which is what makes the two
  # assertions above proof rather than coincidence: a substitution that silently
  # matched nothing would leave the uppercase token in the rendered document.
  assert {
    condition     = !strcontains(aws_iam_role_policy.node.policy, "TFACCOUNT")
    error_message = "the node policy still carries the TFACCOUNT sentinel"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.build.policy, "TFACCOUNT")
    error_message = "the build role policy still carries the TFACCOUNT sentinel"
  }
}

# THE BUILD'S ROLE RUNS INSIDE THE COMPUTE THAT EXECUTES A WORKFLOW, so every
# permission it holds is a permission arbitrary job code holds. It reads one parameter
# and writes logs.
run "build_role_starts_nothing_and_deletes_nothing" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name = "billet-test"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.build.policy, "codebuild:")
    error_message = "the build role can act on CodeBuild, so a job could launch runners billet never escrowed capacity for"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.build.policy, "ssm:DeleteParameter")
    error_message = "the build role can delete a parameter, so a job could destroy another build's registration"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.build.policy, "ssm:PutParameter")
    error_message = "the build role can write a parameter; it reads the one staged for it"
  }

  # AND THE NODE'S ROLE LAUNCHES NO INSTANCES. A codebuild node is an orchestrator
  # that calls one API; the first version of the backup policy is the record of what
  # happens when a principal gets the ec2 runtime statements because nothing said not
  # to.
  assert {
    condition     = !strcontains(aws_iam_role_policy.node.policy, "ec2:RunInstances")
    error_message = "the codebuild node role can launch EC2 instances"
  }
}

# NO WEBHOOK, EVER. CodeBuild's own GitHub Actions runner integration is
# webhook-driven, and a WORKFLOW_JOB_QUEUED webhook on a billet-owned project means two
# schedulers acquiring one job — which produces duplicate runners and reads as GitHub
# misbehaving rather than as a configuration mistake.
run "no_runner_webhook_is_created" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name = "billet-test"
  }

  assert {
    condition     = length(aws_codebuild_project.this) == 1
    error_message = "no project was planned"
  }

  # THE SOURCE IS NO_SOURCE, because billet clones nothing: the runner does the
  # checkout inside the job, and a project with a source would have CodeBuild fetch a
  # repository before the runner starts, on a machine holding a registration.
  assert {
    condition     = one(aws_codebuild_project.this[*].source[0].type) == "NO_SOURCE"
    error_message = "the project has a source configured"
  }
}

# A CONTAINER ENVIRONMENT NEEDS PRIVILEGED MODE, or every job fails on its first
# `docker build` — which a real Actions workflow reaches almost immediately.
run "a_container_environment_gets_docker_privilege" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name             = "billet-test"
    environment_type = "LINUX_CONTAINER"
  }

  assert {
    condition     = one(aws_codebuild_project.this[*].environment[0].privileged_mode) == true
    error_message = "a container project has no Docker privilege, so every job would fail on docker build"
  }
}

# AND AN EC2 OR macOS ENVIRONMENT DOES NOT, because it IS the machine — there is
# nothing to privilege, and billet's config refuses the setting there rather than
# ignoring it.
run "a_macos_fleet_gets_no_container_privilege" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name             = "billet-test"
    environment_type = "MAC_ARM"
    compute_type     = "BUILD_GENERAL1_MEDIUM"
    enable_fleet     = true
    fleet_capacity   = 2
  }

  assert {
    condition     = one(aws_codebuild_project.this[*].environment[0].privileged_mode) == false
    error_message = "a macOS project asked for container privilege"
  }

  assert {
    condition     = length(aws_codebuild_fleet.this) == 1
    error_message = "a macOS deployment planned no reserved fleet, and on-demand CodeBuild has no macOS"
  }

  # THE FLEET CAPACITY IS THE NUMBER A macOS TIER'S macos_vm_limit SHOULD BE, which is
  # why the module outputs it: billet refuses to assume Apple's per-host allowance
  # applies to a fleet AWS operates.
  assert {
    condition     = output.fleet_capacity == 2
    error_message = "the fleet capacity is not reported, so an operator cannot set macos_vm_limit from it"
  }

  # THE FLEET GRANTS ARE ASSERTED IN THE ADOPTED RUN BELOW, not here, and the reason
  # is worth stating: a CREATED fleet's ARN is unknown until apply, so the rendered
  # policy that embeds it is unknown at plan and terraform refuses the condition
  # outright ("Unknown condition value"). An adopted fleet has a known ARN, which
  # makes the same property checkable without an apply — and an apply against a
  # mocked provider would be asserting on values the mock invented anyway.
}

# THE CEILINGS ARE AN OUTPUT, because billet requires them visible before work is
# admitted — and an operator reading a plan is exactly then.
run "the_external_ceilings_are_reported" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name = "billet-test"
  }

  assert {
    condition     = output.external_ceilings.build_timeout_minutes == 2160
    error_message = "the build ceiling is not reported"
  }

  assert {
    condition     = output.external_ceilings.queued_timeout_minutes == 480
    error_message = "the queued ceiling is not reported"
  }

  assert {
    condition     = strcontains(output.external_ceilings.note, "accept_external_build_ceiling")
    error_message = "the note does not say which config key acknowledges the ceilings"
  }
}

# ADOPT OR CREATE, for everything that can be either. The backend's design asks for it explicitly: an
# operator who already has a project, a fleet, a VPC or a log group must be able to
# place billet in it.
run "an_existing_project_and_fleet_are_adopted" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name                     = "billet-test"
    project_name             = "already-mine"
    existing_build_role_name = "already-mines-service-role"
    fleet_arn                = "arn:aws:codebuild:us-east-1:123456789012:fleet/already-mine"
    log_group_name           = "/my/own/group"
    environment_type         = "MAC_ARM"
  }

  assert {
    condition     = length(aws_codebuild_project.this) == 0
    error_message = "an adopted project was recreated"
  }

  # NO REPLACEMENT BUILD ROLE. Creating one while adopting a project produced a role
  # NOTHING USED: the adopted project kept its own, so its builds could not read their
  # registration and every job failed to register while the apply reported success.
  assert {
    condition     = length(aws_iam_role.build) == 0
    error_message = "adopting a project still created a build role, which nothing would run as"
  }

  # AND THE POLICY LANDED ON THE ROLE THAT ACTUALLY RUNS THE BUILDS.
  assert {
    condition     = aws_iam_role_policy.build.role == "already-mines-service-role"
    error_message = "the build-role policy was attached to something other than the adopted project's own role"
  }

  assert {
    condition     = length(aws_codebuild_fleet.this) == 0
    error_message = "an adopted fleet was recreated"
  }

  assert {
    condition     = length(aws_cloudwatch_log_group.this) == 0
    error_message = "an adopted log group was recreated"
  }

  # THE POLICY STILL SCOPES TO THE ADOPTED NAMES, or the node would be granted
  # nothing it can use.
  assert {
    condition = strcontains(
      aws_iam_role_policy.node.policy,
      "arn:aws:codebuild:us-east-1:123456789012:project/already-mine",
    )
    error_message = "the node policy does not name the adopted project"
  }

  assert {
    condition     = output.project_name == "already-mine"
    error_message = "the adopted project is not reported for node.codebuild.project"
  }

  # AND THE FLEET IS READ-ONLY TO THE NODE. A fleet is standing cost, and billet never
  # creates, resizes or deletes one — so `billet check` can report its capacity and
  # nothing can change it.
  assert {
    condition     = strcontains(aws_iam_role_policy.node.policy, "codebuild:BatchGetFleets")
    error_message = "the node cannot describe its own fleet, so billet check cannot report its capacity"
  }

  assert {
    condition = strcontains(
      aws_iam_role_policy.node.policy,
      "arn:aws:codebuild:us-east-1:123456789012:fleet/already-mine",
    )
    error_message = "the fleet grant is not scoped to the adopted fleet"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.node.policy, "codebuild:UpdateFleet")
    error_message = "the node can resize a fleet, which puts standing cost under a node process"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.node.policy, "codebuild:CreateFleet")
    error_message = "the node can create a fleet"
  }
}

# ADOPTING A PROJECT WITHOUT NAMING ITS ROLE IS REFUSED, in both directions: the two
# go together, because only a created project has its service role set by this module.
run "adoption_requires_the_projects_own_build_role" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name         = "billet-test"
    project_name = "already-mine"
  }

  expect_failures = [var.existing_build_role_name]
}

run "naming_a_build_role_without_adopting_is_refused" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name                     = "billet-test"
    existing_build_role_name = "somebody-elses"
  }

  expect_failures = [var.existing_build_role_name]
}

# A CUSTOMER-MANAGED KEY SCOPES BOTH ROLES' KMS GRANTS TO IT, AND TO PARAMETER STORE.
#
# Without a key, a SecureString is encrypted under the account's aws/ssm key, which
# authorizes any principal in the account that can reach Parameter Store — so a
# neighbouring deployment's role could read this one's staged registrations. The
# kms:ViaService condition is what keeps the grant from decrypting anything else the
# key protects.
run "enable_kms_plans_a_key" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name       = "billet-test"
    enable_kms = true
  }

  assert {
    condition     = length(aws_kms_key.registrations) == 1
    error_message = "enable_kms planned no key"
  }

  assert {
    condition     = length(aws_kms_alias.registrations) == 1
    error_message = "the created key has no alias, so nothing names it in the console"
  }

  # THE RENDERED GRANT IS ASSERTED IN THE ADOPTED-KEY RUN BELOW. A created key's ARN
  # is unknown until apply, so the policy that embeds it is unknown at plan and
  # terraform refuses the condition outright — the same limitation the created-fleet
  # case hits, for the same reason.
}

# AN ADOPTED KEY HAS A KNOWN ARN, which is what makes the grant's SHAPE checkable
# without an apply.
#
# Without a customer-managed key, a SecureString is encrypted under the account's
# aws/ssm key, which authorizes any principal in the account that can reach Parameter
# Store — so a neighbouring deployment's role could read this one's staged
# registrations. The kms:ViaService condition is what keeps the grant from decrypting
# anything else the key protects.
run "an_adopted_key_is_scoped_to_parameter_store" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name        = "billet-test"
    kms_key_arn = "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555"
  }

  assert {
    condition     = length(aws_kms_key.registrations) == 0
    error_message = "an adopted key was recreated"
  }

  assert {
    condition     = strcontains(aws_iam_role_policy.build.policy, "ssm.us-east-1.amazonaws.com")
    error_message = "the build role's KMS grant is not scoped to Parameter Store by kms:ViaService"
  }

  assert {
    condition     = strcontains(aws_iam_role_policy.node.policy, "ssm.us-east-1.amazonaws.com")
    error_message = "the node's KMS grant is not scoped to Parameter Store by kms:ViaService"
  }

  assert {
    condition = strcontains(
      aws_iam_role_policy.build.policy,
      "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555",
    )
    error_message = "the build role's KMS grant does not name the adopted key"
  }

  # THE BUILD MAY ONLY DECRYPT. A build that could encrypt under this key could
  # stage a registration of its own choosing.
  assert {
    condition     = !strcontains(aws_iam_role_policy.build.policy, "kms:Encrypt")
    error_message = "the build role can encrypt under the registration key"
  }

  assert {
    condition     = !can(regex("TF[A-Z]+", aws_iam_role_policy.build.policy))
    error_message = "the KMS rendering still carries an unsubstituted sentinel"
  }

  assert {
    condition     = !can(regex("TF[A-Z]+", aws_iam_role_policy.node.policy))
    error_message = "the KMS rendering still carries an unsubstituted sentinel"
  }
}

# A VPC-CONNECTED FLEET NEEDS ITS OWN SERVICE ROLE — AWS requires the pair, and
# `terraform validate` is what said so. A fleetOverride on StartBuild also makes
# CodeBuild ignore the PROJECT's VPC config, so the fleet's network is the one that
# governs and the same settings have to reach it.
#
# AND THE NETWORKING PERMISSIONS GO ON THAT ROLE, NEVER ON THE BUILD'S. A CodeBuild
# service role's credentials are reachable from inside the build, so anything on the
# build role is available to arbitrary workflow code — and the ENI permissions cannot
# be resource-scoped. The first version of this module attached them to the build role
# and the test that was supposed to forbid it read only the GENERATED rendering, so a
# supplemental HCL policy was invisible to it.
run "a_vpc_fleet_puts_networking_on_the_fleet_role_only" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name               = "billet-test"
    enable_fleet       = true
    vpc_id             = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids         = ["subnet-0a0a0a0a0a0a0a0a0"]
    security_group_ids = ["sg-0b0b0b0b0b0b0b0b0"]
  }

  assert {
    condition     = length(aws_iam_role.fleet) == 1
    error_message = "a VPC-connected fleet planned no service role, which AWS refuses"
  }

  assert {
    condition     = length(one(aws_codebuild_fleet.this[*].vpc_config)) == 1
    error_message = "the fleet has no VPC configuration, so the project's would be ignored and nothing would apply"
  }

  assert {
    condition     = length(aws_iam_role_policy.fleet) == 1
    error_message = "the fleet has no network-interface permissions, so it cannot attach to the VPC"
  }

  # THE ASSEMBLED BUILD ROLE, NOT ONE DOCUMENT. There is exactly one policy on it by
  # construction now, and this is what makes that checkable — a second policy resource
  # attached to the same role is what slipped past the generated-rendering assertion.
  assert {
    condition     = !strcontains(aws_iam_role_policy.build.policy, "ec2:")
    error_message = "the build role can act on EC2; its credentials are reachable from inside the build, so that is every workflow's permission"
  }
}

# AND A VPC WITHOUT A FLEET IS REFUSED AT PLAN, because there is no safe role to put
# the networking permissions on: an on-demand VPC project's service role IS the role the
# build runs as.
run "a_vpc_without_a_fleet_is_refused" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name               = "billet-test"
    vpc_id             = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids         = ["subnet-0a0a0a0a0a0a0a0a0"]
    security_group_ids = ["sg-0b0b0b0b0b0b0b0b0"]
  }

  expect_failures = [var.security_group_ids]
}

# AND A VPC WITH AN *ADOPTED* FLEET IS REFUSED, because the module would configure
# nothing at all.
#
# THIS APPLIED CLEANLY AND DID NOTHING. A fleet's VPC settings live on the fleet, this
# module cannot edit a fleet it does not own, and billet sends fleetOverride on every
# launch — which makes CodeBuild ignore the project's VPC configuration entirely. So the
# inputs went nowhere, terraform reported success, and the builds ran on whatever
# network that fleet already had. An apply that accepts a network setting and silently
# does not apply it is worse than one that refuses.
run "a_vpc_with_an_adopted_fleet_is_refused" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name               = "billet-test"
    fleet_arn          = "arn:aws:codebuild:us-east-1:123456789012:fleet/adopted"
    vpc_id             = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids         = ["subnet-0a0a0a0a0a0a0a0a0"]
    security_group_ids = ["sg-0b0b0b0b0b0b0b0b0"]
  }

  expect_failures = [var.security_group_ids]
}

# AND enable_fleet TOGETHER WITH fleet_arn IS REFUSED, which is the same hole through a
# narrower door — and the first version of the guard above left it open.
#
# `create_fleet` is `enable_fleet && fleet_arn == ""`, so with both set the ADOPTED fleet
# wins and no fleet resource is created at all. A validation that asked only for
# `enable_fleet` therefore passed while the VPC inputs reached nothing. The predicate has
# to match the one that decides whether a fleet exists, not the flag that asks for one.
run "a_vpc_with_both_fleet_inputs_is_refused" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name               = "billet-test"
    enable_fleet       = true
    fleet_arn          = "arn:aws:codebuild:us-east-1:123456789012:fleet/adopted"
    vpc_id             = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids         = ["subnet-0a0a0a0a0a0a0a0a0"]
    security_group_ids = ["sg-0b0b0b0b0b0b0b0b0"]
  }

  expect_failures = [var.security_group_ids]
}

# AND A PARTIAL VPC CONFIGURATION IS REFUSED, which was the other silent no-op:
# `use_vpc` needs both a vpc_id and a subnet, so naming one without the other created a
# fleet with no network configuration and reported success.
run "a_vpc_id_without_subnets_is_refused" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name         = "billet-test"
    enable_fleet = true
    vpc_id       = "vpc-0f0f0f0f0f0f0f0f0"
  }

  expect_failures = [var.security_group_ids]
}

run "subnets_without_security_groups_are_refused" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name         = "billet-test"
    enable_fleet = true
    vpc_id       = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids   = ["subnet-0a0a0a0a0a0a0a0a0"]
  }

  expect_failures = [var.security_group_ids]
}

# A RESERVED FLEET TAKES EXACTLY ONE SUBNET, in one availability zone — AWS's own
# documentation, refused here rather than by the API partway through an apply.
run "two_subnets_are_refused" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name               = "billet-test"
    enable_fleet       = true
    vpc_id             = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids         = ["subnet-0a0a0a0a0a0a0a0a0", "subnet-0c0c0c0c0c0c0c0c0"]
    security_group_ids = ["sg-0b0b0b0b0b0b0b0b0"]
  }

  expect_failures = [var.subnet_ids]
}

# AND A COMPLETE VPC CONFIGURATION ON A CREATED FLEET STILL WORKS, or the four refusals
# above would be refusing the only shape the module supports — the failure ADR-005 names,
# after which somebody deletes the check.
run "a_complete_vpc_on_a_created_fleet_is_accepted" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name               = "billet-test"
    enable_fleet       = true
    vpc_id             = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids         = ["subnet-0a0a0a0a0a0a0a0a0"]
    security_group_ids = ["sg-0b0b0b0b0b0b0b0b0"]
  }

  # THE FLEET CARRIES THE NETWORK, which is the property the refusals protect: this is
  # the only configuration in which the inputs reach anything.
  assert {
    condition     = length(aws_codebuild_fleet.this) == 1
    error_message = "no fleet was planned, so the VPC configuration has nowhere to land"
  }

  # THE NETWORK IS ON THE FLEET, taken from the inputs rather than derived, so it is
  # known at plan — unlike the created service role's ARN, which is not.
  assert {
    condition     = length(aws_codebuild_fleet.this[0].vpc_config) == 1
    error_message = "the fleet was planned with no vpc_config, so the VPC inputs went nowhere"
  }

  assert {
    condition     = aws_codebuild_fleet.this[0].vpc_config[0].vpc_id == var.vpc_id
    error_message = "the fleet's vpc_config does not name the vpc that was passed in"
  }
}

# THE DESTROY GUARD EXISTS AND NAMES THE PROJECT IT WATCHES. AWS deletes a project out
# from under a running build (DeleteProject succeeds and the build carries on; measured
# 2026-09-02), so the only thing standing between `terraform destroy` and somebody's
# failed build is this resource's destroy-time provisioner. Its `input` is what reaches
# the script, so a guard planned against the wrong name would refuse nothing.
#
# tftest cannot see dependency edges or provisioner blocks; the Go test beside the
# script (scripts/codebuild_destroy_guard_test.go) reads main.tf for those.
run "the_destroy_guard_watches_the_created_project" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name = "billet-test"
  }

  assert {
    condition     = terraform_data.active_build_guard.input.project == "billet-test"
    error_message = "the destroy guard does not name the project the module created"
  }

  assert {
    condition     = terraform_data.active_build_guard.input.region == "us-east-1"
    error_message = "the destroy guard does not carry the provider's region"
  }
}

# AND AN ADOPTED PROJECT IS GUARDED TOO: the destroy still removes the roles and the
# log group a running build in that project depends on.
run "the_destroy_guard_watches_an_adopted_project" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name                     = "billet-test"
    project_name             = "somebody-elses-project"
    existing_build_role_name = "their-build-role"
  }

  assert {
    condition     = terraform_data.active_build_guard.input.project == "somebody-elses-project"
    error_message = "the destroy guard does not name the adopted project"
  }
}

# THE CONTROLLER'S SWEEP GRANT LANDS ON THE CONTROLLER'S ROLE, scoped to this path and
# its children, and carries nothing a listing and a delete do not need. It is the one
# grant that reaches the principal holding the ledger and the App key.
run "the_controller_sweep_is_scoped_to_the_path" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name                 = "billet-test"
    controller_role_name = "billet-controller"
  }

  assert {
    condition     = length(aws_iam_role_policy.controller_sweep) == 1
    error_message = "naming a controller role attached no sweep policy"
  }

  assert {
    condition     = aws_iam_role_policy.controller_sweep[0].role == "billet-controller"
    error_message = "the sweep policy is not attached to the controller role"
  }

  assert {
    condition     = !can(regex("TF[A-Z]+", aws_iam_role_policy.controller_sweep[0].policy))
    error_message = "the controller sweep policy still carries an unsubstituted sentinel"
  }

  assert {
    condition = strcontains(
      aws_iam_role_policy.controller_sweep[0].policy,
      "\"arn:aws:ssm:us-east-1:123456789012:parameter/billet/billet-test/jit\"",
    )
    error_message = "the sweep policy does not name the path itself, which GetParametersByPath is authorised against"
  }

  assert {
    condition = strcontains(
      aws_iam_role_policy.controller_sweep[0].policy,
      "\"arn:aws:ssm:us-east-1:123456789012:parameter/billet/billet-test/jit/*\"",
    )
    error_message = "the sweep policy does not name the path's children, which DeleteParameter is authorised against"
  }

  assert {
    condition     = strcontains(aws_iam_role_policy.controller_sweep[0].policy, "ssm:GetParametersByPath")
    error_message = "the sweep policy cannot list the path"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.controller_sweep[0].policy, "ssm:PutParameter") && !strcontains(aws_iam_role_policy.controller_sweep[0].policy, "\"ssm:GetParameter\"") && !strcontains(aws_iam_role_policy.controller_sweep[0].policy, "codebuild:") && !strcontains(aws_iam_role_policy.controller_sweep[0].policy, "kms:")
    error_message = "the controller sweep policy grants something beyond listing and deleting under the path"
  }

  # AND THE RENDERED OUTPUT IS THE SAME DOCUMENT, for a controller role terraform does
  # not own.
  assert {
    condition     = output.controller_sweep_policy_json == aws_iam_role_policy.controller_sweep[0].policy
    error_message = "the output and the attached policy differ"
  }
}

# AND NOTHING IS ATTACHED WHEN NO CONTROLLER ROLE IS NAMED.
run "no_controller_role_means_no_sweep_grant" {
  command = plan

  module {
    source = "./modules/fleet-codebuild"
  }

  variables {
    name = "billet-test"
  }

  assert {
    condition     = length(aws_iam_role_policy.controller_sweep) == 0
    error_message = "a sweep policy was attached with no controller role to attach it to"
  }
}
