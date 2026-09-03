# Reaching your hosts to converge them

**billet needs no inbound connectivity to work.** Nodes always dial out to the control plane, and the server dials out only to GitHub, so a node behind NAT with no forwarded ports is the ordinary case. Nothing on this page is required to run jobs.

This page is about configuration management: how the machine running `ansible-playbook` reaches the hosts it converges. That is a separate question from how billet schedules work.

| Route | Prerequisites | Third party | Best for |
|---|---|---|---|
| [A workstation on the same network](#route-a-a-workstation-on-the-same-network) | none | none | one or two hosts you can already reach; the default |
| [AWS Systems Manager](#route-b-aws-systems-manager) | an AWS account, an S3 bucket, a hybrid activation | AWS | fleets already administered from AWS |
| [Cloudflare Tunnel](#route-c-cloudflare-tunnel) | a Cloudflare account and a domain on it | Cloudflare | hosts across several sites, with no ports opened |

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
  source = "github.com/junioryono/billet//terraform/modules/converge-aws-ssm?ref=main"

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
  source = "github.com/junioryono/billet//terraform/modules/converge-cloudflare?ref=main"

  account_id = var.cloudflare_account_id
  zone_id    = var.cloudflare_zone_id
  hostname   = "billet-host-1.example.com"
}
```

It creates the tunnel, its ingress mapping the hostname to SSH, its DNS route, an Access application with the CI policy, and a service token. The host side, `cloudflared` and the tunnel credential, is installed out of band; the host role does not manage it.

## What is deliberately not here

A self-hosted mesh (Headscale, WireGuard) is a reasonable choice for a fleet across several sites and is not documented as a getting-started route: it means running another control plane, and for one or two hosts a deploy runner on Route A achieves the same thing with nothing new to operate. None of these is a requirement; billet runs jobs without them.
