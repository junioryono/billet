# fleet-ec2's plan tests, against the child DIRECTLY: tftest's per-run module
# override makes the child the configuration under test, so its resources stay
# assertable — a root-level test can reach a child only through outputs. Same
# mocked provider as the root suite: no credentials, nothing created.

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

run "sentinels_fully_substituted" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id       = "vpc-0f0f0f0f0f0f0f0f0"
    name         = "billet-test"
    cache_bucket = "acme-cache-us-east-1" # contains a region string on purpose
    cache_prefix = "team-a"
  }

  # No committed sentinel may survive into the rendered policy.
  assert {
    condition     = !can(regex("TF(REGION|BUCKET|PREFIX|KMSKEY|PARTITION|DNSSUFFIX)", aws_iam_role_policy.node.policy))
    error_message = "a committed policy sentinel was not substituted"
  }

  # The partition sentinel is rewritten to the real partition (aws in the
  # commercial test provider), so the rendered ARNs are well-formed.
  assert {
    condition     = strcontains(aws_iam_role_policy.node.policy, "arn:aws:s3:::acme-cache-us-east-1")
    error_message = "the partition sentinel must be substituted into a real ARN"
  }

  # The ListBucket statement is asserted structurally, not by substring: the
  # object-resource ARN also contains "team-a/*", so a substring search would
  # pass with the condition deleted or regressed to a leading slash (the case
  # F1 caught). try() guards statements that have no Condition at all.
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.node.policy).Statement :
      try(
        s.Sid == "BilletCacheList" &&
        tolist(s.Condition.StringLike["s3:prefix"]) == tolist(["team-a/*"]) &&
        tolist(s.Resource) == tolist(["arn:aws:s3:::acme-cache-us-east-1"]),
        false
      )
    ])
    error_message = "the BilletCacheList statement must scope s3:prefix to exactly the real cache_prefix on the bare bucket ARN"
  }

  # THE LAUNCH BOUNDARY, structurally. ec2:RunInstances is granted on "*" and it
  # authorises every snapshot a block-device mapping names, so without this Deny
  # the node role can launch an instance with the control plane's ledger snapshot
  # attached and read the deployment identity and the CA key off it. Asserted as a
  # whole statement, and with the Effect, because the same statement rendered Allow
  # would bound nothing while every substring search still passed.
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.node.policy).Statement :
      try(
        s.Sid == "BilletRuntimeDenyForeignSnapshot" &&
        s.Effect == "Deny" &&
        tolist(s.Action) == tolist(["ec2:RunInstances"]) &&
        tolist(s.Resource) == tolist(["arn:aws:ec2:*:*:snapshot/*"]) &&
        s.Condition.Null["aws:ResourceTag/sh.billet.owner"] == "true",
        false
      )
    ])
    error_message = "the node policy must DENY ec2:RunInstances for a snapshot carrying no billet owner tag, in the real partition"
  }

  # A bucket containing a region string survives intact (the case F4 caught: a
  # global region replace after the bucket replace would corrupt it).
  assert {
    condition     = strcontains(aws_iam_role_policy.node.policy, "acme-cache-us-east-1")
    error_message = "a bucket name containing a region string must not be rewritten"
  }

  # The node role's COMMERCIAL trust document — the China run asserts the
  # aws-cn half, and either alone would pass a suffix hard-coded for the other
  # partition (the same pincer the router role keeps below).
  assert {
    condition = jsondecode(aws_iam_role.node.assume_role_policy) == {
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Action    = "sts:AssumeRole"
        Principal = { Service = "ec2.amazonaws.com" }
      }]
    }
    error_message = "the node trust policy must be exactly one Allow of sts:AssumeRole to the partition's EC2 service principal"
  }

  # The role's NAME is an existing deployment's IAM identity: a rename forces a
  # replacement on every one of them.
  assert {
    condition     = aws_iam_role.node.name == "billet-test-node"
    error_message = "the node role must be named from var.name"
  }
}

