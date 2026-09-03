output "tunnel_id" {
  description = "The tunnel id."
  value       = cloudflare_zero_trust_tunnel_cloudflared.converge.id
}

output "tunnel_token" {
  description = "The credential cloudflared runs with on the host. Install it out of band with `cloudflared service install <token>` -- the billet host role does not manage cloudflared."
  value       = data.cloudflare_zero_trust_tunnel_cloudflared_token.converge.token
  sensitive   = true
}

output "hostname" {
  description = "The hostname CI connects to."
  value       = var.hostname
}

output "service_token_client_id" {
  description = "The Access service-token client id for CI."
  value       = cloudflare_zero_trust_access_service_token.ci.client_id
  sensitive   = true
}

output "service_token_client_secret" {
  description = "The Access service-token client secret for CI. Shown once by Cloudflare and kept in Terraform state; treat the state file accordingly."
  value       = cloudflare_zero_trust_access_service_token.ci.client_secret
  sensitive   = true
}
