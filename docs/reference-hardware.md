# Reference hardware

billet does not require any particular machine. This documents the bare-metal
host its Firecracker provider is developed and measured against, so the numbers
elsewhere in the docs have something concrete behind them, and so anyone sizing
their own box has a known-good starting point.

Nothing here is a minimum. A laptop running the `docker` provider is a valid
billet deployment; so is a single EC2 spot instance.

## The reference host

| | |
|---|---|
| CPU | AMD EPYC 7763 (Milan, Zen 3), 64 cores / 128 threads |
| Board | Supermicro H12SSL-CT (single socket SP3) |
| Memory | 512 GB DDR4-3200 ECC RDIMM (8× Samsung M393A8G40BB4-CWE, 64 GB 2Rx4) |
| Network | 2× 10GBase-T (Broadcom BCM57416) + dedicated IPMI (ASPEED AST2500) |
| Storage | 8× SATA3, 8 SAS3 device ports (Broadcom 3008), 2× M.2 PCIe 4.0 ×4 |

Verified against [AMD's 7763 product
page](https://www.amd.com/en/products/processors/server/epyc/7003-series/amd-epyc-7763.html)
and the [H12SSL-i/C/CT/NT
manual](https://www.supermicro.com/manuals/motherboard/EPYC7000/MNL-2314.pdf)
and the [H12SSL-CT product
page](https://www.supermicro.com/en/products/motherboard/h12ssl-ct) rather than
from memory — the H12SSL variants differ in exactly the ways that matter here
(`-i`/`-C` carry 1 GbE, `-CT`/`-NT` carry 10 GbE; only `-C`/`-CT` carry the
SAS3008).

### CPU topology, and why it decides tier shapes

- **8 CCDs × 8 cores, 32 MB of L3 per CCD**, 256 MB total.
- Base 2.45 GHz, boost up to 3.5 GHz, 280 W default TDP (cTDP 225–280 W).
- 8 memory channels, DDR4-3200, 204.8 GB/s theoretical per socket.
- 128 PCIe 4.0 lanes.

The CCD geometry is the part that matters for placement, and Zen 3 is better
for this than Zen 2 was. On Zen 2 a CCD held two 4-core CCXs with separate L3;
on Zen 3 **the CCX and the CCD are the same thing** — 8 cores sharing one 32 MB
L3. So:

**billet does not implement any of this yet** — there is no provider, no
topology discovery and no CPU affinity, so nothing below is a guarantee the
software currently makes. It is why the 8-vCPU shape is the one to design
toward when the Firecracker provider lands.

- An **8-vCPU tier can fit exactly within one CCD**, if its vCPUs are pinned
  one-to-one to that CCD's 8 physical cores: a private 32 MB L3 and no
  cross-CCD traffic. Unpinned, the scheduler is free to spread it anywhere.
- A **4-vCPU tier** fits inside a CCD with room to spare, but two of them on one
  CCD share that L3.
- A **16-vCPU tier cannot fit in a CCD.** It either spans two (crossing
  Infinity Fabric for L3 misses) or uses 8 cores plus their 8 SMT siblings
  within one, which is not the same thing as 16 cores. Which is better is a
  measurement, not a derivation.

**128 threads are not 128 core-equivalents.** Capacity planning targets ~64
physical-core equivalents and admits on measured per-job-class CPU weight, not
on the vCPU number in a tier label.

### Memory

512 GB is 8× 64 GB registered ECC modules (Samsung M393A8G40BB4-CWE, 2Rx4 dual
rank, 1.2 V) filling all **8 DIMM slots** — one per channel. That is the
configuration that reaches full 8-channel bandwidth, and 1 DIMM per channel is
also what lets DDR4-3200 run at its rated 3200 MT/s rather than clocking down.
Dual-rank modules help slightly here: rank interleaving gives the controller more
to overlap.

The board tops out at 2 TB, but only by replacing these modules with larger ones
— every slot is already populated, so there is no incremental upgrade path.

Scheduled guest memory should stay meaningfully below the physical total. The OS,
the Ceph OSDs, and billet itself are not free, and a host that OOMs mid-job fails
every lease it is holding at once. Each OSD sizes itself against
`osd_memory_target`, 4 GiB by default and worth setting explicitly on a machine
that is also running guests — the default assumes the daemon has the box to
itself.

## What still has to be measured

These are deliberately not decided in advance:

- **NPS mode.** NPS1 interleaves all 8 channels; NPS4 exposes 4 NUMA domains of
  2 CCDs each. NPS4 plus NUMA-local pinning gives better locality per VM and less
  bandwidth per VM. Which wins depends on the job mix.
- **SMT policy** — whether a vCPU is a core or a thread, and whether siblings are
  handed to the same guest.
- **Whether pinning helps at all.** Over-strict packing can lower utilization and
  memory bandwidth enough to lose more than the L3 locality gains.
- **Whether the bottleneck is even CPU.** Storage IOPS and latency, PSI, and NIC
  throughput are the more likely limits under a burst, and none of them are
  visible in a core count.

Enumerate the real topology on the machine with `hwloc` / sysfs before writing
any placement policy — a SKU name does not fully determine what the OS sees.

## Single-thread performance, honestly

Server EPYC parts clock lower than desktop CPUs (2.45 GHz base / 3.5 GHz boost
here, against ~5+ GHz on current desktop parts). Highly parallel jobs win big on
a 64-core host; jobs with a long serial phase may not improve, and could regress
even while total throughput rises sharply.

If serial latency turns out to dominate a particular workload, the fix is adding
a high-clock node and pinning that tier to it — not a larger server CPU.

## Storage note

Per Supermicro's [H12SSL-CT product
page](https://www.supermicro.com/en/products/motherboard/h12ssl-ct), the board
carries **8× SATA3**, **2× M.2** (PCIe 4.0 ×4, M-key, 2280/22110), and a
**Broadcom 3008 SAS3** controller. The two M.2 sockets are real and confirmed —
useful, because a mirrored pair of NVMe drives is the natural home for the
control-plane database, which is the one thing here that is not disposable.
Golden images do not live on the mirror: they are RBD images in the Ceph pools
below, so that a second machine can map them rather than be sent a copy.

The SAS3008 provides **eight SAS device ports through two Mini-SAS HD
connectors** — each connector breaks out to four drives. The product page counts
the two physical connectors while the manual counts the eight device ports it
labels `L-SAS 0-3` and `L-SAS 4-7`; those groupings are the same hardware
described at different granularity, not a contradiction. (The retail pack ships
two Mini-SAS HD cables, consistent with that.) The M.2 links are separate and
are not shared with the SAS lanes.

One detail worth knowing before planning a layout:

- **The 3008 advertises RAID 0/1/10**, so it is not unconditionally a plain HBA.
  Ceph wants direct, unabstracted access to each disk — one OSD per drive is what
  makes a drive a failure domain — so confirm the controller is presenting drives
  individually (IT/JBOD-style) rather than through a RAID volume. Handing Ceph a
  RAID volume gives it one device where it thinks it has several, and the
  redundancy it reports is not the redundancy you have.

Golden images and control-plane state should be **replicated**. The control-plane
database is the one thing in a billet deployment that is not disposable; cache
data and sticky disks are disposable by design. Here the two are protected by
different mechanisms — mdraid RAID1 for the root and the ledger, Ceph `size=2`
across two OSDs for the pools — which is described below.


## The reference host as actually built (verified, not assumed)

Ubuntu 26.04 LTS, kernel 7.0.0-29-generic. `/dev/kvm` present, `kvm_amd` loaded, cgroup v2 — which
is the jailer's requirement, not a nice-to-have.

**Firecracker and jailer pinned at v1.16.1** (released 2026-07-02), installed as versioned filenames
behind stable symlinks so `firecracker --version` always matches what is on disk and a bump is
reversible. Billet pins to what the host has rather than the other way round: the provider is
written against a version known to be installed, not against a version someone hopes is.

**A Ceph cluster on the two bare NVMe**, deployed with `cephadm` — Ceph 20.2.3 Tentacle, one mon, two
mgr, one OSD per drive, and two pools at `size=2 min_size=1`: `billet-images` for golden images and
per-job root clones, `billet-cache` for everything the caching plane keeps. The pool names match
`billet.example.yaml` so nothing has to be edited to match a machine.

It replaced a ZFS pool called `tank`, and the reason is one sentence: a ZFS clone exists only on the
machine that took it, so a cache written to one belongs to that host and to no other — the storage
half of billet being a one-machine product, alongside global rather than per-node capacity (#21),
escrow-time placement (#30) and broadcast teardown (#31). RBD presents the same drives as a pool any
node at the site can map, which was verified from a second kernel client before this document was
changed. **How it was built, what it measured, and the two things about Ubuntu 26.04 that break
`cephadm bootstrap` are in [`adr-003-ceph-rbd.md`](adr-003-ceph-rbd.md).**

Redundancy is not lost in the move, which was the open question when it was planned. `cephadm
bootstrap --single-host-defaults` sets the CRUSH failure domain to the OSD rather than the host, so
`size=2` on two OSDs places the two replicas on **two different drives** — the same protection the
ZFS mirror gave, on the same disks — and `min_size=1` keeps the pool writable while one of them is
dead. What a single machine still cannot provide is redundancy against losing the machine: one mon is
not a quorum.

`/var/lib/billet/node` exists at 0750 on local disk — the mdraid root, never NFS, because the state
directory holds SQLite and the deployment identity. The Ceph pools are not a candidate for it either:
the state directory must be local storage, and RBD is a network block device.

**The host kernel is newer than Firecracker's CI is likely to cover.** A 7.0 host kernel is well past
anything a 2026-07 release was tested against, and the interface that matters is the KVM ioctl
surface the VMM uses plus the jailer's cgroup v2 handling. This is not something to settle by
reading: boot one microVM on the box before designing around it. It is a cheap check now and an
expensive surprise during P2.

## An inherited machine can arrive with the worst case already set up

The four NVMe drives arrived configured as a **4-way RAID 0** — no redundancy, and exactly the
"handing a storage system a RAID volume" shape this document warns against. It was destroyed and
rebuilt as mdraid RAID1 for root plus two bare disks for the cluster.

Worth stating because the failure is SILENT. Neither ZFS nor Ceph objects to being handed a RAID
volume: the pool imports, the cluster reports `HEALTH_OK`, and what has been lost is the per-disk
visibility that lets either of them say which copy is the bad one. Nothing warns you, and the day you
find out is the day a disk starts lying. On any inherited or vendor-configured machine, check what
the disks actually are before building on them.

## Guest egress: what this host in particular must block

The security model says guests are blocked from the host LAN, RFC1918, BMC/IPMI,
cloud metadata endpoints and controller ports, default-deny by trust class. On
THIS machine two of those are concrete addresses worth naming, because a
generic rule written from the model alone misses one of them entirely.

**The overlay network, not just the LAN.** This host joins a Cloudflare Mesh and
takes an address in `100.96.0.0/12`. That range is not RFC1918 and is not the
host LAN, so a blocklist derived from the usual list does not cover it — and it
is the most valuable thing on the machine to reach. A compromised job that can
route into the mesh is inside a Zero Trust network that reaches beta and
production databases. **`100.96.0.0/12` belongs in the guest deny list**, and it
belongs there before the first untrusted job runs, not after.

**The BMC is on the LAN with full power and console control** (192.168.1.125 on
the reference machine). Blocking RFC1918 covers it only if RFC1918 is genuinely
blocked rather than assumed; it is worth an explicit rule, because the
consequence of missing it is an attacker with the machine's reset button and its
serial console.

Neither of these is billet enforcing something clever. They are host firewall
rules that have to exist before billet is trusted with anything but its own
jobs, and they are recorded here because the machine is what makes them
specific.