# Prove the FULL China substitution path, including the KMS-ARN-first ordering and
# the dns-suffix sentinel, by overriding the partition/region data sources and the
# (post-apply) KMS key ARN to known values. A commercial plan cannot exercise this
# because data.aws_partition resolves to "aws".
run "china_kms_substitutes_partition_region_and_suffix" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id                        = "vpc-0f0f0f0f0f0f0f0f0"
    name                          = "billet-test"
    enable_cache                  = true
    enable_kms                    = true
    enable_spot                   = true
    cache_bucket                  = "acme-cache-cn"
    cache_prefix                  = "team-a"
    job_instance_profile_role_arn = "arn:aws-cn:iam::123456789012:role/job-instances"
  }

  override_data {
    target = data.aws_partition.this
    values = {
      partition  = "aws-cn"
      dns_suffix = "amazonaws.com.cn"
    }
  }
  override_data {
    target = data.aws_region.this
    values = { region = "cn-north-1" }
  }
  override_resource {
    target          = aws_kms_key.cache[0]
    override_during = plan
    values          = { arn = "arn:aws-cn:kms:cn-north-1:123456789012:key/abcd-1234" }
  }

  # The whole KMS-ARN sentinel was replaced first, so the real China key ARN lands
  # intact (no leftover TFKMSKEY, no partition corruption).
  assert {
    condition     = strcontains(aws_iam_role_policy.node.policy, "arn:aws-cn:kms:cn-north-1:123456789012:key/abcd-1234")
    error_message = "the KMS key ARN was not substituted for the China deployment"
  }

  # The kms:ViaService carries the partition's dns suffix — hard-coding
  # amazonaws.com here would DENY every KMS operation in China.
  assert {
    condition     = strcontains(aws_iam_role_policy.node.policy, "ec2.cn-north-1.amazonaws.com.cn")
    error_message = "the kms:ViaService must use the partition dns suffix (amazonaws.com.cn)"
  }

  # Partition rewritten into the EC2/S3 ARNs, and NO sentinel survives.
  assert {
    condition     = strcontains(aws_iam_role_policy.node.policy, "arn:aws-cn:ec2:")
    error_message = "the partition sentinel was not rewritten to aws-cn"
  }
  assert {
    condition     = !can(regex("TF(REGION|BUCKET|PREFIX|KMSKEY|PARTITION|DNSSUFFIX)", aws_iam_role_policy.node.policy))
    error_message = "a committed policy sentinel survived the China render"
  }

  # The trust policies are where a hard-coded amazonaws.com is a fully broken
  # deployment: no China EC2 instance could assume the node role, and no China
  # Lambda could assume the router role. jsonencode in module code keeps both
  # observable under the mock, asserted as complete documents.
  assert {
    condition = jsondecode(aws_iam_role.node.assume_role_policy) == {
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Action    = "sts:AssumeRole"
        Principal = { Service = "ec2.amazonaws.com.cn" }
      }]
    }
    error_message = "the node trust policy must follow the partition dns suffix"
  }
  assert {
    condition = jsondecode(aws_iam_role.spot_router[0].assume_role_policy) == {
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Action    = "sts:AssumeRole"
        Principal = { Service = "lambda.amazonaws.com.cn" }
      }]
    }
    error_message = "the router trust policy must follow the partition dns suffix"
  }

  # PassedToService must follow the partition too — asserted here because the
  # commercial pass_role run cannot distinguish a hard-coded amazonaws.com
  # from a substituted one.
  assert {
    condition     = jsondecode(aws_iam_role_policy.pass_role[0].policy).Statement[0].Condition.StringEquals["iam:PassedToService"] == "ec2.amazonaws.com.cn"
    error_message = "the PassRole condition must follow the partition dns suffix"
  }
}

run "kms_path_plans" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id       = "vpc-0f0f0f0f0f0f0f0f0"
    name         = "billet-test"
    enable_cache = true
    enable_kms   = true
  }

  # The KMS-enabled node policy comes from node-policy.json and the key ARN is
  # known only after apply, so the rendered document can't be asserted at plan —
  # the drift test pins that file and the substitution is shared. What this proves
  # is that the KMS branch PLANS: the customer key is created and no index error
  # (the enable_kms-without-cache class) is hit.
  assert {
    condition     = length(aws_kms_key.cache) == 1
    error_message = "enable_kms with enable_cache should create the customer key"
  }
}

