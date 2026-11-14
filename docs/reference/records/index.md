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
job-logs
job-attach
assignment-protocol
guest-release-removal
job-reruns
cpu-placement
image-store-wait
docker-published-ports
untrusted-fork-tart
untrusted-fork-ec2
ec2-cache-probes
idle-control-plane
runtime-predictions
replay-policies
target-weights
egress-allowlist
bare-metal-hosts
pool-release-energy
job-statistics
cache-breaker
docker-builder
```

| Record | Covers |
|---|---|
| [AWS acceptance](aws-acceptance.md) | cold end-to-end jobs on EC2 from a private repository, the `billet acceptance` command that turned the hand procedure into one, spot interruption, same-label failover, and what the CodeBuild API actually does |
| [EC2 cache probes](ec2-cache-probes.md) | what chose where an EC2 node keeps a cache's bytes: a store volume per archive against whole objects in S3 against GitHub's own store, from one host in one VPC |
| [Site acceptance](site-acceptance.md) | the site boundary proved on real hosts: three nodes, two sites, one deployment, and the two defects only a real AWS could show |
| [Restore rehearsal](restore-rehearsal.md) | why the two CI rehearsal legs exist, what each proves that the other cannot, and what neither proves |
| [Host rehearsals](host-rehearsals.md) | the rollout, recover, CA rotation and promotion rehearsals on packaged hosts under real systemd, what the first run found, and what has and has not run |
| [Shared uplink](shared-uplink.md) | what a day of failing fetches from the reference deployment's guests turned out to be: a mirror outage measured from two networks (the site and AWS), a compute host behind a residential gateway, and what billet changed so a job depends on less of either |
| [Cache parity](cache-parity.md) | what each cache costs without billet on the reference deployment, what git and GitHub measurably do that the Git proxy's design rests on, and what is still to be measured once the caches are deployed |
| [The stale rollout that downgraded a control plane](stale-rollout-downgrade.md) | the day a real control plane was moved backwards by its own updater: the three defects that had to line up, what each guard should have said, and what an operator does to a host left that way |
| [The ledger's write budget](ledger-write-budget.md) | what a renewal, a purchase and a placement cost against the ledger's one writer slot on both engines, on a laptop and then at the target fleet on server hardware with SQLite and a PostgreSQL on its own machine, each on network block storage and on NVMe; why a listener renews its leases in one commit a pass, and how placement stopped growing with the fleet squared |
| [Workspace snapshots](workspace-snapshots.md) | what Firecracker did when one warm guest was snapshotted and restored many times: millisecond restores, per-fork memory, VMGenID driver binding, differing kernel random samples and duplicated userspace RNG state, MMDS and logging order, clone networking, vsock and the clock across a snapshot, and the memory file under the jailer |
| [Queue wait on a shared node](shared-node-queue-wait.md) | why an organization's jobs waited up to 55 minutes on 2026-10-16: a node full by memory at 120 of its 180 vCPU, a workload that tripled beside billet's own CI, the admission order and each tier's own backlog at GitHub, and what the cold lint runs and the uplink showed |
| [CI capacity](ci-capacity.md) | what a pull request's CI reserves on the fleet it shares, what each host-lifecycle group uses measured from its own `/proc`, the move of every group to 2 vCPU and what it frees, and the options for a shorter Go verdict compared on those numbers (#237) |
| [Job logs](job-logs.md) | what the runner writes for a job's log and when it goes, whether a line is masked before it is written, and what the index measured |
| [Attaching to a job](job-attach.md) | a terminal in a real microVM during a job and after its runner exited (2026-11-05), and a failed job's VM held, attached to, released, kept across a restart and destroyed 15.4 s after its bound, through the whole stack (2026-11-07); and the same through the real `billet server` and real GitHub jobs on SQLite and PostgreSQL, including a drain that waited for a hold and one that cut it, and, on #794's branch, two restarts whose held VMs were destroyed about 24 s after their bounds while GitHub still refused the new session (2026-11-07) |
| [What GitHub.com does with an assignment](assignment-protocol.md) | the protocol facts the steady advertisement rests on, measured 2026-10-21: how long an unrun assignment is kept (cancelled twice after about five minutes, then held at least 80 minutes), what the advertised ceiling counts, that lowering it to zero did not revoke fresh assignments early, how fast capacity is used, what a cancellation looks like, what a new session restores, and how many sessions one installation holds |
| [Removing the guest releases published before #584](guest-release-removal.md) | the 19 `guest-*` releases removed on 2026-10-22 because they carried components billet may not redistribute and no corresponding source, the release that replaced them, and what that does to a deployment |
| [CPU placement and idle power](cpu-placement.md) | what pinning microVMs to host cpus (`compact`, `spread`, `packed`) and capping the cpu while idle did to run time and power on the reference node on 2026-10-23, in three drained sessions with the wall meter beside RAPL: compact slower than spread on every load spread could hold, but using less energy on a memory-bound load at half the threads; sharing a CCD's L3 costing little and idle CCDs showing no energy saving; no cpu ceiling showing an idle saving; boost off saving energy under compact and costing it under spread; `node.power` restoring a launch's ceiling in about 20 ms; and billet's energy attribution at a mean 0.89 of the package's energy above idle, the shortfall tracking the host's own CPU |
| [How long a guest waits for its Docker image store](image-store-wait.md) | what the reference deployment's ledger and node journal showed on 2026-10-24 about a guest's wait for its image store before its runner starts: no runner handed a job anywhere near the old 780-second bound, the slow ones all stores whose acquisition ran into Ceph's trouble at the three-minute bound and were still handed their jobs, and the four-minute bound on the whole request that the guest now waits just past, with a failed attach kept in custody until the compute is gone (#320) |
| [Docker's published ports, with and without the userland proxy](docker-published-ports.md) | what turning Docker's userland proxy off did on a billet EC2 guest on 2026-10-25: the published port nearly as fast as the container's own address (select-only up 53%), and `localhost` broken for Node 18 and `[::1]` for every client, so billet's images keep the proxy, which GitHub's runner image template also leaves at Docker's default (#163) |
| [An untrusted fork pull request on Tart](untrusted-fork-tart.md) | a fork's pull request run twice on an untrusted Tart tier on a development Mac on 2026-10-25 (a Linux arm64 guest): softnet with `@host` kept it from the LAN's router and another LAN host, both of which answered the Mac, while the internet worked; a VM booted with billet's flags reached neither of the Mac's own addresses where a listener answered a VM on tart's NAT, no cache and no billet authority reached it, and the outside evidence agreed; plus the two defects only the real run found, an isolated launch that did not wait for the guest agent and a cleanup that killed `tart delete` part-way (#23) |
| [An untrusted fork pull request on EC2](untrusted-fork-ec2.md) | a fork's pull request run on an untrusted EC2 tier in the CI account on 2026-10-25: the untrusted security group and no instance profile seen from inside the job and from outside it, no interception, and the pool's own image store published by default-branch jobs, read by the fork and never published from it, proved by a tag each job wrote; and on 2026-11-04, on an AMI built from GitHub's template, the same proof for sticky disks: read warm by the fork, its own key cold again on its re-run, and what it wrote to the default branch's disk absent from that re-run and from the next default-branch job (#23) |
| [What an idle control plane costs](idle-control-plane.md) | fifty idle control planes on one host, measured 2026-11-04: memory, CPU, connections to PostgreSQL and what a limit does to them, and what a restart waits for on real GitHub |
| [Runtime predictions](runtime-predictions.md) | how well a job's runtime is predicted from the deployment's own history, measured on the reference deployment's fortnight to 2026-10-25: 30,253 jobs graded as an hourly-refreshing controller would have predicted them, 78% within 2x and 99.4% from the job's own runs, the wide shapes best (84 to 96% within 2x), the 2 vCPU tiers close in seconds and loose in ratio, and what the record does not grade |
| [Admission orders on replay](replay-policies.md) | `fair`, `fill` and `hold` replayed through billet's real scheduler: on a whole-host shape beside smaller ones over four seeds, `hold` beating `fair` on every smaller shape (8 vCPU p99 40m to 1h38m against 2h26m to 6h37m) and `fill` on the widest (49m to 1h03m against 1h46m to 3h27m); on the reference deployment's own week of 19,757 jobs, `hold` cutting the 2 and 4 vCPU p99 to about 12 minutes and leaving the 16 vCPU p99 at 1h43m, between `fair`'s 11m and `fill`'s 3h59m. |
| [Weights between targets](target-weights.md) | `targets[].weight` replayed through billet's real scheduler on 2026-11-04: two saturating targets at 2:1 holding 64% to 66% of the contested vCPU (1.83:1 on 16 slots, 1.94:1 on 64, the gap the size of a lease), about 1:1 with no weight, an idle target's rival holding the whole fleet, and the two starvations a replay of equal shapes could not show |
| [Admission orders on replay](replay-policies.md) | `fair` against `fill` replayed through billet's real scheduler: on a whole-host shape beside smaller ones, `fill` starving the 32 vCPU jobs (p99 1h46m against 28m) and `fair` making every smaller shape wait behind them (8 vCPU p99 5h12m against 19m), and on the reference deployment's own week of 19,757 jobs, `fill` making the 16 vCPU jobs wait (p99 3h59m against 11m) and `fair` the 8 vCPU ones (p99 2h30m against 56m). |
| [The egress allowlist](egress-allowlist.md) | a tier's allowlist in a real Firecracker guest: an allowed name connects, any other name, a hard-coded address, an outside resolver and IPv6 are refused and recorded by the host; and the kernel's behaviour behind each rule, including the frame a bridge's hooks never see, on kernels 7.0 and 6.8 |
| [The pool's release, priced in idle energy](pool-release-energy.md) | a synthetic work week replayed through the pool's real scaler on 2026-11-14, under the replay's release assumptions: releasing a host idle past `idle_after` removed 94 to 98% of the idle host-hours a kept pool pays for (about 42 to 68 kWh a week at the reference EPYC's 149.9 W idle draw), at a p99 queue wait about two to three minutes longer with a 3-minute lead time and about one lead time longer with a 15-minute one; 20 minutes the value to start from |
| [Bare-metal hosts](bare-metal-hosts.md) | what rented bare metal measured on 2026-11-05: four AWS metal types from the launch call to a running script in 21 to 47 s, which of them fit a 4 GiB per vCPU pool with local NVMe, their NVMe throughput and single-thread speed alone and under full load; and phoenixNAP's catalogue, stock per location, a new account's one-server quota and the order its API refused (#553) |
| [Reading the ledger back](job-statistics.md) | `billet jobs stats`, `billet jobs rightsize` and `billet migrate workflows` against the reference deployment's week and GitHub's record of it: 78 of billet's own jobs agreeing with GitHub, what a re-run of failed jobs records under each attempt, why a recommendation reads the job's own window and its VM's memory rather than the usage summary, two jobs recomputed by hand, and GitHub's hosted labels on 2026-11-03 |
| [A store that stops answering](cache-breaker.md) | the store breaker's deadlines, and a real Ceph site with its cache index's lock held, its cache pool's OSDs stopped and its monitors out of reach |
| [What a Docker build's cache saves](docker-builder.md) | the persistent builder against a registry cache, `type=gha` and none, for four projects and four kinds of change on 2026-11-05; two Dockerfiles on one key; attach time against the state's size; and what the Docker image store saves a job using a 6 GB image (#542) |
