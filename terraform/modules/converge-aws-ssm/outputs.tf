output "activation_id" {
  description = "The SSM activation id a host registers with."
  value       = aws_ssm_activation.hosts.id
}

output "activation_code" {
  description = "The SSM activation code a host registers with. Single-purpose and expiring, and still a registration secret: it is in Terraform state."
  value       = aws_ssm_activation.hosts.activation_code
  sensitive   = true
}

output "transfer_bucket" {
  description = "The S3 bucket the Ansible SSM connection plugin transfers through. Set it as ansible_aws_ssm_bucket_name. Versioning is suspended deliberately: objects here can carry a GitHub App private key."
  value       = aws_s3_bucket.transfer.id
}

output "ci_role_arn" {
  description = "The role a workflow in the configured repository assumes through OIDC."
  value       = aws_iam_role.ci.arn
}

output "transfer_bucket_kms_key" {
  description = "The CMK ALIAS the transfer bucket encrypts with (alias/<name>-transfer), which is what the plugin accepts. Set it as ansible_aws_ssm_bucket_sse_kms_key_id, with ansible_aws_ssm_bucket_sse_mode=aws:kms."
  value       = aws_kms_alias.transfer.name
}