run "compute_only_role_has_no_cache_or_spot_grants" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id       = "vpc-0f0f0f0f0f0f0f0f0"
    name         = "billet-test"
    enable_cache = false
    enable_spot  = false
  }

  # A cacheless, non-spot node's role carries ONLY the ec2 runtime — no S3, no EBS
  # cache, no SQS — proving the feature-gated rendering, not the full superset.
  assert {
    condition     = !strcontains(aws_iam_role_policy.node.policy, "s3:") && !strcontains(aws_iam_role_policy.node.policy, "sqs:") && !strcontains(aws_iam_role_policy.node.policy, "BilletCache")
    error_message = "a compute-only role must not carry cache or spot grants"
  }

  # THE COMPUTE-ONLY RENDERING CARRIES A PARTITION SENTINEL TOO, since the launch
  # boundary names a snapshot ARN — and this run is the only one that would see it
  # unsubstituted, because every other assertion here is about the cache rendering.
  assert {
    condition     = !can(regex("TF(REGION|BUCKET|PREFIX|KMSKEY|PARTITION|DNSSUFFIX)", aws_iam_role_policy.node.policy))
    error_message = "a committed policy sentinel survived the compute-only render"
  }

  # And a role with no cache still refuses a foreign snapshot: the exposure is the
  # bare ec2:RunInstances grant, which every ec2 node holds.
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.node.policy).Statement :
      try(
        s.Sid == "BilletRuntimeDenyForeignSnapshot" &&
        s.Effect == "Deny" &&
        tolist(s.Resource) == tolist(["arn:aws:ec2:*:*:snapshot/*"]),
        false
      )
    ])
    error_message = "a compute-only role must still deny a launch from a snapshot it does not own"
  }
  assert {
    condition     = length(aws_iam_role_policy.spot) == 0
    error_message = "no spot policy without enable_spot"
  }
}

# The spot grants as COMPLETE documents. Both embed post-apply ARNs, so the
# queue and log group are overridden to known values during plan — the same
# machinery the China run uses for the KMS key — which makes the jsonencoded
# documents fully concrete and exactly assertable. The actions come from the
# committed policy/spot-actions.json that internal/tfpolicy pins to
# ec2.SpotIAMActions().
run "spot_grants_are_scoped_to_the_created_queue" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id      = "vpc-0f0f0f0f0f0f0f0f0"
    name        = "billet-test"
    enable_spot = true
  }

  override_resource {
    target          = aws_sqs_queue.interruptions[0]
    override_during = plan
    values          = { arn = "arn:aws:sqs:us-east-1:123456789012:billet-test-interruptions" }
  }
  override_resource {
    target          = aws_cloudwatch_log_group.spot_router[0]
    override_during = plan
    values          = { arn = "arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/billet-test-spot-router" }
  }

  # The node's grant: the actions come FROM the committed file here too, which
  # closes the chain generator → committed file → rendered module policy. A
  # literal list would let the module regress to hard-coded actions and stay
  # green after the file moves on.
  assert {
    condition = jsondecode(aws_iam_role_policy.spot[0].policy) == {
      Version = "2012-10-17"
      Statement = [{
        Sid      = "BilletSpotInterruptions"
        Effect   = "Allow"
        Action   = jsondecode(file("${path.root}/policy/spot-actions.json"))
        Resource = "arn:aws:sqs:us-east-1:123456789012:billet-test-interruptions"
      }]
    }
    error_message = "the node's spot grant must be exactly the committed spot-actions.json on exactly the created queue"
  }

  # The router's grant: describe (unscopable), forward to exactly the queue,
  # logs scoped to the declared log group's ARN plus ":*" — and nothing else.
  # (That the declared group IS the one Lambda writes to is a name coupling
  # asserted in spot_creates_the_interruption_router, where nothing overrides
  # the group.)
  assert {
    condition = jsondecode(aws_iam_role_policy.spot_router[0].policy) == {
      Version = "2012-10-17"
      Statement = [
        {
          Sid      = "ReadInstanceNodeTag"
          Effect   = "Allow"
          Action   = "ec2:DescribeInstances"
          Resource = "*"
        },
        {
          Sid      = "ForwardToNodeQueue"
          Effect   = "Allow"
          Action   = ["sqs:GetQueueUrl", "sqs:SendMessage"]
          Resource = "arn:aws:sqs:us-east-1:123456789012:billet-test-interruptions"
        },
        {
          Sid      = "Logs"
          Effect   = "Allow"
          Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
          Resource = "arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/billet-test-spot-router:*"
        },
      ]
    }
    error_message = "the router grant must be exactly describe, queue-scoped forward, and own-log-group writes"
  }
}

