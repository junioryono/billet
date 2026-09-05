# billet

[![CI](https://github.com/junioryono/billet/actions/workflows/ci.yml/badge.svg)](https://github.com/junioryono/billet/actions/workflows/ci.yml) [![Coverage](https://codecov.io/gh/junioryono/billet/branch/main/graph/badge.svg)](https://codecov.io/gh/junioryono/billet) [![Go Reference](https://pkg.go.dev/badge/github.com/junioryono/billet.svg)](https://pkg.go.dev/github.com/junioryono/billet) [![Docs](https://img.shields.io/badge/docs-billet.readthedocs.io-2980B9)](https://billet.readthedocs.io/en/latest/) [![License: Apache-2.0](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)

**Self-hosted GitHub Actions runners on your own hardware, with the cloud as fallback and a cache beside the compute.**

A box under your desk is the cheapest fast CI you will ever have, and the reason people do not rely on it is that houses lose power. billet is built so that one `runs-on` label can mean "the machine at home if it is up, EC2 if it is not". The control plane talks to GitHub over an outbound long-poll, so GitHub never connects to you; a single-box deployment opens nothing at all. One static binary, no hosted component, Apache-2.0.

> **Pre-alpha.** Jobs run end to end through every backend against real GitHub and AWS, but do not point release or deploy pipelines at it yet. [What is proven](https://billet.readthedocs.io/en/latest/reference/status.html) says exactly what has run for real, by backend, with dates.

## Contents

- [Why billet](#why-billet)
- [Quick start](#quick-start)
- [Deployment shapes](#deployment-shapes)
- [Backends](#backends)
- [How it works](#how-it-works)
- [Security](#security)
- [Documentation](#documentation)
- [Contributing](#contributing)

## Why billet

- **One label, several machines, in your order of preference.** A tier can name `[firecracker, ec2]`, and the control plane picks the host when the job is admitted: the box at home first, the cloud when it is not there. Failover under one unchanged label has completed against real GitHub and AWS.
- **A kernel per job on your own hardware.** Firecracker microVMs under the jailer on Linux, tart VMs on an Apple Silicon Mac, one EC2 instance or CodeBuild build per job in AWS. Docker is there for trials.
- **A cache beside the compute.** Guest images, sticky disks, BuildKit state, the Docker image store and, on Linux, `actions/cache` itself, served from a Ceph pool or EBS at the same site instead of crossing the internet.
- **Nothing hosted.** No SaaS, no webhook endpoint, no public IP, no tunnel. Nodes dial the control plane; the control plane dials GitHub; that is all.

## Quick start

One Linux machine with Docker, a GitHub organization you own, and about fifteen minutes. (A personal account is served one repository at a time, with `--repository owner/name`, on a backend that admits untrusted work; the docs say why.)

```bash
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh

billet init --org your-org --runner-group billet-trusted \
  --workflow 'your-org/your-repo/.github/workflows/ci.yml@refs/heads/main' --config ~/billet.yaml
billet github-app create --org your-org --config ~/billet.yaml   # creates and installs the App
billet check --config ~/billet.yaml                              # proves the credential and the config

billet server --config ~/billet.yaml   # the control plane; and in a second terminal:
billet node   --config ~/billet.yaml   # the machine that runs jobs
```

Then, in a workflow:

```yaml
jobs:
  build:
    runs-on: billet-2vcpu
```

The runner group must be workflow-restricted before `init`, and the App is created in a browser. [Get started](https://billet.readthedocs.io/en/latest/getting-started/index.html) walks every step, including the one that costs an hour.

## Deployment shapes

| Goal | Control plane | Compute | Guide |
|---|---|---|---|
| Try it on one computer | `billet server` and SQLite on it | Docker on the same machine, trusted workflows only | [Single host with Docker](https://billet.readthedocs.io/en/latest/deploying/single-host-docker.html) |
| One Linux server, real isolation, a persistent cache | on the server | Firecracker guests and a Ceph pool on its disks, converged by Ansible | [A Linux Firecracker host](https://billet.readthedocs.io/en/latest/deploying/linux-firecracker-host.html) |
| macOS or arm64 Linux jobs on a Mac you own | local, on the Mac, or in AWS | tart VMs on Apple Silicon | [Run jobs on a Mac](https://billet.readthedocs.io/en/latest/deploying/mac-tart.html) |
| Everything in AWS | a small EC2 instance, SQLite on EBS | one EC2 instance per job | [AWS with EC2](https://billet.readthedocs.io/en/latest/deploying/aws-ec2.html) |
| Managed macOS in AWS | in AWS | CodeBuild on a reserved Apple Silicon fleet | [AWS with CodeBuild](https://billet.readthedocs.io/en/latest/deploying/aws-codebuild.html) |
| Owned hardware first, the cloud when it is not there | a small EC2 instance | local Firecracker, then EC2, under one label | [Hybrid](https://billet.readthedocs.io/en/latest/deploying/hybrid-owned-hardware.html) |
| Controller failover | active and passive controllers on PostgreSQL | any fleet | [PostgreSQL and active-passive](https://billet.readthedocs.io/en/latest/deploying/postgres-and-active-passive.html) |

[Choose a shape](https://billet.readthedocs.io/en/latest/deploying/choose-a-shape.html) compares them by failure domain.

## Backends

| Backend | Runs on | Boundary | Fork pull requests |
|---|---|---|---|
| `firecracker` | Linux with KVM and Ceph | a microVM under the jailer: its own kernel, chroot, uid, cgroup, seccomp | with a separate untrusted bridge |
| `tart` | Apple Silicon | a VM under Apple's hypervisor; macOS or arm64 Linux guests | with softnet isolation |
| `ec2` | AWS | one instance per job, destroyed with it | with a separate security group |
| `codebuild` | AWS | one build per job on managed hosts, Linux or Apple Silicon | refused |
| `docker` | anywhere with Docker | a container sharing the host kernel | refused |

Storage is a property of the site: a Ceph pool for Firecracker hosts, EBS snapshots with an S3 pointer for EC2. The ledger is SQLite on local disk by default, or PostgreSQL when the controller should be rebuildable rather than restored.

## How it works

```text
                 GitHub  ◄── outbound long-poll only; nothing inbound
                    ▲
              ┌─────┴──────┐
              │   server   │   scale-set listeners · capacity ledger · scheduler
              └─────┬──────┘
                    │  nodes dial OUT over mTLS (plain HTTP on loopback)
        ┌───────────┼──────────────┬───────────────┐
        ▼           ▼              ▼               ▼
   ┌──────────┐ ┌──────────┐ ┌───────────┐ ┌────────────┐
   │bare metal│ │ Apple Si │ │    EC2    │ │  CodeBuild │
   │firecracker│ │   tart   │ │ per job   │ │  per job   │
   └──────────┘ └──────────┘ └───────────┘ └────────────┘
```

One binary, two roles. `billet server` creates one GitHub runner scale set per tier, reserves capacity in its ledger before it advertises a slot, accepts a job, chooses the machine, and tells that machine's node to launch. `billet node` runs one provider, polls for commands, and hands each guest a single-use runner registration off argv. When GitHub reports completion the node destroys the compute and the capacity comes back, and only then. A control plane that restarts re-adopts the jobs already running; nothing billet does on its own destroys a running job. [How billet works](https://billet.readthedocs.io/en/latest/concepts/how-it-works.html) and [Tiers and capacity](https://billet.readthedocs.io/en/latest/concepts/tiers-and-capacity.html) go deeper.

## Security

billet is not a sandbox for untrusted code, and says so. Trust belongs to a runner pool, not to the event that scaled it up: every tier is untrusted unless promoted, and a trusted tier requires a workflow-restricted runner group that billet re-checks against GitHub before every registration it mints. Fork pull requests get arbitrary code execution on your hardware and are admitted only on a backend that is a real boundary with a network of its own; [GitHub's own guidance](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access) against self-hosted runners on public repositories applies. The App billet creates holds two permissions (metadata read, organization self-hosted runners read and write; for a repository target, metadata read and repository administration read and write, the only grant GitHub offers for the job) and cannot read your code. One deployment can serve several organizations and repositories, each through its own App. The control plane runs unprivileged; the Linux node runs as root because Firecracker and the Docker socket require it, and the jailer drops every VM to its own uid. Caches are a deliberate cross-job channel, so do not cache secrets. Read [Trust and isolation](https://billet.readthedocs.io/en/latest/concepts/trust-and-isolation.html) before pointing billet at anything. To report a vulnerability, email the maintainer rather than opening an issue.

## Documentation

Everything is at [billet.readthedocs.io](https://billet.readthedocs.io/en/latest/).

- **[Get started](https://billet.readthedocs.io/en/latest/getting-started/index.html)**: from nothing to a job in fifteen minutes.
- **[Concepts](https://billet.readthedocs.io/en/latest/concepts/how-it-works.html)**: how it works, tiers and capacity, trust and isolation, sites and storage, identity and security, state and controllers.
- **[Deploying](https://billet.readthedocs.io/en/latest/deploying/choose-a-shape.html)**: choose a shape; a single host; a Firecracker host; a Mac; AWS with EC2 or CodeBuild; hybrid; PostgreSQL and active-passive controllers; reaching your hosts.
- **[Operating](https://billet.readthedocs.io/en/latest/operating/status-and-leases.html)**: status and leases, nodes, guest images, the Actions cache, upgrades (automatic by default, on Linux, macOS and PostgreSQL controllers alike, and unable to go backwards by accident), backup and recovery, draining and stopping, CA rotation, troubleshooting.
- **[Reference](https://billet.readthedocs.io/en/latest/reference/cli.html)**: every command and every configuration key, what is proven, the reference hardware, the [architecture decisions](https://billet.readthedocs.io/en/latest/reference/decisions/index.html) and the [acceptance records](https://billet.readthedocs.io/en/latest/reference/records/index.html).

Terraform modules live under [`terraform/modules`](terraform/modules/billet/README.md), the Ansible collection under [`ansible_collections/junioryono/billet`](ansible_collections/junioryono/billet/README.md), and the published Actions under [`actions/`](actions).

## Alternatives

Use one of these if it fits. [actions-runner-controller](https://github.com/actions/actions-runner-controller) is GitHub's own and needs Kubernetes. [terraform-aws-github-runner](https://github.com/github-aws-runners/terraform-aws-github-runner) is AWS-only. [GARM](https://github.com/cloudbase/garm) is the closest open-source multi-provider control plane, without a cache. [Ubicloud](https://github.com/ubicloud/ubicloud) is the best open reference for how a commercial runner cloud is built, under AGPL. [Actuated](https://actuated.com), [Blacksmith](https://blacksmith.sh), [Namespace](https://namespace.so), [WarpBuild](https://warpbuild.com) and [Depot](https://depot.dev) are managed products. billet is one `runs-on` label spanning bare metal, Apple Silicon and the cloud with failover between them, plus a colocated cache, in one Apache-2.0 binary with no Kubernetes; every piece of that exists somewhere in that list and the combination does not. billet borrows several of Blacksmith's published designs and says so.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). `make check` is the gate; the skills under `.claude/skills` are the project's written-down rules.

## License

Apache-2.0. See [LICENSE](LICENSE). Apple Silicon support requires [Tart](https://tart.run), which is licensed FSL-1.1-ALv2 and is an external dependency you install yourself, like Docker or Ceph.
