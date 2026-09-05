# Reaching your hosts to converge them

**billet needs no inbound connectivity to work.** Nodes always dial out to the control plane, and the server dials out only to GitHub, so a node behind NAT with no forwarded ports is the ordinary case. Nothing on this page is required to run jobs.

This page is about configuration management: how the machine running `ansible-playbook` reaches the hosts it converges. That is a separate question from how billet schedules work.

| Route | Prerequisites | Third party | Best for |
|---|---|---|---|
| [A workstation on the same network](#route-a-a-workstation-on-the-same-network) | none | none | one or two hosts you can already reach; the default |
| [AWS Systems Manager](#route-b-aws-systems-manager) | an AWS account, an S3 bucket, a hybrid activation | AWS | fleets already administered from AWS |
| [Cloudflare Tunnel](#route-c-cloudflare-tunnel) | a Cloudflare account and a domain on it | Cloudflare | hosts across several sites, with no ports opened |
| [A WARP client enrolled by service token](#route-d-a-warp-client-enrolled-by-service-token) | a Cloudflare Zero Trust account; hosts already reachable on private addresses in it | Cloudflare | hosts that are privately routable already, with no SSH proxy in the path |

A converge restarts billet's services, which drains the node. **Whatever route you choose, do not drive a converge from a runner billet itself manages**: the drain destroys the jobs on that host, including the one running the playbook, and GitHub does not requeue a job whose runner vanished. The host role refuses this by default; `billet_allow_converge_from_billet_runner` exists for an operator who has established that the runner is backed by some other host.

## Route A: a workstation on the same network

Ansible is agentless, so this needs nothing installed anywhere.

```bash
ansible-galaxy collection install junioryono.billet
ansible-playbook -i inventory.ini site.yml --check --diff   # inspect
ansible-playbook -i inventory.ini site.yml                  # apply
```

Its limits are process properties rather than technical ones: no audit trail of who converged what, no review gate before a change reaches a host, and your machine has to be on. For many deployments they do not matter.

If you want converges in CI without solving reachability, put a small always-on machine on the same network and register it as an ordinary GitHub Actions runner that billet does not manage. It dials out to GitHub, picks up the converge job from inside the network, and reaches the hosts over the LAN.

## Route B: AWS Systems Manager

Ansible connects through SSM instead of SSH, and CI authenticates to AWS rather than holding an SSH key. No inbound ports; the SSM agent dials out.

Read this before choosing it. The `amazon.aws.aws_ssm` connection plugin requires an S3 bucket, and its documentation is explicit that files transit S3 even for modules that send no files, because Ansible ships the module's own code through it, and that secrets in a task's arguments are written into those objects in plaintext. **The host role installs a GitHub App private key, and on this route that key transits the bucket.** Suspended versioning and a short expiry keep it from persisting; versioning left enabled preserves a copy indefinitely.

Because those defaults are easy to get wrong, they are provisioned as a module:

```hcl
module "converge" {
  source = "github.com/junioryono/billet//terraform/modules/converge-aws-ssm?ref=v0.7.0"

  name              = "billet-converge"
  github_repository = "your-org/your-infra-repo"
}
```

It creates the hybrid activation and its IAM role, the transfer bucket with versioning suspended and public access blocked, a policy limited to the operations the plugin performs on this deployment's nodes, and a GitHub OIDC role so CI needs no long-lived keys. The OIDC trust matches an exact subject (`repo:<repo>:ref:refs/heads/main` by default), because a wildcard would admit any pull-request job in the repository and this role reads the bucket the key transits; an environment subject does not close that, since GitHub emits it for any job that references an environment. Your hosts are not EC2 instances, so each registers its SSM agent against the hybrid activation; the [module's README](https://github.com/junioryono/billet/tree/main/terraform/modules/converge-aws-ssm) has the registration command and the inventory variables.

## Route C: Cloudflare Tunnel

`cloudflared` dials out from each host and CI reaches them through Cloudflare Access with a service token. No inbound ports, no S3, and no plaintext-secret path.

You must own a domain on Cloudflare, because the tunnel is addressed by hostname; each host runs another daemon; and Cloudflare sits between CI and your infrastructure.

```hcl
module "converge" {
  source = "github.com/junioryono/billet//terraform/modules/converge-cloudflare?ref=v0.7.0"

  account_id = var.cloudflare_account_id
  zone_id    = var.cloudflare_zone_id
  hostname   = "billet-host-1.example.com"
}
```

It creates the tunnel, its ingress mapping the hostname to SSH, its DNS route, an Access application with the CI policy, and a service token. The host side, `cloudflared` and the tunnel credential, is installed out of band; the host role does not manage it.

This route puts Cloudflare's Access SSH proxy in the path, and on one real hybrid deployment that proxy never engaged: the target, the certificate authority, the identity-provider pin and the hostname were all correct, no certificate was issued, and the Access logs stayed empty. If your hosts are already reachable on private addresses inside the Zero Trust network, Route D needs no proxy to engage.

## Route D: a WARP client enrolled by service token

The CI runner joins your Zero Trust network as a headless **WARP client**, and Ansible connects over **plain SSH** with the fleet's own CI key. No per-host Access application, no SSH proxy, no inbound port, no S3. It is the route that worked first time on a real hybrid deployment (measured 2026-09) whose hosts were already privately routable: a machine at home that is a Mesh node, and a controller in a VPC that a `cloudflared` connector advertises a `/32` route to.

Cloudflare documents the mechanism for headless Linux: `mdm.xml` with `auth_client_id` and `auth_client_secret`, `service_mode: warp` and `auto_connect`, and the enrolled device then authenticates as the shared `non_identity@<team>.cloudflareaccess.com` principal. Two things on the Cloudflare side make it work, and they are provisioned as a module:

```hcl
module "converge" {
  source = "github.com/junioryono/billet//terraform/modules/converge-cloudflare-warp?ref=v0.7.0"

  account_id     = var.cloudflare_account_id
  team_name      = "example"                      # the <team> in <team>.cloudflareaccess.com
  host_addresses = ["100.96.0.14", "10.60.0.10"]  # bare addresses, never /32
  precedence     = 3                              # before your block rule for the private ranges
}
```

It creates the service token, a **Service Auth** policy that lets a device holding it enrol, and one **Gateway L4 allow rule** keyed on that principal admitting exactly those addresses. What it deliberately does not do is touch your WARP enrolment application: that application is the one thing between every employee and their own network, and adopting it into Terraform means pinning every live field, because in provider v5 an omitted optional plans to null on an adopted resource and `policies` is one of them. You attach the module's `enrollment_policy_id` to it yourself, once, and the [module's README](https://github.com/junioryono/billet/tree/main/terraform/modules/converge-cloudflare-warp) shows both ways.

The workflow installs the WARP client from a fingerprint-verified signing key, writes `mdm.xml` with `install -m 0600` from stdin so the secret never lands in argv or a world-readable file, restarts the daemon (the client reads the file once at startup), waits for *Connected*, and **proves each host with a TCP check before trusting the path**, because a device that enrolled and is then denied by Gateway looks identical to a connected one until the first SSH times out. Host keys are committed pins, never an `ssh-keyscan` at job time. At the end it deletes its registration.

Three trade-offs are worth stating. `non_identity@` is a **shared principal**, so the Gateway rule is the entire scope: one rule per fleet, not per token. Gateway **normalises `/32` off** a single address, so a rule written with `/32`s never converges; the module refuses a prefix. And the device profile's **split tunnel must include the destinations**, or the traffic never reaches Gateway and the rule never matters.

## What is deliberately not here

A self-hosted mesh (Headscale, WireGuard) is a reasonable choice for a fleet across several sites and is not documented as a getting-started route: it means running another control plane, and for one or two hosts a deploy runner on Route A achieves the same thing with nothing new to operate. None of these is a requirement; billet runs jobs without them.