run "spot_creates_the_queue" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id      = "vpc-0f0f0f0f0f0f0f0f0"
    name        = "billet-test"
    enable_spot = true
  }

  assert {
    condition     = length(aws_sqs_queue.interruptions) == 1
    error_message = "enable_spot should create the interruption queue"
  }
}

run "spot_creates_the_interruption_router" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  override_resource {
    target          = aws_lambda_function.spot_router[0]
    override_during = plan
    values          = { arn = "arn:aws:lambda:us-east-1:123456789012:function:override-sentinel" }
  }

  variables {
    vpc_id      = "vpc-0f0f0f0f0f0f0f0f0"
    name        = "billet-test"
    enable_spot = true
  }

  # The whole router — Lambda, rule, target, invoke permission, role, log group —
  # exists only with spot. (The router's complete IAM document is asserted in
  # spot_grants_are_scoped_to_the_created_queue, where the post-apply ARNs are
  # overridden; spot_router_test.py covers the handler.)
  assert {
    condition     = length(aws_lambda_function.spot_router) == 1 && length(aws_cloudwatch_event_rule.spot_interruption) == 1 && length(aws_cloudwatch_event_target.spot_router) == 1 && length(aws_lambda_permission.spot_router) == 1
    error_message = "enable_spot should create the router Lambda, rule, target and invoke permission"
  }
  assert {
    condition     = aws_lambda_function.spot_router[0].handler == "spot_router.handler"
    error_message = "the router Lambda must use the committed handler"
  }
  # The commercial half of the router trust policy — the China run asserts the
  # aws-cn document, and either alone would pass a hard-coded suffix for the
  # other partition.
  assert {
    condition = jsondecode(aws_iam_role.spot_router[0].assume_role_policy) == {
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Action    = "sts:AssumeRole"
        Principal = { Service = "lambda.amazonaws.com" }
      }]
    }
    error_message = "the router trust policy must be exactly one Allow of sts:AssumeRole to the partition's Lambda service principal"
  }
  # The EventBridge target routes THIS rule to the router Lambda (not a stray rule).
  assert {
    condition     = aws_cloudwatch_event_target.spot_router[0].rule == aws_cloudwatch_event_rule.spot_interruption[0].name
    error_message = "the rule must target the router Lambda"
  }
  # ...and its target arn is READ FROM the router resource, not reconstructed:
  # the Lambda's arn is overridden to a sentinel no string-building could
  # derive from the function name, so a target that rebuilt the arn — or
  # pointed at any other function — fails rather than silently losing every
  # warning.
  assert {
    condition     = aws_cloudwatch_event_target.spot_router[0].arn == aws_lambda_function.spot_router[0].arn && aws_lambda_function.spot_router[0].arn == "arn:aws:lambda:us-east-1:123456789012:function:override-sentinel"
    error_message = "the EventBridge target must read the router Lambda's own arn"
  }
  # The rule matches EXACTLY the Spot interruption warning from aws.ec2 — decoded
  # and compared field-for-field, not a substring, so dropping the source filter or
  # broadening the detail-type is caught.
  assert {
    condition     = length(jsondecode(aws_cloudwatch_event_rule.spot_interruption[0].event_pattern).source) == 1 && jsondecode(aws_cloudwatch_event_rule.spot_interruption[0].event_pattern).source[0] == "aws.ec2"
    error_message = "the rule must match exactly source aws.ec2"
  }
  assert {
    condition     = length(jsondecode(aws_cloudwatch_event_rule.spot_interruption[0].event_pattern)["detail-type"]) == 1 && jsondecode(aws_cloudwatch_event_rule.spot_interruption[0].event_pattern)["detail-type"][0] == "EC2 Spot Instance Interruption Warning"
    error_message = "the rule must match exactly the Spot interruption warning detail-type"
  }
  # EXACTLY those two keys — no extra narrowing that could exclude real warnings.
  assert {
    condition     = length(keys(jsondecode(aws_cloudwatch_event_rule.spot_interruption[0].event_pattern))) == 2
    error_message = "the event pattern must carry only source and detail-type"
  }
  # The declared log group must be the group Lambda actually writes to —
  # /aws/lambda/<function name> is the name Lambda derives, not a convention
  # billet chose. If these drift, Lambda auto-creates an unmanaged group the
  # router's policy does not cover and every log line is lost while the
  # policy-content run above stays green (it overrides the group's ARN).
  assert {
    condition     = aws_cloudwatch_log_group.spot_router[0].name == "/aws/lambda/${aws_lambda_function.spot_router[0].function_name}"
    error_message = "the declared log group must be the /aws/lambda/<function> group Lambda writes to"
  }

  # Only EventBridge may invoke the Lambda — exact principal, not a substring,
  # which "events.attacker.example" would satisfy. The mocked partition suffix
  # is amazonaws.com; the China run covers the suffix substitution machinery.
  assert {
    condition     = aws_lambda_permission.spot_router[0].principal == "events.amazonaws.com"
    error_message = "only EventBridge (events.amazonaws.com) may be permitted to invoke the router"
  }
}

