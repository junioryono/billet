output "service_token_client_id" {
  description = "The service token's client id, for the runner's mdm.xml (auth_client_id). A username rather than a secret, marked sensitive anyway so it is read deliberately."
  value       = cloudflare_zero_trust_access_service_token.ci.client_id
  sensitive   = true
}

output "service_token_client_secret" {
  description = "The service token's client secret, for the runner's mdm.xml (auth_client_secret). Bearer material: shown once by Cloudflare and kept in Terraform state; treat the state file accordingly."
  value       = cloudflare_zero_trust_access_service_token.ci.client_secret
  sensitive   = true
}

output "enrollment_policy_id" {
  description = "The Service Auth policy to attach to the account's WARP enrolment application. This module deliberately does not attach it; the README says how, and why."
  value       = cloudflare_zero_trust_access_policy.enrol.id
}

output "principal" {
  description = "The identity an enrolled runner carries, non_identity@<team>.cloudflareaccess.com. Shared by every service-token device in the account, which is why the Gateway rule is the whole of its reach."
  value       = local.principal
}

output "gateway_policy_id" {
  description = "The Gateway L4 rule that admits the runner to the fleet's addresses."
  value       = cloudflare_zero_trust_gateway_policy.allow.id
}
