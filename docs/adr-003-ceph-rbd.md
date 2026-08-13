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
  --skip-dashboard --skip-monitoring-stack --ssh-user <account-with-passwordless-sudo>
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

**cephadm drives every host over SSH as root, and this host has `PermitRootLogin no`.** Enabling root login to satisfy an orchestrator is the wrong trade; `--ssh-user <user>` with passwordless sudo is supported and leaves the hardening in place. The account chosen already had passwordless sudo, so no new privileged account was created.

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

The build benchmark below made the same class of mistake twice more before it produced anything: its first pass ran `go build ./...` outside the source tree, which returned in 76 ms having built nothing, and wrote and read its 40,000 files inside one run, so every read came from the page cache it had just filled. Both defects reported that RBD costs nothing — which is the answer a benchmark gives when it never reaches the storage. **A suspiciously equal result is a bug until proven otherwise.**

**Small synchronous writes look alarming here, and the next section is why that reading was wrong.** 4k random writes are 14× slower than the same disks locally at qd64, and 3.85 ms each at qd1 — a replicated round trip is not free, and a build writing thousands of small files is that shape. That was recorded as the open risk, with the expectation that a guest's page cache would absorb most of it. Expectations of that kind are what this document exists to replace, so it was measured.

## What a real build costs, which is not what fio implies

Three workloads on a mounted RBD volume against the same tree on the mdraid NVMe root, caches dropped before every phase, median of three. All times in milliseconds. **The whole table was taken twice**, and the two passes agree within 1% on every figure that carries an argument — `go build` 15379/15350 locally and 15738/15641 on RBD, the cold read 13077/12969, fsync-per-file 23364/23298. A single benchmark pass is a sample, and the ratios below are only worth quoting because the second pass reproduced them.

| | go build, cold cache | 40k files write+sync | 40k files read COLD | 40k delete | 2k files, fsync each | git clone | git status |
|---|---|---|---|---|---|---|---|
| local NVMe | 15379 | 2274 | 3214 | 888 | 9885 | 100 | 49 |
| RBD | 15738 | 3180 | **13077** | 888 | 23364 | 177 | 108 |
| RBD tuned | 15776 | 3298 | 12540 | 904 | 23173 | 180 | 98 |
| **cost** | **1.02×** | 1.40× | **4.07×** | 1.00× | 2.36× | 1.77× | 2.20× |

**A real compile is 2% slower.** That is the headline and it is the workload CI is mostly made of: `go build` with a cold build cache is CPU-bound, its writes are buffered, and the storage underneath it barely shows. The fio table above cannot tell you that, because a benchmark that does nothing but IO measures a machine that does nothing but IO.

**fsync-per-file costs 2.4×, not 14×.** Package managers that fsync every file are the worst realistic case, and even they land nowhere near what the synthetic number implies — a real workload has work between its fsyncs, and the round trip overlaps with it.

**The expensive one is a COLD READ of many small files, at 4×**, and it is worth noticing that this is the opposite of what the fio table predicted: reads were "on the fast side" there. They are, sequentially. Reading 40,000 small files is not a sequential read — it is 40,000 round trips, IOPS-bound, and no amount of readahead helps because nothing is being read ahead *of*.

**That is also the cache-restore path, so it needs the right baseline.** 13 s to hydrate 40,000 files from RBD is four times what the local disk costs and a small fraction of what NOT having the cache costs — re-running an install over the network is tens of seconds to minutes. The comparison that decides whether a sticky disk is worth having is against re-fetching, not against a disk the second machine cannot see.

**Tuning did nothing, and that is a finding rather than a gap.** `read_ahead_kb` at 32 MB and an ext4 stride/stripe aligned to the 4 MiB object size moved nothing outside noise on any workload. A 64 KiB object size (measured in the earlier pass) did not either. So this ADR ships no tuning advice: the honest answer is that the defaults are what these numbers were taken on, and the shape that costs — small scattered reads — is not one those knobs address.

**What #26 still has to establish** is narrower than it was: not whether RBD is viable for build workloads, which it is, but whether the *hydrate* step of a sticky disk holding a real `node_modules` stays acceptable at 4×, and whether it is better served by streaming the volume than by walking it as files.

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

It is one physical machine, so this proves the *sharing*, not cross-machine network performance; the second real host will have to be measured when there is one. `rbd-second` was an LXD VM rather than a container, and the distinction is the whole test: a container shares the host kernel, so `rbd map` inside one would be the host's client under another name. Recreate it with