run "no_spot_router_without_spot" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id      = "vpc-0f0f0f0f0f0f0f0f0"
    name        = "billet-test"
    enable_spot = false
  }

  # EVERY spot-gated resource is absent — not just the Lambda and rule, but the
  # role, log group, target and invoke permission too.
  assert {
    condition     = length(aws_lambda_function.spot_router) == 0 && length(aws_cloudwatch_event_rule.spot_interruption) == 0 && length(aws_cloudwatch_event_target.spot_router) == 0 && length(aws_lambda_permission.spot_router) == 0 && length(aws_iam_role.spot_router) == 0 && length(aws_iam_role_policy.spot_router) == 0 && length(aws_cloudwatch_log_group.spot_router) == 0
    error_message = "a non-spot deployment must not create any router resource"
  }

  assert {
    condition     = length(aws_sqs_queue.interruptions) == 0
    error_message = "no interruption queue without enable_spot"
  }
}

# iam:PassRole is granted only for the exact configured job role, passed only
# to EC2 — both rendered by jsonencode, so both are exact-assertable here.
run "pass_role_is_scoped_to_the_exact_job_role" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id                        = "vpc-0f0f0f0f0f0f0f0f0"
    name                          = "billet-test"
    job_instance_profile_role_arn = "arn:aws:iam::123456789012:role/job-instances"
  }

  # The complete document: a partial assertion would pass an Effect flip, a
  # dropped condition, or an appended statement.
  assert {
    condition = jsondecode(aws_iam_role_policy.pass_role[0].policy) == {
      Version = "2012-10-17"
      Statement = [{
        Sid      = "BilletPassJobRole"
        Effect   = "Allow"
        Action   = "iam:PassRole"
        Resource = "arn:aws:iam::123456789012:role/job-instances"
        Condition = {
          StringEquals = { "iam:PassedToService" = "ec2.amazonaws.com" }
        }
      }]
    }
    error_message = "pass_role must be exactly one Allow of iam:PassRole on the configured role, passed only to EC2"
  }
}

run "no_pass_role_without_a_job_role" {
  command = plan

  module {
    source = "./modules/fleet-ec2"
  }

  variables {
    vpc_id = "vpc-0f0f0f0f0f0f0f0f0"
    name   = "billet-test"
  }

  assert {
    condition     = length(aws_iam_role_policy.pass_role) == 0
    error_message = "no PassRole grant may exist without a configured job role"
  }
}

