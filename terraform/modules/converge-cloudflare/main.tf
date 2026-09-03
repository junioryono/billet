# Reaching Billet hosts from CI through a Cloudflare Tunnel.
#
# OPTIONAL, AND NOT PART OF THE billet ROOT MODULE. Billet needs no inbound
# connectivity to run jobs: nodes always dial outbound. This exists so a
# deployment can converge hosts from CI without opening a port, and it is the
# route to prefer over Systems Manager when there is no reason to be in AWS: it
# needs no S3 bucket, so a converge's secrets -- including the GitHub App private
# key the host role installs -- never transit object storage.
#
# WHAT IT COSTS. A domain on Cloudflare is a hard prerequisite, each host runs
# another daemon, and Cloudflare sits in the path between CI and the hosts.

locals {
  # 32 random bytes, base64-encoded, which is Cloudflare's stated minimum.
  # Generated here rather than asked for, so an operator is never tempted to
  # reuse something they already have.
  tunnel_name = "${var.name}-${replace(var.hostname, ".", "-")}"
}

resource "random_bytes" "tunnel_secret" {
  length = 32
}

resource "cloudflare_zero_trust_tunnel_cloudflared" "converge" {
  account_id    = var.account_id
  name          = local.tunnel_name
  tunnel_secret = random_bytes.tunnel_secret.base64
}

# THE HOSTNAME IS A CNAME TO THE TUNNEL, PROXIED. Unproxied would publish the
# tunnel's address directly and skip Access entirely, which is the whole control
# on this path.
resource "cloudflare_dns_record" "converge" {
  zone_id = var.zone_id
  name    = var.hostname
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.converge.id}.cfargotunnel.com"
  proxied = true
  ttl     = 1
}

# ACCESS IS WHAT MAKES THE TUNNEL USABLE RATHER THAN INERT. An application with no
# policy is deny-by-default, so the hostname is not reachable -- the failure of an
# unattached policy is that nothing gets in, including CI, not that everyone does.
# THE TOKEN IS DERIVED, NOT AN ATTRIBUTE OF THE TUNNEL. Provider v5 exposes it
# through its own data source; reading it here keeps the credential cloudflared
# needs in one place rather than asking an operator to assemble it.
data "cloudflare_zero_trust_tunnel_cloudflared_token" "converge" {
  account_id = var.account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.converge.id
}

resource "cloudflare_zero_trust_access_application" "converge" {
  account_id       = var.account_id
  name             = local.tunnel_name
  domain           = var.hostname
  type             = "self_hosted"
  session_duration = var.session_duration

  # The converge is a machine-to-machine flow; a browser identity has no place in
  # it, and leaving the default on would let a human session reach the host too.
  allowed_idps              = []
  auto_redirect_to_identity = false

  # ATTACHED, NOT MERELY CREATED. A reusable account policy that no application
  # references protects nothing. Access is deny-by-default so the omission failed
  # CLOSED -- CI simply could not connect -- but the comment and the README both
  # claimed the host was protected by it, which is the worse half of the mistake.
  policies = [{
    id         = cloudflare_zero_trust_access_policy.ci.id
    precedence = 1
  }]
}

# WITHOUT THIS, A CONNECTED cloudflared PUBLISHES NOTHING. The tunnel and the DNS
# record give the hostname somewhere to resolve; this is what maps it to the SSH
# service on the host. The catch-all is required: an ingress list whose last rule
# is not a bare service is rejected.
resource "cloudflare_zero_trust_tunnel_cloudflared_config" "converge" {
  account_id = var.account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.converge.id

  config = {
    ingress = [
      {
        hostname = var.hostname
        service  = "ssh://localhost:22"
      },
      {
        service = "http_status:404"
      },
    ]
  }
}

resource "cloudflare_zero_trust_access_service_token" "ci" {
  account_id = var.account_id
  name       = "${local.tunnel_name}-ci"
}

# SERVICE TOKEN ONLY. The one credential that opens this application is the one CI
# holds, and it is rotated by replacing this resource.
resource "cloudflare_zero_trust_access_policy" "ci" {
  account_id = var.account_id
  name       = "${local.tunnel_name}-ci"
  decision   = "non_identity"

  # ON THE POLICY, NOT ONLY THE APPLICATION. A reusable policy carries its own
  # duration and defaults to 24 hours in provider v5, so setting it on the
  # application alone left var.session_duration describing something that was not
  # in force.
  session_duration = var.session_duration

  include = [{
    service_token = {
      token_id = cloudflare_zero_trust_access_service_token.ci.id
    }
  }]
}
