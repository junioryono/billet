output "node_role_arn" {
  description = "The IAM role billet assumes to launch and reap EC2 compute."
  value       = aws_iam_role.node.arn
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
  description = "The name a SPOT node must use as its node.name (the queue basename); empty when spot is disabled."
  value       = var.enable_spot ? "${var.name}-spot-interruptions" : ""
}
