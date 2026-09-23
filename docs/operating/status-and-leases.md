# Status and leases

Every operator command reaches the ledger without taking the exclusive lock the control plane holds, so all of these work against a live deployment. A command that only reads never waits for the write lock, so `status` and `leases` answer immediately however busy the deployment is.

## `billet status`

```text
admission open
capacity  2 of 8 vCPU, 8GiB of 32GiB, 1 open leases
tier      billet-2vcpu
          discovery 1, pending 0, launching 0, running 0, cleanup 0, unknown 0
          reserved floor 0, additional headroom 0
          advertisement last confirmed 1, sent 1, exchange confirmed (observed 2026-09-13T12:00:00.000000000Z)
held      none
```

In order: whether admission is open or sealed, and by whom; any force-destroy in progress; the rollout line, if one is running; which controller holds the deployment and its fencing epoch; capacity used against the deployment ceiling and the open leases; each tier's capacity breakdown, grouped by GitHub target; the deployment-wide cost peak across cloud nodes; and the node inventory, protocol and compute-proof reports.

The tier counts are leases: **discovery** is escrow the listener still holds, including backing awaiting withdrawal; **pending** is an acquisition commitment or an assigned lease that has not entered launch; **launching** is compute being started; **running** is online or busy compute; **cleanup** is custody, teardown or quarantine; **unknown** is a capacity-phase lease absent from the listener's last ownership observation. The phase alone cannot separate discovery from pending, because acquiring a job does not change a capacity lease's ledger phase. Counts use only leases still open in the ledger, so an old listener observation cannot resurrect a released lease.

**Reserved floor** is the configured operator guarantee, not an extra set of leases to add to those counts. **Additional headroom** is what the allocator could grant beyond every existing charge and protected floor. In the example, the node contributes only 2 vCPU and 8GiB, so the discovery hold leaves zero additional headroom even though the deployment ceiling is larger. `0 additional headroom` does not mean zero advertised capacity: every tier advertises up to what the configured ceiling could run of its shape, whatever is free at the moment, and an assignment that finds no room waits for it. `held none` means no compute-cleanup holds, not no discovery holds.

The advertisement is the listener's **last confirmed** completed exchange and its latest **sent** value, with the exchange state and observation time. During a lower-capacity poll, the sent value can be below the last confirmed value while backing remains charged. A failed exchange is **ambiguous** and retains the last confirmed value; the report cannot tell whether GitHub received the attempted value. A missing observation is **unknown**, never zero. These are dated observations, not a live read from GitHub; a stopped controller leaves its last observation behind. A host that is not live is one the control plane has not heard from within its silence window; its compute may still be running and its capacity stays charged.

Every tier advertises at once and holds nothing while idle. A lease is bought when a runner is about to start, so the discovery count is normally zero; escrow bought for an offer or an assignment that did not use it is released after that message is handled. Explicit `reserved` floors still protect their configured room. When the fleet is full, an assigned job waits at GitHub and starts on a later poll once running work has finished and released its capacity.

A tier with work it could not buy room for says so, on a line of its own:

```text
          waiting 3 job(s) for room, oldest since 2026-09-20T11:40:00Z
```

**That line is the only place a queue is visible.** Nothing is reserved for an idle tier, so in the ledger a tier with three queued jobs and a tier nobody has asked for are the same shape: both hold nothing and both advertise their ceiling. The count is what GitHub has assigned beyond what this tier could start, and the timestamp is when it first could not start it — not when the observation was written, so it reads as the age of the oldest wait. It is absent when nothing is waiting, and a control plane older than this field reports nothing rather than a queue of zero.

Which waiting tier takes that room is `server.admission_order`. Under `fair`, the default, it goes to the tier that has waited longest, and no other tier buys until that tier's shape fits: a large shape only ever fits when several small jobs end together, so first-come starves it on a fleet that is never idle. The freed room therefore sits idle while it accumulates, for as long as the longest job already running, provided the waiting tier's listener is there to buy it. A tier stops holding the line as soon as its demand is served or GitHub's count says the work was cancelled, and also while its listener has made no admission progress for three minutes (a refused reconciliation or a finished launch counts; a launch in flight never goes stale). That is the 2026-09-23 stall: the longest waiter's listener sat on a dead connection to GitHub for 18 minutes, and every tier sharing its host declined every assignment with most of the fleet free. Such a waiter keeps its place and holds the line again as soon as it progresses, and the report says so beneath its waiting line:

```text
          waiting 1 job(s) for room, oldest since 2026-09-23T15:48:42Z
          NOT HOLDING THE LINE: no admission progress for 18m2s (last 2026-09-23T15:58:38Z); other tiers may buy ahead of it until it progresses
```

A stalled listener cannot publish that it has stalled, so `billet status` compares the last progress it did publish with its own clock. Under `fill` anything that fits starts instead, and the largest shape may wait indefinitely.

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
