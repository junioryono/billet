output "node_role_arn" {
  description = "The IAM role billet assumes to launch and reap EC2 compute."
  value       = aws_iam_role.node.arn
}

output "node_role_name" {
  description = "The node role's NAME, plan-known, for a composing root to hand to control-plane-ec2-sqlite's instance_profile_role_name so the controller's backup grant lands on the role it actually runs with."
  value       = aws_iam_role.node.name
}

output "instance_profile_name" {
  description = "The instance profile carrying the node role — attach it to whatever EC2 instance runs billet (the opinionated root attaches it to the control plane)."
  value       = aws_iam_instance_profile.node.name
}

output "runner_security_group_id" {
  description = "The trusted-runner security group (node.ec2.security_group_ids)."
  value       = aws_security_group.runner.id
}

output "cache_bucket" {
  description = "The S3 cache bucket name (node.ebs_s3.bucket), or empty when the cache is disabled."
  value       = var.enable_cache ? aws_s3_bucket.cache[0].bucket : ""
}

output "cache_prefix" {
  description = "The cache object prefix (node.ebs_s3.prefix)."
  value       = var.cache_prefix
}

output "cache_kms_key_arn" {
  description = "The customer-managed KMS key ARN for the cache's EBS volumes (node.ebs_s3.kms_key_id), or empty."
  value       = local.enable_cache_kms ? aws_kms_key.cache[0].arn : ""
}

output "interruption_queue_url" {
  description = "The spot interruption queue URL (node.ec2.interruption_queue_url), or empty when spot is disabled."
  value       = var.enable_spot ? aws_sqs_queue.interruptions[0].url : ""
}

output "spot_node_name" {
  description = "The name a SPOT node must use as its node.name (the queue basename); empty when spot is disabled. With spot_node_names, this is the primary queue's node and interruption_queue_urls carries every node."
  # Read from the queue, not rebuilt from var.name: this name is also what the
  # router is told it serves, and three copies of one literal is two that can drift.
  value = var.enable_spot ? aws_sqs_queue.interruptions[0].name : ""
}

output "interruption_queue_urls" {
  description = "Every spot node's interruption queue URL, keyed by the node.name that must consume it (node.ec2.interruption_queue_url): the primary queue under spot_node_name and one per spot_node_names entry. Empty when spot is disabled."
  value       = { for q in local.spot_queues : q.name => q.url }
}

# WHAT THIS MODULE DID, NOT WHAT THE ROLE CAN DO.
#
# It was `builder_granted`, described as whether the role carries the builder's
# grant — which this module cannot know. The supported deployment-scoped shape is
# an `iam_policy_json` override generated with `--builder` and `builder = false`,
# and against that the old output answered "no" while the role held the grant:
# a could-not-tell reported as a no, about a security-relevant capability.
#
# The renamed one states a fact the module owns. Whether an override carries the
# builder's statements is a question for whoever generated it, and reading the
# document to guess is the thing this module deliberately does not do.
output "builder_policy_attached" {
  description = "Whether THIS MODULE attached its own (account-wide) builder policy, which it does when builder = true. It says nothing about an iam_policy_json override: one generated with `billet init iam --deployment <id> --builder --payload-bucket <bucket>` carries the builder's grant while this reports false."
  value       = var.builder
}
