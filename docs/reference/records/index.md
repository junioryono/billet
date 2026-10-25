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
ci-capacity
assignment-protocol
guest-release-removal
cpu-placement
untrusted-fork-tart
untrusted-fork-ec2
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
| [CI capacity](ci-capacity.md) | what a pull request's CI reserves on the fleet it shares, what each host-lifecycle group uses measured from its own `/proc`, the move of every group to 2 vCPU and what it frees, and the options for a shorter Go verdict compared on those numbers (#237) |
| [What GitHub.com does with an assignment](assignment-protocol.md) | the protocol facts the steady advertisement rests on, measured 2026-10-21: how long an unrun assignment is kept (cancelled twice after about five minutes, then held at least 80 minutes), what the advertised ceiling counts, that lowering it to zero did not revoke fresh assignments early, how fast capacity is used, what a cancellation looks like, what a new session restores, and how many sessions one installation holds |
| [Removing the guest releases published before #584](guest-release-removal.md) | the 19 `guest-*` releases removed on 2026-10-22 because they carried components billet may not redistribute and no corresponding source, the release that replaced them, and what that does to a deployment |
| [CPU placement and idle power](cpu-placement.md) | what pinning microVMs to host cpus (`compact`, `spread`, `packed`) and capping the cpu while idle did to run time and power on the reference node on 2026-10-23, in three drained sessions with the wall meter beside RAPL: compact slower than spread on every load spread could hold, but using less energy on a memory-bound load at half the threads; sharing a CCD's L3 costing little and idle CCDs showing no energy saving; no cpu ceiling showing an idle saving; boost off saving energy under compact and costing it under spread; `node.power` restoring a launch's ceiling in about 20 ms; and billet's energy attribution at a mean 0.89 of the package's energy above idle, the shortfall tracking the host's own CPU |
| [An untrusted fork pull request on Tart](untrusted-fork-tart.md) | a fork's pull request run twice on an untrusted Tart tier on a development Mac on 2026-10-25 (a Linux arm64 guest): softnet with `@host` kept it from every address it tried, the Mac's softnet gateway, the LAN's router, another LAN host and the Mac's own LAN address (where a control listener answers on tart's NAT and not under billet's flags), while the internet worked, no cache and no billet authority reached it, and the outside evidence agreed; plus the two defects only the real run found, an isolated launch that did not wait for the guest agent and a cleanup that killed `tart delete` part-way (#23) |
| [An untrusted fork pull request on EC2](untrusted-fork-ec2.md) | a fork's pull request run on an untrusted EC2 tier in the CI account on 2026-10-25: the untrusted security group and no instance profile seen from inside the job and from outside it, no interception, and the pool's own image store published by default-branch jobs, read by the fork and never published from it, proved by a tag each job wrote; plus why sticky disks cannot work on an EC2 guest, one cause fixed and one left open (#23) |
