# How billet works

One binary, two roles, and a wire between them.

```text
                 GitHub  ◄── outbound long-poll only; nothing inbound
                    ▲
              ┌─────┴──────┐
              │   server   │   the control plane: scale-set listeners, capacity ledger, scheduler
              └─────┬──────┘
                    │  nodes dial OUT, over mTLS (plain HTTP on loopback)
        ┌───────────┼──────────────┬───────────────┐
        ▼           ▼              ▼               ▼
   ┌──────────┐ ┌──────────┐ ┌───────────┐ ┌────────────┐
   │bare metal│ │ Apple Si │ │    EC2    │ │  CodeBuild │
   │firecracker│ │   tart   │ │ per job   │ │  per job   │
   └──────────┘ └──────────┘ └───────────┘ └────────────┘
```

## The two roles

**`billet server` is the control plane.** It creates one GitHub runner scale set per tier, opens a message session for each and long-polls it. When GitHub offers a job, the server reserves capacity for it in the ledger, accepts it, chooses a machine, and tells that machine's node to launch. When the job completes, it tells the node to destroy what it launched and releases the capacity. It owns the ledger, the deployment identity, the certificate authority nodes authenticate with, and the GitHub App credential. It runs no compute of its own.

**`billet node` is a compute host.** It dials the control plane, registers what it contributes (vCPU, memory, its site, its provider, its purchasable shapes if it is a cloud orchestrator), and then polls for commands one at a time: launch, destroy, sweep, tend, inventory, upgrade. It runs exactly one provider: `docker`, `firecracker`, `tart`, `ec2` or `codebuild`. For each launch it mints a single-use runner registration through the control plane and hands it to the provider off argv; the guest's runner registers with GitHub, runs the job, and is destroyed with the compute.

A single machine runs both as two processes reading one config file and talking over loopback. There is no combined mode and no flag for one. A fleet is the same thing with the server listening on an address nodes can reach, and every node holding a certificate the control plane issued.

## Why nothing connects inward

GitHub's Runner Scale Set API is a long-poll: billet asks, GitHub answers when it has something. No public IP, no webhook endpoint, no tunnel. A node dials out to the control plane and never listens, so a machine behind NAT on a home connection is the ordinary case rather than a workaround. The one listener a fleet has is the node wire on the control plane, which requires a client certificate in the TLS handshake and serves nothing to a caller without one; the enrollment routes a new machine needs before it has a certificate sit on a separate, optional port that is meant to be closed between enrollments.

## What a job looks like from the inside

1. A workflow says `runs-on: <tier label>`. GitHub queues the job against the scale set with that name.
2. The server's listener for that tier has already escrowed capacity in the ledger, so it advertises a slot. GitHub offers the job; the server acquires it and records the assignment.
3. Placement picks the host: the tier's providers in preference order, then packing (or spreading), then name. The lease is bound to that host and charged against both the host's budget and the deployment ceiling.
4. The node launches: a container, a microVM under the jailer with a copy-on-write root disk, a tart VM, an EC2 instance, or a CodeBuild build. The runner inside registers with GitHub using the single-use JIT configuration and picks up the job.
5. The job runs. The node keeps the lease alive; the control plane keeps polling.
6. GitHub reports completion. The server records GitHub's result, asks the node to destroy the compute, and once the node confirms it is gone, releases the capacity.

Everything authoritative about that sequence lives in the ledger as a lease with a written-down state machine, which is what lets a control plane restart mid-job and re-adopt the compute rather than losing track of it. [Tiers and capacity](tiers-and-capacity.md) explains the ledger; [Trust and isolation](trust-and-isolation.md) explains which backend may run which work.

## What lives where

| Thing | Where |
|---|---|
| The capacity ledger, job history, admission state | `server.state_dir` (SQLite) or a PostgreSQL database |
| The deployment identity and the node-wire CA | `server.identity_dir` (the same directory as the SQLite ledger by default) |
| The GitHub App private key | `github.private_key_path` |
| A node's certificate and state | `node.tls` paths and `node.state_dir` |
| Guest images and caches | the site's store: a Ceph cluster or EBS snapshots with an S3 pointer |
| Compute | wherever the node's provider puts it, labelled with the deployment identity |

The first four together are the deployment; [Backup, restore and recover](../operating/backup-restore-recover.md) treats them as one unit for that reason.
