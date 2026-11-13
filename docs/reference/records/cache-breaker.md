# A store that stops answering

A site's store that hangs rather than refuses held every launch on that site for three minutes, and each later cache call waited on the same store (#531). Each node now keeps a breaker for its store ([When a store stops answering](../../operating/build-caches.md#when-a-store-stops-answering)). This record is what set its deadlines, and what it did on a real Ceph site on 2026-11-05.

## Where

- **The node.** An EC2 m8i.4xlarge (16 vCPU, 64 GiB) in a test account, Ubuntu 24.04, with nested virtualization, so Firecracker ran inside a VM and its timings are not a bare host's.
- **Ceph.** Single-host Ceph 19.2.3 on four 150 GB gp3 OSDs, `billet-images` on two and `billet-cache` on the other two, each pool keeping two copies.
- **The control plane.** On a second host, reached over an SSH tunnel.
- **Jobs.** One Firecracker tier of 2 vCPU guests. The control plane's ceiling held seven at once. Each job attached the Docker image store, attached a sticky disk, and slept 30 seconds.
- **The binary.** Built from #741's head merged with #739.

## The deadlines

**A volume on a healthy store.**

| Store | What | Samples | Time |
|---|---|---|---|
| Ceph, this host | Docker image store attach (`took`), waves of 1, 3 and 6 concurrent jobs | 10 | 0.8 to 2.2 s |
| Ceph, the reference node, 2026-10-03 | one `rbd device map` through a restart's relaunch of 26 guests (`MapTimeout`'s record) | | 0.21 s idle, 4 to 13 s at a load of 25 to 38, 17.83 s at worst |
| ebs-s3, us-west-2 | `CreateVolume` from a snapshot until available, then attach until in use | 10 | 4.3 to 4.6 s (create 2.6 to 2.7 s, attach 1.7 to 1.9 s) |
| ebs-s3, #680's EC2 runs | the image store's attach (`took`) | | 5.6 to 5.9 s |

