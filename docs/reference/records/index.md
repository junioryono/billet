# Acceptance records

What ran for real, when, and what it found. These are the evidence behind [What is proven](../status.md); every claim in them names a date, an account or a host.

```{toctree}
:maxdepth: 1

aws-acceptance
site-acceptance
restore-rehearsal
host-rehearsals
stale-rollout-downgrade
shared-uplink
cache-parity
ledger-write-budget
workspace-snapshots
shared-node-queue-wait
assignment-protocol
```

| Record | Covers |
|---|---|
| [AWS acceptance](aws-acceptance.md) | cold end-to-end jobs on EC2 from a private repository, the `billet acceptance` command that turned the hand procedure into one, spot interruption, same-label failover, and what the CodeBuild API actually does |
| [Site acceptance](site-acceptance.md) | the site boundary proved on real hosts: three nodes, two sites, one deployment, and the two defects only a real AWS could show |
| [Restore rehearsal](restore-rehearsal.md) | why the two CI rehearsal legs exist, what each proves that the other cannot, and what neither proves |
| [Host rehearsals](host-rehearsals.md) | the rollout, recover, CA rotation and promotion rehearsals on packaged hosts under real systemd, what the first run found, and what has and has not run |
| [Shared uplink](shared-uplink.md) | what a day of failing fetches from the reference deployment's guests turned out to be: a mirror outage measured from two networks (the site and AWS), a compute host behind a residential gateway, and what billet changed so a job depends on less of either |
| [Cache parity](cache-parity.md) | what each cache costs without billet on the reference deployment, what git and GitHub measurably do that the Git proxy's design rests on, and what is still to be measured once the caches are deployed |
| [The stale rollout that downgraded a control plane](stale-rollout-downgrade.md) | the day a real control plane was moved backwards by its own updater: the three defects that had to line up, what each guard should have said, and what an operator does to a host left that way |
| [The ledger's write budget](ledger-write-budget.md) | what a renewal, a purchase and a placement cost against the ledger's one writer slot on both engines, why billet does not batch heartbeats, and the placement cost that grows with the fleet squared |
| [Workspace snapshots](workspace-snapshots.md) | what Firecracker did when one warm guest was snapshotted and restored many times: millisecond restores, per-fork memory, VMGenID driver binding, differing kernel random samples and duplicated userspace RNG state, MMDS and logging order, clone networking, vsock and the clock across a snapshot, and the memory file under the jailer |
| [Queue wait on a shared node](shared-node-queue-wait.md) | why an organization's jobs waited up to 55 minutes on 2026-10-16: a node full by memory at 120 of its 180 vCPU, a workload that tripled beside billet's own CI, the admission order and each tier's own backlog at GitHub, and what the cold lint runs and the uplink showed |
| [What GitHub.com does with an assignment](assignment-protocol.md) | the protocol facts the steady advertisement rests on, measured 2026-10-21: how long an unrun assignment is kept (cancelled twice after about five minutes, then held at least 80 minutes), what the advertised ceiling counts, that lowering it to zero did not revoke fresh assignments early, how fast capacity is used, what a cancellation looks like, what a new session restores, and how many sessions one installation holds |