```bash
lxc launch ubuntu:26.04 rbd-second --vm -c limits.cpu=4 -c limits.memory=4GiB
lxc exec rbd-second -- apt-get install -y ceph-common
# copy /etc/ceph/ceph.conf and the client.billet keyring in, 0600
```

and delete it again when the property has been re-checked; nothing depends on it existing.

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

### Ceph is more permissive than a validator expects, so every name rule is measured

The first version of `CheckCeph` refused a pool or an identity beginning with `-`, on the reasoning that billet builds an argv and nothing quotes a value out of being a flag. Running it says otherwise, and the correction is worth recording because this repository has the rule written down already — *a rule about someone else's API is pinned to measured behaviour, not to reasoning* — and walked into it anyway.

| probe | result |
|---|---|
| `rbd --id -weird -p billet-images ls` | `-weird` is used as the identity; librados looks for a keyring for it |
| `rbd -p -weirdpool ls` | `rbd: error opening pool '-weirdpool'` — consumed as the pool |
| `rbd info -weirdpool/nothing` | **`rbd: unrecognised option '-weirdpool/nothing'`** |
| `ceph osd pool create -weirdpool` | `pool '-weirdpool' created` |
| `ceph osd pool create a/b`, `a@b`, `a b`, `a<TAB>b` | all four created |
| `ceph osd pool create .weirdpool` | `pool names beginning with . are not allowed` |
| `ceph auth get-or-create client.a/b`, `client.a b` | both created |
| `rbd --id client.billet -p billet-images ls` | `(13) Permission denied`, where `--id billet` lists the pool |

An option that requires a value consumes the next token whatever it starts with, so the argv argument was simply false — for the *value* of a flag. It is true for a **positional**, which is what a `pool/image` spec is, and that is the rule billet kept. The identity is never positional, so its dash rule was removed along with the whitespace and slash rules beside it; what survives there is the `client.` prefix, which measurably authenticates as `client.client.billet`. "Is this a legal Ceph name" turned out never to be the question, because Ceph accepts nearly anything; "does billet address it correctly" is.

billet drives Ceph through the `rbd` **command** rather than through librados. The Go binding is cgo, which would end the single static binary and the cross-build matrix in one move — the same reason `mattn/go-sqlite3` is banned — and billet already treats Ceph the way it treats Docker and Tart: an external dependency an operator installs. The cost is a process per call, so `internal/store/ceph` is for operations measured in tens per job and never per block.

### The cluster is a precondition, and billet checks it

Clone v2 is a requirement of billet's storage rather than a preference, and a requirement nothing verifies is a hope. `billet check` therefore **refuses** a cluster whose `require-min-compat-client` predates mimic, naming the one command that fixes it.

Refusing now rather than warning is a judgement about when the cost lands. Nothing in billet clones yet, so a clone-v1 cluster works perfectly today and the fix is one command on an empty cluster; the same fix after a fleet has been built on it is a pool that cannot be reclaimed and a debugging session that starts nowhere near the storage. The failure this prevents is invisible right up until it is expensive.

The rule is stated as **"not one of the releases older than mimic"** rather than "one of the releases at or after mimic", and the direction is the point: a list of recent releases goes stale on the next Ceph release and starts refusing correct clusters, while the set of releases older than mimic can never grow. Measured against 20.2.3 rather than remembered — every name in that set is one this cluster accepts, and one it does not know is refused with `is not recognized`, so the list is complete.

**That direction is fail-open, so what bounds it matters.** "Anything I do not recognise is newer" admits whatever the binary at that path printed, so the answer must first be a release-shaped token — a short lowercase word — and `unknown` is refused by name. `unknown` is not hypothetical: it is the zero value of Ceph's release enum, and `osd set-require-min-compat-client unknown` is *refused* (measured), so it can only arrive from Ceph itself. It means the cluster was never told which clients it admits, and a cluster that was never told clones the old way. Reading it as "some release newer than mimic" is exactly the mistake the shape check exists to prevent.

**And the floor alone is a proxy, which one config key defeats.** `rbd_default_clone_format` overrides what the minimum client release implies, and billet reads it too. Measured: with the floor at mimic and the format forced to `1`, cloning an unprotected snapshot fails with `rbd: clone error: (22) Invalid argument` — a green preflight beside the exact failure the preflight exists to prevent. The two settings are resolved together, and each cause gets its own remedy, because "raise require-min-compat-client" is wrong advice when the floor is already high enough.

