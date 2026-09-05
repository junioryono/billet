# THE NODE ROLE the control-plane instance assumes to launch and reap EC2 compute,
# attach cache volumes and read the cache bucket. Its policy is billet's OWN
# generator's output: the module commits a rendering of internal/awspolicy (kept
# equal by internal/tfpolicy's drift test) and substitutes this deployment's
# account, region, cache bucket, prefix, KMS key and interruption queue for the
# sentinel values in it. An operator who wants the exact minimal policy for their
# config passes `billet init iam` output as var.iam_policy_json.
locals {
  # SELECT THE RENDERING BY THE ENABLED FEATURES, so a compute-only or cacheless
  # node's role carries only what it exercises rather than the full superset. The
  # committed files use UPPERCASE sentinels (TFREGION, TFBUCKET, ...), which cannot
  # appear in any real value the module substitutes — region, bucket, prefix and
  # name are all validated lowercase — so every replace is a single unambiguous
  # rewrite with no chance of collision. Spot is NOT in these files; it is a
  # separate grant below, scoped to the created queue, added only when enabled.
  _policy_file = (
    !var.enable_cache ? "${path.module}/policy/node-policy-compute.json" :
    local.enable_cache_kms ? "${path.module}/policy/node-policy-cache-kms.json" :
    "${path.module}/policy/node-policy-cache.json"
  )

  # The KMS-ARN replace is INNERMOST so it matches the whole sentinel ARN (which
  # itself contains TFPARTITION and TFREGION) before those are substituted
  # individually. TFPARTITION and TFDNSSUFFIX come from data.aws_partition, so one
  # committed rendering serves the commercial partition, GovCloud and China alike.
  _rendered_policy = replace(replace(replace(replace(replace(replace(
    file(local._policy_file),
    "arn:TFPARTITION:kms:TFREGION:000000000000:key/TFKMSKEY",
    local.enable_cache_kms ? aws_kms_key.cache[0].arn : ""),
    "TFPARTITION", data.aws_partition.this.partition),
    "TFDNSSUFFIX", data.aws_partition.this.dns_suffix),
    "TFREGION", local.region),
    "TFBUCKET", local.cache_bucket),
  "TFPREFIX", var.cache_prefix)

  node_policy = var.iam_policy_json != "" ? var.iam_policy_json : local._rendered_policy
}

# jsonencode rather than aws_iam_policy_document, deliberately: this document
# is fully known at plan, and rendering it in Terraform (not in the provider)
# keeps it assertable under the mocked test provider. A hard-coded
# amazonaws.com here would render a trust policy no China EC2 instance can
# assume — the test suite pins the suffix substitution.
resource "aws_iam_role" "node" {
  name = "${var.name}-node"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "ec2.${data.aws_partition.this.dns_suffix}" }
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy" "node" {
  name   = "${var.name}-node"
  role   = aws_iam_role.node.id
  policy = local.node_policy
}

# iam:PassRole ONLY for the exact job-instance role, when one is configured.
# jsonencode for the same reason as the trust policy above: plan-known, and
# the PassedToService suffix must follow the partition.
resource "aws_iam_role_policy" "pass_role" {
  count = var.job_instance_profile_role_arn != "" ? 1 : 0

  name = "${var.name}-pass-role"
  role = aws_iam_role.node.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "BilletPassJobRole"
      Effect   = "Allow"
      Action   = "iam:PassRole"
      Resource = var.job_instance_profile_role_arn
      Condition = {
        StringEquals = { "iam:PassedToService" = "ec2.${data.aws_partition.this.dns_suffix}" }
      }
    }]
  })
}

resource "aws_iam_instance_profile" "node" {
  name = "${var.name}-node"
  role = aws_iam_role.node.name
  tags = local.tags
}

# THE AMI BUILDER'S GRANT, as its own document so the node's rendering does not
# change when a deployment turns it on.
#
# BILLET'S OWN GENERATOR'S OUTPUT, exactly as the node policy is: the module
# commits a rendering of internal/awspolicy with NoCompute and Builder set —
# the builder statements and nothing else — kept equal by internal/tfpolicy's
# drift test, and substitutes this deployment's partition and payload bucket for
# the sentinels. The two renderings differ only in whether the installers are
# staged in S3, which is needed once the toolcache declaration outgrows EC2's
# user-data limit.
locals {
  _builder_policy_file = (
    var.builder_payload_bucket == ""
    ? "${path.module}/policy/builder-policy.json"
    : "${path.module}/policy/builder-policy-payload.json"
  )

  builder_policy = replace(replace(
    file(local._builder_policy_file),
    "TFPARTITION", data.aws_partition.this.partition),
  "TFPAYLOADBUCKET", var.builder_payload_bucket)
}

resource "aws_iam_role_policy" "builder" {
  count = var.builder ? 1 : 0

  name   = "${var.name}-builder"
  role   = aws_iam_role.node.id
  policy = local.builder_policy
}

# THE SPOT INTERRUPTION GRANT, scoped to exactly the queues this module creates
# and added only when spot is enabled — kept out of the committed rendering so a
# non-spot node's role does not carry it. Every spot node in the co-located root
# runs under this one role, so the grant covers every queue, from the same list
# the router's grant and environment are derived from. The ACTIONS come from the
# committed policy/spot-actions.json, which internal/tfpolicy byte-compares
# against ec2.SpotIAMActions() — the same source-of-truth pinning as the node
# policy, replacing a regex scrape of this file. jsonencode (not the provider's
# policy-document data source) so the whole document is assertable in the mocked
# plan tests with the queue ARNs overridden to known values.
resource "aws_iam_role_policy" "spot" {
  count = var.enable_spot ? 1 : 0

  name = "${var.name}-spot"
  role = aws_iam_role.node.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "BilletSpotInterruptions"
      Effect   = "Allow"
      Action   = jsondecode(file("${path.module}/policy/spot-actions.json"))
      Resource = local.spot_queue_arns
    }]
  })
}
