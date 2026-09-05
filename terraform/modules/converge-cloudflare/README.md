# converge-cloudflare

Reach Billet hosts from CI through a Cloudflare Tunnel, so a converge needs no inbound port.

**This is optional.** Billet needs no inbound connectivity to run jobs — nodes always dial outbound to the control plane. This module is about *configuration management*. See [Reaching your hosts to converge them](../../../docs/deploying/reaching-hosts.md) for the alternatives.

## Why this over Systems Manager

No S3 bucket, so **a converge's secrets never transit object storage** — including the GitHub App private key the host role installs. That is a materially simpler secret story than [converge-aws-ssm](../converge-aws-ssm), where the connection plugin requires a bucket that files pass through even for modules that send none.

## What it costs

- **A domain on Cloudflare is a hard prerequisite.** The tunnel is addressed by hostname; there is no way around this.
- Each host runs `cloudflared` as another daemon.
- Cloudflare sits in the path between CI and your infrastructure.

## Usage

```hcl
module "converge" {
  source = "github.com/junioryono/billet//terraform/modules/converge-cloudflare?ref=v0.9.0"

  account_id = var.cloudflare_account_id
  zone_id    = var.cloudflare_zone_id
  hostname   = "billet-host-1.example.com"
}
```

It creates the tunnel, its ingress mapping the hostname to `ssh://localhost:22`, a proxied DNS route, an Access application, and a service token for CI. The policy is attached to the application directly rather than left as a free-standing resource: Access is deny-by-default, so an unattached policy fails closed -- nothing reaches the host, CI included -- and the module would be inert while appearing configured.

The policy is `non_identity` with a service token — a converge is machine-to-machine, and no browser identity should reach the host.

## On the host

`cloudflared` runs with the `tunnel_token` output. **The Billet host role does not manage this** — install and run `cloudflared` out of band:

```bash
sudo cloudflared service install "$TUNNEL_TOKEN"
```

The tunnel's ingress is configured by this module, so a connected `cloudflared` publishes `ssh://localhost:22` at the hostname without further configuration on the host.

## In CI

The workflow connects over SSH with `cloudflared` as a `ProxyCommand`, presenting the service token:

```
ProxyCommand cloudflared access ssh --hostname %h \
  --service-token-id $CF_ACCESS_CLIENT_ID \
  --service-token-secret $CF_ACCESS_CLIENT_SECRET
```

Both halves of the token are module outputs and both are sensitive. Cloudflare shows a service-token secret once; it is kept in Terraform state, so treat the state file accordingly.

## Outputs

| Output | |
|---|---|
| `tunnel_id`, `tunnel_token` | The tunnel and the credential `cloudflared` runs with. |
| `hostname` | What CI connects to. |
| `service_token_client_id`, `service_token_client_secret` | The Access credential CI presents. |

## What this does not do

It does not install `cloudflared` on your hosts and it does not write the workflow. It provisions the Cloudflare side.