Reading that option is itself a small lesson in not reasoning about someone else's grammar: `rbd config pool list` takes its pool as a **positional** argument, so `rbd --format json -p <pool> config pool list` answers `unrecognised option '-p'` — and the unit test asserted billet's own mistake, exactly, and passed, until the real cluster refused it. An argv assertion pins what the code does; only running it says whether the tool agrees.

Pool replication is **reported and not judged**. How many copies a pool keeps is the operator's decision, and billet has no standing to refuse it — but it is invisible from the config file, so `billet check` prints `size` and `min_size` for both pools and says plainly when a pool keeps one copy. An operator who believes their golden images are mirrored should find out there rather than after a drive dies.

```
ceph     client.billet -> /etc/ceph/ceph.conf
         billet-images      0 image(s)  size 2, min_size 1       golden images and per-job root clones
         billet-cache       0 image(s)  size 2, min_size 1       cache volumes
         clone v2 (require-min-compat-client mimic, rbd_default_clone_format auto), so a cache generation can be reclaimed while a job still holds a clone of it
         (read only — launching also needs create, clone, snapshot and remove in both pools; …)
```

and the two ways it refuses, each verified by putting the live cluster into that state and restoring it:

```
$ ceph config set client rbd_default_clone_format 1
node.ceph: this cluster would clone snapshots the old way…: rbd_default_clone_format is set to 1,
which overrides the cluster's minimum client release (mimic); unset it with
`ceph config rm client rbd_default_clone_format`

$ ceph osd set-require-min-compat-client luminous
node.ceph: this cluster would clone snapshots the old way…: require-min-compat-client is "luminous";
raise it with `ceph osd set-require-min-compat-client mimic` (which refuses while clients older than
mimic are connected — `ceph features` lists them)
```

Both facts come from `ceph`, which ships in the same package as `rbd`, and both are readable by the scoped `client.billet` identity — verified, because a check that needs admin rights to run is a check that will be run as admin.

`billet check` proves the configuration rather than parsing it: it lists both pools as the configured identity, which establishes that the monitors answered, the keyring authenticated and the pools exist. It says out loud that this is a **read** and that launching also needs create, clone, snapshot and remove — the same honesty the ec2 preflight owes about `DescribeInstances` not implying `RunInstances`. A host with no `rbd` command fails the check rather than being reported: only a firecracker node may carry a `node.ceph` block, so this file always describes a machine that is meant to run jobs, and a control plane is unaffected because it has no node section to check.

The failure paths are worth showing, because they are most of the value:

```
$ billet check --config billet.yaml      # a pool that does not exist
node.ceph: this host could not read the pools it is configured with, so it could not launch
anything: ceph: pool "billet-typo" could not be listed as client.billet: exit status 2:
rbd: listing images failed: (2) No such file or directory

$ billet check --config billet.yaml      # an identity the cluster does not know
… could not be listed as client.nosuchuser: exit status 13: rbd: listing images failed:
(13) Permission denied
```

## What is still open

- **The cluster is degenerate on one host and always was.** One mon is not a quorum. The mgr has a standby, the OSDs mirror across two drives, and the machine is still a single point of failure. This is the accepted cost recorded in #23, restated here because "HEALTH_OK" makes it easy to forget.
- **Nothing mounts any of this yet.** #24 boots a microVM from a mapped clone; #25 builds the `Store` interface, the commit protocol and eviction on top; #26 wires a sticky disk into a job. The question #26 inherits is no longer whether RBD can carry a build — it can, at 2% — but whether hydrating a real `node_modules` at 4× is better served by walking it as files or by streaming the volume.
- **The monitoring stack and dashboard were skipped** (`--skip-monitoring-stack --skip-dashboard`) to keep the daemon count down on a machine that is also a compute host. `ceph health detail` reports a failing disk without either. Add them with `ceph orch apply prometheus` and `ceph mgr module enable dashboard` if the graphs are wanted.
- **The cache pool has no quota.** Eviction is #25's, and until it exists a runaway cache fills the same 7.3 TiB the golden images live in. `ceph osd pool set-quota billet-cache max_bytes <n>` is the stopgap if that becomes real before #25 does.
