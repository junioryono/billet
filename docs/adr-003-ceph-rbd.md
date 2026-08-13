# ADR-003 — Ceph RBD is billet's storage, and ZFS is gone

Status: accepted. Supersedes the ZFS layout in `docs/reference-hardware.md`. Closes [#23](https://github.com/junioryono/billet/issues/23); the `Store` interface, the commit protocol and eviction are [#25](https://github.com/junioryono/billet/issues/25).

## The decision

Every cache in billet's caching plane is a block device that is cloned copy-on-write and mounted: golden images, per-job root disks, sticky disks, the Docker layer cache. Those clones now live in a **Ceph cluster on the nodes' own NVMe**, reached with RBD, and the ZFS pool they used to live in has been destroyed rather than kept beside it.

The reason is one sentence long. **A ZFS clone exists only on the machine that took it**, so a cache written to one belongs to that host and to no other — which is the storage half of billet being a one-machine product. The capacity, placement and teardown halves were fixed by #21, #30 and #31; this is the last one. RBD presents the same drives as a pool any node at the site can map, and it solves golden-image *distribution* for free: a new machine maps the same base image instead of having a copy shipped to it.

Two things were **not** re-litigated here, and this ADR does not reopen them: Ceph replaces ZFS rather than sitting beside it (two snapshot implementations, two garbage collectors and two failure modes to do one job), and the cluster lives at the bare-metal site rather than in the cloud (a cloud cluster serving a machine at home pulls every read across a domestic uplink, which lands below GitHub's own hosted runners and makes the feature worse than not having it).

What follows is what building it actually established. Every number was measured on the reference host; nothing here is taken from documentation that was not then run.

## The cluster, as built

Ubuntu 26.04 LTS, kernel 7.0.0-29-generic, on the reference host in `docs/reference-hardware.md`.

| | |
|---|---|
| Ceph | 20.2.3 Tentacle (`quay.io/ceph/ceph:v20`), deployed by `cephadm` 20.2.0 from the Ubuntu archive |
| Container engine | Docker 29.1.3 — the host already runs it for the `docker` provider |
| Daemons | 1 mon, 2 mgr, 1 crash, 2 OSD, all on `ubuntu-01` |
| OSDs | the two bare Samsung 990 PRO 4TB (`nvme0n1`, `nvme3n1`), 7.3 TiB raw |
| Pools | `billet-images` and `billet-cache`, `size=2 min_size=1`, autoscaled PGs |
| Identity | `client.billet`, `mon 'profile rbd'`, `osd 'profile rbd pool=billet-images, profile rbd pool=billet-cache'` |

The mdraid RAID1 root on `nvme1n1`/`nvme2n1` is untouched and still holds `/`, `/var/lib/billet` and the control-plane database.

```bash
cephadm bootstrap --mon-ip 192.168.1.126 --single-host-defaults \
  --skip-dashboard --skip-monitoring-stack --ssh-user junior
ceph orch daemon add osd ubuntu-01:/dev/nvme0n1
ceph orch daemon add osd ubuntu-01:/dev/nvme3n1
ceph osd pool create billet-images && rbd pool init billet-images
ceph osd pool create billet-cache  && rbd pool init billet-cache
ceph osd set-require-min-compat-client mimic     # see "clone v2", below
ceph auth get-or-create client.billet \
  mon 'profile rbd' \
  osd 'profile rbd pool=billet-images, profile rbd pool=billet-cache'
```

`ceph orch daemon add osd` names each device, deliberately, rather than `ceph orch apply osd --all-available-devices`. The second installs a standing rule that consumes any device that later becomes free — including a disk pulled out of the root mirror to be replaced, at the moment it is least wanted.

## The four questions #23 was opened to answer

### 1. cephadm or packages? — cephadm, with Docker

`cephadm` is the only deployment path Ceph supports going forward, the host already runs containers, and the alternative is hand-managed systemd units for five daemons. The `ceph`/`rbd` **client** still comes from the distro package (`ceph-common`), because `rbd map` is a host operation and billet shells out to the binary.

Two things about that combination cost real time and are worth writing down.

**Ubuntu 26.04's default coreutils is [uutils](https://github.com/uutils/coreutils), and `cephadm bootstrap` fails on it.** Bootstrap runs `install -d -m0770 -o 167 -g 167 /var/run/ceph/<fsid>`, where 167 is the `ceph` uid *inside the container image*. GNU `install` accepts a numeric owner that has no passwd entry; uutils' `install` answers `invalid user: '167'` and bootstrap dies partway through, deleting the half-built cluster on its way out. `chown 167:167` on the same host works, which is what makes it confusing. The fix is a host account for that uid:

```bash
groupadd --system --gid 167 ceph-daemon
useradd  --system --uid 167 --gid 167 --home-dir /var/lib/ceph \
         --no-create-home --shell /usr/sbin/nologin ceph-daemon
```

That is one host account, not a PATH override, because cephadm re-runs the same command on every daemon start. (GNU coreutils is installed alongside as `gnu-coreutils`, with every binary prefixed `gnu`: `/usr/bin/gnuinstall`, `/usr/bin/gnudd`. Reach for those when a tool disagrees with its documentation — `dd iflag=direct` is another one that does.)

**cephadm drives every host over SSH as root, and this host has `PermitRootLogin no`.** Enabling root login to satisfy an orchestrator is the wrong trade; `--ssh-user <user>` with passwordless sudo is supported and leaves the hardening in place. The user chosen already had passwordless sudo, so nothing new was granted.

Neither of these is a Ceph bug, and neither is findable by reading. They are the reason this section exists.

### 2. One pool with namespaces, or two pools? — two pools, and the ZFS reason for it does not apply

The ZFS layout used `tank/images` at a 128K recordsize and `tank/cache` at 1M, and that difference is what made two datasets necessary. **It does not carry over: RBD's object size is a per-IMAGE property**, set at creation and free to differ inside one pool. Measured:

```
rbd create billet-cache/objsize-1m   --size 1G --object-size 1M    -> order 20 (1 MiB objects)
rbd create billet-cache/objsize-128k --size 1G --object-size 128K  -> order 17 (128 KiB objects)
```

So the layout was decided on blast radius instead, and two pools won on three grounds a namespace does not cover. A quota and a PG autoscaler are per-pool. "Throw the cache away" stays a single operation that cannot touch a golden image. And a garbage collector walking the cache pool has no way to reach the images even by mistake, because the pool it can enumerate does not contain them.

`config.CheckCeph` refuses one name for both, for the third reason: a golden image is rebuilt from a recipe and a cache is disposable, and eviction must not be able to confuse them.

### 3. Replication on a single host — `size=1` is not the only option, and the question's premise was wrong

#23 assumed a single-host cluster forces `size=1`, and called that a real data-loss window for golden images. It does not. **CRUSH's failure domain is configurable, and `cephadm bootstrap --single-host-defaults` sets it to the OSD** rather than the host:

```
osd_crush_chooseleaf_type = 0
crush rule steps: take default -> choose firstn 0 type osd -> emit
```

With one OSD per NVMe, `size=2` therefore places the two replicas on **two different drives** — which is exactly the redundancy the ZFS mirror it replaces provided, on the same two disks. `min_size=1` keeps the pool writable when one of them dies, which is again what a degraded ZFS mirror does.

What a single host still does not provide is redundancy against losing the *host*: the mon is a quorum of one, and a machine that is off is a site that is down. That is a real limitation and it is unchanged by any of this — but it is not the data-loss window the question described, and golden images do not need to live anywhere else pending a second machine.

### 4. `krbd` or `rbd-nbd`? — krbd, and it maps the default feature set

The kernel client is faster and is what Firecracker needs anyway: a `/dev/rbdN` is a block device a VMM can attach, while `rbd-nbd` inserts a userspace daemon in the data path for every job. The stated risk was that krbd couples the host kernel to the cluster's feature set, so it was measured rather than assumed. On kernel 7.0.0-29 against Tentacle:

- The features an image gets by default — `layering, exclusive-lock, object-map, fast-diff, deep-flatten` — **all map**, with no `rbd feature disable` dance.
- `journaling` does **not**: enabling it makes `rbd device map` fail with `(6) No such device or address`. Nothing billet does needs it, and this is the shape of failure to expect if a future feature is turned on cluster-wide.

The coupling is therefore real but narrow, and it belongs in the upgrade checklist rather than in the design.

## Clone v2, which turned out to matter more than the map

`rbd snap create` → `snap protect` → `clone` → `map` is the recipe in Ceph's own docs and in #23. **On a cluster as `cephadm` leaves it, the protect step is mandatory — and it makes eviction impossible.** Measured, in that order:

```
$ rbd clone billet-images/ubuntu-2404-x64@g1 billet-cache/job-1
librbd::image::CloneRequest: validate_parent: parent snapshot must be protected
$ rbd snap unprotect billet-images/ubuntu-2404-x64@g1     # with one clone live
rbd: unprotecting snap failed: (16) Device or resource busy
$ rbd snap rm billet-images/ubuntu-2404-x64@g1
rbd: snapshot 'g1' is protected from removal.
```

A cache generation that any running job holds a clone of therefore **cannot be deleted**, and a garbage collector would be blocked by ordinary traffic rather than by anything wrong. That is a design-breaking property, and it is invisible until you try to delete something.

The cause is that `cephadm bootstrap` leaves `require-min-compat-client = luminous`, and `rbd_default_clone_format = auto` reads that as "use clone v1". Setting the floor to `mimic` switches it, and the behaviour changes completely:

```
$ ceph osd set-require-min-compat-client mimic
$ rbd clone billet-images/ubuntu-2404-x64@g2 billet-cache/job-v2   # unprotected
$ rbd snap rm billet-images/ubuntu-2404-x64@g2                     # with the clone live
Removing snap: 100% complete...done.
$ cat /mnt/job-v2/marker
generation 1
```

The parent snapshot is removed on request, the clone keeps reading it correctly, and the space is reclaimed when the last child goes. **Clone v2 is a requirement of billet's storage, not a preference**, and #25's eviction rests on it. `mimic` is the minimum that enables it; anything newer would exclude clients for no gain.

## What it costs, measured

A 10 GiB golden image, `client.billet`, kernel client, ten jobs in a row:

| step | time |
|---|---|
| `rbd clone` | 87–96 ms |
| `rbd device map` | 125–142 ms |
| `mount` | 126–168 ms |
| **clone + map + mount** | **341–397 ms** |
| unmount + unmap + `rbd rm`, per job | ~275 ms |

Ten live clones of that 10 GiB image cost **26 MiB** in the cache pool, which is the copy-on-write claim holding up.

Throughput on a mapped clone, `fio`, O_DIRECT, against `/dev/rbd0` — with the mdraid NVMe root measured the same way for reference:

| workload | RBD | root NVMe |
|---|---|---|
| sequential read, 4M, qd1 | 364 MB/s | |
| sequential read, 4M, qd32 | **4.5 GB/s** | 7.7 GB/s |
| sequential read, 4M, qd32 × 4 jobs | 4.9 GB/s | |
| sequential write, 4M, qd32 | 2.0 GB/s | |
| random read, 4k, qd1 | 14.6 MB/s (3.6k IOPS, 0.23 ms) | |
| random read, 4k, qd64 | 237 MB/s (58k IOPS) | |
| random write, 4k, qd1 | 1.0 MB/s (256 IOPS, **3.85 ms**) | |
| random write, 4k, qd64 | 33 MB/s (8k IOPS) | 458 MB/s (112k IOPS) |

Three things follow.

**The number the caching plane rests on is met.** Blacksmith's published figure is 6 GB in 3 seconds, ≈2 GB/s, and a cache restore is a large sequential read. At 4.5 GB/s that is 1.3 s. This is the measurement that says the design is worth building on these disks.

**Queue depth 1 is a latency measurement wearing a throughput measurement's clothes**, and it is easy to publish by accident. The identical sequential read is 364 MB/s at qd1 and 4.5 GB/s at qd32 — a 12× difference from one flag. A `dd` benchmark is qd1 by construction, which is how the first pass at this table nearly recorded "246 MB/s" as Ceph's sequential write speed. (The pass before *that* recorded 5.8 GB/s, because `dd if=/dev/zero` writes zeros and librbd turns an all-zero write into a cheap zero op. Both numbers were wrong, in opposite directions, for different reasons. Benchmark with incompressible data at a realistic depth or do not benchmark.)

**Small synchronous writes are where this costs you, and that is the open risk.** 4k random writes are 14× slower than the same disks locally at qd64, and 3.85 ms each at qd1 — a replicated round trip is simply not free. A build writing thousands of small files into a sticky disk is exactly that shape. Two things soften it and neither is proof: the guest's page cache absorbs buffered writes and flushes them deeper and larger than O_DIRECT ever does, and a job's *reads* — the part a cache exists to accelerate — are on the fast side of this table. **#26 must measure a real build against a mounted sticky disk before this is called settled**, and the fallback if it is not is a writeback layer in the guest rather than a different cluster.

## The property this was all for

A clone made on one machine, mapped and read on another. `rbd-second` is a VM with its own kernel (7.0.0-28 against the host's 7.0.0-29) and its own RBD client, reaching the cluster over the network:

```
ubuntu-01   $ rbd clone billet-images/ubuntu-2404-x64@golden billet-cache/from-host
ubuntu-01   $ ... mount, write host-note, unmount, unmap

rbd-second  $ rbd --id billet device map billet-cache/from-host   -> /dev/rbd0
rbd-second  $ cat /mnt/x/host-note
            written on ubuntu-01 at 2026-08-13T16:22:08Z
rbd-second  $ sha256sum /mnt/x/payload
            3097e2ff...f002d003        # byte-identical to what ubuntu-01 wrote
rbd-second  $ echo 'written on the second machine' > /mnt/x/vm-note

ubuntu-01   $ cat /mnt/fromhost/vm-note
            written on the second machine
```

Both directions, on a clone neither machine created alone. **ZFS cannot do this at all** — a `zfs clone` is a dataset on one pool on one host, and there is no operation that hands it to another kernel. That difference is the whole of #23.

It is one physical machine, so this proves the *sharing*, not cross-machine network performance; the second real host will have to be measured when there is one.

The scoped identity holds on both: `rbd --id billet -p .mgr ls` answers `Operation not permitted` from the host and from the VM. A node that is compromised reaches the two pools it was granted and nothing else.

## What billet changed

`node.firecracker.zfs_pool` is gone. `node.ceph` replaces it, as a **sibling of the firecracker block rather than a field inside it**, because storage belongs to the site: two hosts in one place map the same pool, which is the point. The block is required for `provider: firecracker` and refused for every other backend — a container has nowhere to attach a block device, and an ec2 node's compute runs in a region that cannot reach this cluster, so a storage block on either is a deployment that believes it has a cache and does not.

```yaml
node:
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
    # user defaults to billet; conf_path and keyring_path default to Ceph's search path
```

`user` defaults to `billet` and **`admin` is refused**, which is worth more than it looks: `admin` is what the `rbd` command picks for itself when nothing names an identity, and an admin key can delete a pool. Defaulting to it would quietly make every node able to destroy the site's storage, with nothing about the deployment looking different until the day one is compromised.

billet drives Ceph through the `rbd` **command** rather than through librados. The Go binding is cgo, which would end the single static binary and the cross-build matrix in one move — the same reason `mattn/go-sqlite3` is banned — and billet already treats Ceph the way it treats Docker and Tart: an external dependency an operator installs. The cost is a process per call, so `internal/store/ceph` is for operations measured in tens per job and never per block.

`billet check` now proves the configuration rather than parsing it: it lists both pools as the configured identity, which establishes that the monitors answered, the keyring authenticated and the pools exist. It says out loud that this is a **read** and that launching also needs create, clone, snapshot and remove — the same honesty the ec2 preflight owes about `DescribeInstances` not implying `RunInstances`.

## What is still open

- **The cluster is degenerate on one host and always was.** One mon is not a quorum. The mgr has a standby, the OSDs mirror across two drives, and the machine is still a single point of failure. This is the accepted cost recorded in #23, restated here because "HEALTH_OK" makes it easy to forget.
- **Nothing mounts any of this yet.** #24 boots a microVM from a mapped clone; #25 builds the `Store` interface, the commit protocol and eviction on top; #26 puts a real build's writes through it, which is the measurement that decides whether the 4k write cost above matters.
- **The monitoring stack and dashboard were skipped** (`--skip-monitoring-stack --skip-dashboard`) to keep the daemon count down on a machine that is also a compute host. `ceph health detail` reports a failing disk without either. Add them with `ceph orch apply prometheus` and `ceph mgr module enable dashboard` if the graphs are wanted.
- **The cache pool has no quota.** Eviction is #25's, and until it exists a runaway cache fills the same 7.3 TiB the golden images live in. `ceph osd pool set-quota billet-cache max_bytes <n>` is the stopgap if that becomes real before #25 does.
