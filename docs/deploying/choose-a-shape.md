# Choose a shape

## Start with the failure domains

A billet installation has three independent parts, and the first decision is which of them are allowed to fail together.

| Part | What it does | What must survive |
|---|---|---|
| Control plane | `billet server` polls GitHub, schedules jobs and owns the capacity ledger | the ledger, the deployment identity, the node-wire CA and the GitHub App credential |
| Compute fleet | one or more `billet node` processes launch jobs through Docker, Firecracker, tart, EC2 or CodeBuild | running jobs, and enough replacement capacity for your availability target |
| Site storage | Firecracker nodes share Ceph images and caches; EC2 nodes use EBS and S3 | cache data only if you choose to keep it; the ledger stays authoritative |

A compute node is not a backup control plane, and a database backup does not provide compute when the local server is offline. Plan control-plane recovery and compute fallback separately.

## The shapes

| Shape | Control plane and state | Compute | Best for | Guide |
|---|---|---|---|---|
| Single host, Docker | `billet server` and SQLite on one machine | Docker on the same machine, trusted workflows only | trying billet; a small team's private repositories | [Single host with Docker](single-host-docker.md) |
| Single Linux host, Firecracker | server and SQLite on the server | Firecracker guests and a Ceph pool on that server's disks, converged by Ansible | one owned server with real kernel isolation and a persistent cache | [A Linux Firecracker host](linux-firecracker-host.md) |
| Owned Mac | local, on the Mac, or in AWS | tart VMs on an Apple Silicon Mac: macOS and native arm64 Linux | Xcode jobs; arm64 Docker builds; anything a managed macOS service's duration limit would cut | [Run jobs on a Mac](mac-tart.md) |
| AWS only | a small EC2 controller with SQLite on encrypted EBS | one EC2 instance per job | Linux CI without an owned server | [AWS with EC2](aws-ec2.md) |
| Managed macOS in AWS | an AWS controller | CodeBuild on a reserved `MAC_ARM` fleet | Xcode jobs without owning Macs; every job inherits CodeBuild's 36-hour ceiling | [AWS with CodeBuild](aws-codebuild.md) |
| Hybrid | a small EC2 controller | local Firecracker first, EC2 second, under one label | owned hardware without making local power or connectivity the CI availability boundary | [Hybrid](hybrid-owned-hardware.md) |
| Two controllers | active and passive controllers on a PostgreSQL ledger, identity shared through Parameter Store | any fleet | controller failover across hosts or availability zones | [PostgreSQL and active-passive](postgres-and-active-passive.md) |

The simplest shape is not automatically the most available one. A single computer is the right default for evaluation and local use. Once CI availability matters, separating the controller from the local compute host removes the local site's power, ISP and hardware from the scheduling path.

## The three layers

Terraform creates cloud resources and returns narrow outputs; Ansible converges existing machines over SSH; billet owns live jobs, leases, enrollment and drains.

| Tool | Owns | Does not own |
|---|---|---|
| Terraform | AWS instances, EBS, VPC and subnets, security groups, IAM, KMS, S3, DNS, monitoring, CodeBuild projects, RDS | live jobs, leases, forced teardown decisions, fingerprint approval |
| Ansible | packages, users, files, systemd units, Firecracker, Ceph client or bootstrap, bridges, the billet configuration, validation, host upgrades | buying cloud resources, or deciding whether a running job may be destroyed |
| billet | GitHub polling, capacity escrow, placement, node identity, the runner lifecycle, custody, drains | the operating system, the VPC, the disks, general host configuration |

There is no billet Terraform provider, on purpose: a provider would imply ownership of tiers and nodes, billet has no configuration API, and a node is a self-registering runtime identity rather than a remote object ([ADR-004](../reference/decisions/adr-004-terraform-provider.md)). Terraform must never use resource deletion, `remote-exec` or a timeout as implicit authorisation to kill running work; the CodeBuild module refuses a destroy under a live build for that reason.

**All three layers name the same version.** A release carries the binary, the Ansible collection and the Terraform module, so `billet_version`, `requirements.yml` and every module `?ref=` should read the same `vX.Y.Z`. A moving target makes a converge non-deterministic and drives a real drain on a day nobody chose, which is why the host role refuses `billet_version: latest`.

## What every shape shares

- The GitHub side is identical: a workflow-restricted runner group, an App billet creates, `billet check` before anything starts ([The GitHub side](../getting-started/github-side.md)).
- Nodes register dynamically; adding one needs no control-plane restart. Tiers and node policy are read at startup.
- Fork pull requests need a backend that is a real boundary and a network of its own ([Trust and isolation](../concepts/trust-and-isolation.md)).
- Rehearse controller recovery before calling a deployment recoverable ([Backup, restore and recover](../operating/backup-restore-recover.md)).
