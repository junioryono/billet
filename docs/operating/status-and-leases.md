# Status and leases

Every operator command reaches the ledger without taking the exclusive lock the control plane holds, so all of these work against a live deployment. A command that only reads never waits for the write lock, so `status` and `leases` answer immediately however busy the deployment is.

## `billet status`

```text
admission:  open
controller: host-a (epoch 3)
capacity:   12 of 64 vCPU, 48GiB of 256GiB, 3 open leases
tiers:
  billet-8vcpu-ubuntu-2404   firecracker,ec2   6 available   2 running
  billet-macos-arm64         tart              1 available   1 running
nodes:
  epyc-1    firecracker   home        live   protocol 19   v0.5.2
  mac-1     tart          (no site)   live   protocol 19   v0.5.2
  ec2-usw2  ec2           us-west-2   live   protocol 19   v0.5.2   peak $2.14/h
```

In order: whether admission is open or sealed, and by whom; any force-destroy in progress; the rollout line, if one is running; which controller holds the deployment and its fencing epoch; capacity used against the deployment ceiling and the open leases; each tier's providers, what it advertises and what it is running; each node's provider, site, liveness, negotiated protocol version and release; the deployment-wide cost peak across cloud nodes; and any host whose compute proof is unproven or whose CodeBuild registration path was never swept.

`0 available` on a tier means billet is advertising nothing for it and GitHub has nothing to assign, usually because another tier's reservation holds the capacity. A host that is not live is one the control plane has not heard from within its silence window; its compute may still be running and its capacity stays charged.

## `billet leases`

| Command | Shows |
|---|---|
| `billet leases held` | every lease whose compute is not confirmed gone: running jobs, custody a healthy node is tending, teardowns not yet confirmed, and quarantine, each with its node, phase and age |
| `billet leases quarantined` | only the capacity held for compute nobody has accounted for |
| `billet leases failures --since 24h --limit 50` | jobs GitHub did not report as succeeded on leases billet's own infrastructure had disrupted |
| `billet leases release <lease> --force` | hand the capacity back on your assertion that its compute is gone |

**Custody** is compute billet cannot account for and nothing else in the process is managing: a container that survived a restart and is talking to GitHub on its own, or a launch that failed ambiguously. **Teardown** is a destroy that was asked for and not yet confirmed stopped; on EC2 a terminate request is accepted before the machine stops, so every EC2 teardown spends a minute or two here. **Quarantine** is a lease whose holder stopped heartbeating while something was running behind it; the capacity stays charged to its host, because expiry proves the control plane stopped hearing from something, never that the container stopped.

Quarantine resolves itself in the ordinary case: the host destroys the compute and says so, or reports what it is actually running on its next sweep and the quarantined lease is absent from that report. `billet leases release --force` records your assertion instead. Quarantine is resolved immediately because it has no holder; a live custody holder receives the request through its next heartbeat, drops its local obligation and releases the lease itself. Force, because nothing has confirmed anything: if you are wrong, that slot is sold twice.

## What `leases failures` is and is not

It shows two facts side by side and no verdict between them: GitHub's own result for the job, and what billet's infrastructure was doing at the time (the host stopped answering, the guest went missing from an inventory, the machine was reclaimed by a spot interruption, the job was destroyed under a custody bound). billet cannot tell a broken host from a broken build, and it **does not re-run anything**, because a re-run is a side effect on your repository and a deploy must not happen twice because a machine went away. Every runner in this ecosystem fails-and-stops on a lost runner; what billet owes the person whose build went red is the one thing only billet knows. A restart of billet itself is deliberately not recorded as a disruption, because adopted jobs routinely finish green.

## `billet check`

Validates the config and the state directory, proves the App key signs a JWT GitHub accepts and the App is installed with exactly the requested permissions, checks every trusted tier's runner group, reports each registered node's site and liveness, reports which of a tart node's images are missing and whether softnet is granted, reports the newest backup's age when `backup.s3` is set, reports an EC2 node's conservative cost peak, and refuses a Ceph cluster that would clone the old way. `--authorize` also dry-runs a launch against AWS. It works while the control plane is running, and `billet local up` refuses to start a server until it has passed.

## Restarts

| Action | Control-plane restart? |
|---|---|
| a machine reconnecting | no |
| admitting a new machine | no |
| reclaiming stranded capacity | no |
| adding or changing a tier | yes |
| changing `nodes:` policy | yes |
| changing `sites:` | yes |

A newer CLI against an older running control plane refuses to migrate the schema it is mid-transaction against and tells you which side to restart. A stopped deployment is migrated by whoever opens it first, so upgrade the server binary when you upgrade the CLI ([Upgrades](upgrades.md)).
