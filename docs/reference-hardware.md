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
| Storage | 8× SATA3, Broadcom 3008 SAS3 controller, 2× M.2 PCIe 4.0 ×4 |

Verified against [AMD's 7763 product
page](https://www.amd.com/en/products/processors/server/epyc/7003-series/amd-epyc-7763.html)
and the [H12SSL-i/C/CT/NT
manual](https://www.supermicro.com/manuals/motherboard/EPYC7000/MNL-2314.pdf)
and the [H12SSL-CT product
page](https://www.supermicro.com/en/products/motherboard/h12ssl-ct) rather than
from memory — the H12SSL variants differ in exactly the ways that matter here
(`-i`/`-C` carry 1 GbE, `-CT`/`-NT` carry 10 GbE; only `-C`/`-CT` carry the
SAS3008), and the family manual and the per-model page do not agree on
everything. See the storage note below.

### CPU topology, and why it decides tier shapes

- **8 CCDs × 8 cores, 32 MB of L3 per CCD**, 256 MB total.
- Base 2.45 GHz, boost up to 3.5 GHz, 280 W default TDP (cTDP 225–280 W).
- 8 memory channels, DDR4-3200, 204.8 GB/s theoretical per socket.
- 128 PCIe 4.0 lanes.

The CCD geometry is the part that matters for placement, and Zen 3 is better
for this than Zen 2 was. On Zen 2 a CCD held two 4-core CCXs with separate L3;
on Zen 3 **the CCX and the CCD are the same thing** — 8 cores sharing one 32 MB
L3. So:

- An **8-vCPU tier maps exactly onto one CCD** using 8 physical cores, with a
  private 32 MB L3 and no cross-CCD traffic. This is the shape to prefer.
- A **4-vCPU tier** fits inside a CCD with room to spare, but two of them on one
  CCD share that L3.
- A **16-vCPU tier does not fit in a CCD.** It either spans two (crossing
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
ZFS ARC, and billet itself are not free, and a host that OOMs mid-job fails every
lease it is holding at once. Cap the ARC explicitly rather than letting it size
itself against a number that assumes it owns the machine.

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
control-plane database and golden images.

Two details worth knowing before planning a layout:

- **The SAS3 port count is documented inconsistently.** The product page's I/O
  section lists **2 SAS3 ports**, and its key-features summary describes the
  3008 as serving "2 SAS3 ports, 2 M.2" — implying its lanes are shared with the
  M.2 sockets. The family manual's variant table instead says 8. Count the
  physical connectors before committing to a drive plan.
- **The 3008 advertises RAID 0/1/10**, so it is not unconditionally a plain HBA.
  ZFS wants direct, unabstracted access to each disk: confirm the controller is
  presenting drives individually (IT/JBOD-style) rather than through a RAID
  volume. Handing ZFS a RAID volume hides exactly the per-disk errors it exists
  to detect and repair.

Golden images and control-plane state should be **mirrored**. A single SSD lets
ZFS detect corruption but not repair it, and the control-plane database is the
one thing in a billet deployment that is not disposable. Cache data and sticky
disks are disposable by design and do not need the same treatment.