**The deadline is 60 seconds on Ceph and 30 on ebs-s3** (`cephStoreDeadline` and `ebsS3StoreDeadline` in `internal/app/nodecache.go`). Ceph's is about three times the worst map the reference node measured under load. A clone is several `rbd` calls besides the map, and this host's idle numbers say little about a busy one. ebs-s3's is about five times EBS's own create and attach. A metal node's ebs-s3 store, which attaches every guest's volumes to its one instance, has 45 seconds (`ebsS3AttachedStoreDeadline`, 2026-11-13, #861): EC2 carries out one instance's attachments one at a time, in order, so an attach can wait behind the one detach the store has in flight (9.6 s to the device on a `z1d.metal`, against 1.7 s alone), on top of the 2.6 to 2.7 s create; about three times that, as Ceph's is. Before the store let only one of its detaches go at a time, an attach behind eight took 60 s, and three acceptance runs in four missed 30 s at a phase's start ([bare-metal-hosts](bare-metal-hosts.md)).

**What this does not settle.** A warm clone on a busy, healthy store that takes longer than 60 seconds now goes cold where it would have waited for three minutes. The slowest warm runner in [How long a guest waits](image-store-wait.md) was handed its job 199 seconds after launch, which includes its boot. The node's `took` values on the reference deployment are what would show how many warm attaches pass 60 seconds; they were not read for this record.

## The cache index's lock held

This is the failure the breaker is for, and the one the reference node had on 2026-10-03: a store whose launches work while its caches do not, because something holds the cache index's lock. The rehearsal takes that lock as a holder that is not finished (`rbd lock add` under a cookie with a current timestamp, which billet waits out rather than breaking for ten minutes). Root disks take no index lock, so they kept cloning. Jobs were queued every 20 seconds.

| UTC | What happened |
|---|---|
| 04:02:43 | The lock is taken. |
| 04:03:54, 04:04:16, 04:04:38 | Three jobs each wait the full deadline (60.1 s) for their image store, go cold, and the breaker opens on the third: `failures=3 reason=timeout last="a clone"`. Each one's sticky disk is answered at once, because its session still has the image store's call out. |
| 04:04:52 | A probe answers (the index is read without its lock), and the breaker goes on probation. |
| 04:05:02 to 04:11:25 | Probe, probation, one job's trial (its image store or its sticky disk) waits the deadline and fails, open again: ten times, about one every 40 seconds. `billet status` showed it each time, either `ceph breaker OPEN for 20s (1 timeout failures); its new jobs start cold` or `on probation for 7s`. |
| 04:11:46 | The lock is released. |
| 04:11:52 | A probe answers. A pending publication takes the trial and succeeds, and the breaker closes: `the site's store is answering again`. |
| 04:12:02 onward | Image stores attach again in 1.6 to 5.1 s, the first behind the queue of waiting publications. |

**What the 24 jobs in those nine minutes recorded**, from the control plane's ledger:

| Image store | Sticky disk | Jobs |
|---|---|---|
| bypassed | bypassed | 10 |
| bypassed | unavailable | 4 |
| bypassed | warm | 1 |
| unavailable | unavailable | 9 |

So 13 of the 24 waited one deadline: the three before the breaker opened, and the ten trials after it, six for an image store and four for a sticky disk. None waited two. The other 11 went on at once. All 24 succeeded, and so did the 12 jobs after the release, every one with a warm sticky disk.

**One trial for each probation is what probation costs.** While a hold lasts, about one job every 40 seconds pays the 60-second deadline. Probing less often the longer a store stays open would bring that down, and it is not built.

## The cache pool's OSDs stopped

**The rehearsal planned to stop the cache pool's two OSDs and watch the breaker. It cannot do that, because each job's root disk is a clone in the cache pool** (`CloneRoot` in `internal/store/ceph/clone.go`), whatever the pool's configuration comment had said. With both OSDs stopped from 03:28:42 to 03:35:33, every launch failed at its root disk (`clone billet-images/ubuntu-2404-x64@… to billet-cache/…: rbd did not answer: context deadline exceeded`). No guest started, so no cache was asked for, and the breaker stayed closed, which is correct. Five jobs' leases recorded `unused`.

- **Capacity.** The failed launches' capacity stayed held until the OSDs came back and their destroys were proved, between 30 s and 6 min 10 s later.
- **Recovery.** Seven jobs started within 30 seconds of the restart.
- **Never counted:** the trash purge and the destroys failed throughout (`list the cache pool's trash: rbd did not answer`), and the breaker did not count them.

A cache-pool outage is therefore a launch outage. The docs and the pool's comment now say so.

## The monitors out of reach

From 04:19:02 to 04:25:26 an nftables rule dropped the node's own connections to the monitors (`socket cgroupv2` on `billet-node.service`, ports 3300 and 6789), so its `rbd` calls could not reach the cluster, while the kernel's client and Ceph's own daemons kept theirs. That is what a node cut off from its site looks like.

- **Launches.** Every one failed before it reached a root disk, at reading which generations are verified (`read the verifications of billet-images/ubuntu-2404-x64: rbd did not answer`), after `rbd`'s own 15-second bound. Nine leases recorded `unused`. No guest started, so the breaker was never asked and stayed closed.
- **Recovery.** A launch already waiting on an `rbd` call finished a second after the rule was removed, and the 14 jobs after it ran with warm sticky disks.

So with no monitors, as with no cache pool, the outage is capacity's and not the breaker's, and the docs say so.

## What this does not establish

- **A real ebs-s3 site.** Only EBS's latency was measured there, not the breaker.
- **A bare host.** Every timing here is under nested virtualization.
- **The metric.** `billet_cache_breaker_open` was not scraped, because the node had no metrics address. The breaker's report reached `billet status` through the ledger.
- **Warm Docker image stores.** The tier was untrusted, so it published no image store, and every attach was a fresh volume. Sticky disks were warm, and those were clones.
- **One thing seen and not fixed here.** After the OSD leg and a restart, deferred sticky-disk publications verified device paths that by then held other images, a guest's root disk among them, and each failed closed until its window ended: #743.
