# Reference hardware

billet does not require any particular machine. This documents the hosts its
providers are developed and measured against, so the numbers elsewhere in the
docs have something concrete behind them, and so anyone sizing their own box has
a known-good starting point.

There are two, because the two backends need different machines and neither can
stand in for the other: a **Linux host** for the Firecracker provider, and an
**Apple Silicon Mac** for the tart provider, since a macOS guest is only legal
and only possible on Apple hardware.

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
half of billet being a one-machine product, alongside global rather than per-node capacity,
escrow-time placement and broadcast teardown. RBD presents the same drives as a pool any
node at the site can map, which was verified from a second kernel client before this document was
changed. **How it was built, what it measured, and the two things about Ubuntu 26.04 that break
`cephadm bootstrap` are in [`adr-003-ceph-rbd.md`](adr-003-ceph-rbd.md).**

The cluster also runs `ceph osd set-require-min-compat-client mimic`, which `cephadm` does not: without
it a snapshot must be protected before it can be cloned, and a protected snapshot with a live clone can
be neither unprotected nor removed — so a cache generation any running job holds would be undeletable.
`billet check` refuses a cluster that would clone the old way, for either of the two settings that
decide it, so this is enforced rather than remembered.

Redundancy is not lost in the move, which was the open question when it was planned. `cephadm
bootstrap --single-host-defaults` sets the CRUSH failure domain to the OSD rather than the host, so
`size=2` on two OSDs places the two replicas on **two different drives** — the same protection the
ZFS mirror gave, on the same disks — and `min_size=1` keeps the pool writable while one of them is
dead. What a single machine still cannot provide is redundancy against losing the machine: one mon is
not a quorum.

`/var/lib/billet/node` exists at 0750 on local disk — the mdraid root, never NFS, because the state
directory holds SQLite and the deployment identity. The Ceph pools are not a candidate for it either:
the state directory must be local storage, and RBD is a network block device.

**The host kernel is newer than Firecracker's CI is likely to cover, and it works.** A 7.0 host kernel is well past anything a 2026-07 release was tested against, and the interfaces that matter are the KVM ioctl surface the VMM uses and the jailer's cgroup v2 handling. That was not settled by reading: a microVM was booted on the box before anything was designed around it, and then billet's own provider launched, found, listed and destroyed one. Both halves work — the VMM on this kernel, and the jailer's chroot, uid drop and cgroup v2 placement.

**An account for the jailer to drop the VMM to.** `billet`, uid 994, created with `useradd --system --no-create-home --shell /usr/sbin/nologin billet`. The jailer takes a uid and a gid rather than a name, so billet resolves it once at startup and refuses root — dropping privileges is the entire reason the jailer is used, and `--uid 0` keeps every one of them in front of a process whose input is somebody's CI job.

**Four things about this host decide how the provider is written**, and each was measured here rather than read:

- **The jailer names its chroot after the RESOLVED `--exec-file`.** Because the binaries are installed as versioned filenames behind stable symlinks — which is what makes `firecracker --version` honest — every microVM lives under `/srv/jailer/firecracker-v1.16.1/`, not `/srv/jailer/firecracker/`. That directory is the one billet enumerates to find out what is running, so reading the wrong one reports an empty inventory, and an empty inventory is capacity freed for guests that are still running.
- **`jailer --daemonize` exits 0 for a VM that died during startup**, leaving a pid file and an API socket behind. Its exit code proves nothing; the VMM's own API is what billet believes.
- **The jailer creates a per-VM cgroup only when given at least one `--cgroup`**, and the two forms cannot coexist: once any VM on the host has been started with one, a VM started without one fails with `CgroupMove … Resource busy`.
- **There is no API action that kills a microVM.** `SendCtrlAltDel` is a keyboard event the guest decides what to do with, and a real guest here ignored it for twenty seconds. billet signals the VMM process, after proving the pid is still that microVM's by the `--id` in `/proc/<pid>/cmdline`.

The `junioryono.billet.host` Ansible role owns the guest network rather than an application repository: `billet0` is the trusted DHCP/NAT bridge and `billet1` is the separate untrusted bridge. Its nftables table permits DHCP, DNS and the node cache endpoint on the host, blocks every other guest-to-host connection, and blocks forwarded traffic to private, link-local and CGNAT destinations. The live provider tests originally ran against `lxdbr0`; deployment uses the two billet-owned bridges so removing LXD cannot take CI networking with it.

## The reference Mac, for the tart provider

A macOS guest is only legal, and only possible, on Apple hardware, so the tart provider has a reference host of its own. It is a **Mac mini, M5 Pro (18-core CPU, 20-core GPU), 64 GB unified memory, 1 TB SSD, standard 2.5GbE**.

The same framing as above: a known-good starting point, not a minimum. A single Mac mini with 24 GB runs one macOS guest and is a real deployment.

**Every number below is sized against a cap, not against the hardware.** Apple's licence permits **two concurrent macOS guests per physical Mac**, which is what `config.DefaultMacOSVMLimit` encodes — so a Mac's useful macOS capacity is two guests no matter how large it is, and the memory is chosen to make those two comfortable rather than to fit more:

| | |
|---|---|
| 64 GB | two ~24 GB macOS guests, plus the host and a Linux arm64 tier |
| 1 TB | one Xcode image is **87 GB on disk** (140 GB virtual), before clones |
| 2.5GbE | deliberate: a macOS node never joins a Ceph site |

**10GbE was skipped on purpose, and that is a statement about the architecture rather than about money.** `node.ceph` is *refused* on a tart node — a Mac's compute cannot attach an RBD device, so a storage block on one describes a cache that does not exist. A macOS node's traffic is therefore GitHub long-polling, image pulls and artifact uploads: bandwidth to the internet, not to a storage fabric. The bare-metal host above needs 10GbE because it *is* the storage fabric.

**Disk is the constraint that surprises people.** The Xcode image measured 87 GB in the local store, and billet refuses to pull one inside a launch precisely because of that size — a node executes one command at a time, and a multi-hour download inside a job would time out that job and everything queued behind it. Fetch images with `billet images pull` before the first job; `billet check` lists which of this node's tier images are missing.

### What was measured, and on what

The mini is the machine this is *sized* for. Everything asserted about tart, macOS guests and softnet below was measured on an **Apple M2 Max (12-core, 32 GB) running macOS 26.0 (25A354), tart 2.36.0 and softnet 0.23.0** — including a real GitHub Actions Xcode job that built an iOS target inside a billet-launched guest and was torn down afterwards. Where a fact came from running something, this says so; where it did not, it says that too.

Three host limits are encoded in billet rather than assumed, and each is a sentence Virtualization.framework or tart actually produced:

- **A macOS guest under 4 GiB is refused** — `LessThanMinimalResourcesError`. A tier that asks for less is a config error, not a small guest.
- **A third concurrent macOS guest is refused** — *"The number of VMs exceeds the system limit (other running VMs: …)"*. This is where `config.DefaultMacOSVMLimit = 2` actually lands: the constant is pinned to observed behaviour, not to a reading of the licence.
- **`tart run` is the VM.** There is no daemon; the process *is* the guest, so billet starts it detached in its own session and never believes its exit status. A billet restart or upgrade therefore does not kill running jobs.

### Before the commands: what this host needs that a Linux host does not

Stated first because none of it can be done remotely on a machine that has not been set up yet, and because a tart deployment is **not stranger-completable** without it — the same objection that applies to a Firecracker-first example.

- **Apple Silicon and a recent macOS.** Not a preference: Virtualization.framework is what runs the guests.
- **One session with a display and a keyboard.** macOS Setup Assistant has no headless path without MDM enrolment. Everything after that first login is remote.
- **An image pull measured in tens of gigabytes**, done before the first job.
- **A one-time `softnet` grant, if this node will run untrusted work.** softnet ships with tart and needs to be setuid-root to create a vmnet interface. `billet check` reports its state on every run, because the grant survives nothing — `brew upgrade` replaces the binary and resets its ownership.

Run the exact command `billet check` prints, rather than one written from memory. **The symlink is not the file**: Homebrew puts `bin/softnet` in front of a Cellar path, and the ownership that decides is the target's — on the machine these were measured on, that is `/opt/homebrew/Cellar/softnet/0.23.0/bin/softnet`, which changes on every upgrade. billet resolves it and names it, in this shape:

```
sudo chown root <resolved path> && sudo chmod u+s <resolved path>
```

**Both commands, in that order, always.** `chown` clears the setuid bit — measured on macOS — so granting ownership after setting the bit silently undoes it. And `ls` showing an `s` on a binary owned by the installing user grants *nothing*: setuid confers the **owner's** privileges, so the bit without root ownership is decoration. softnet then fails on the first untrusted launch with *"root privileges are required to run and passwordless sudo was not available"*. `billet check` checks both facts, which is why it reports the grant on every run rather than only when it is missing.

When the `softnet` on `PATH` is **not** beside the tart binary, `billet check` still shows the command but puts a warning in front of it: verify it is tart's own softnet before granting it root. That ordering is deliberate — `softnet` is taken from `PATH`, so a writable directory earlier in `PATH` would otherwise let this check hand an operator a privilege-escalation recipe for a stranger's executable.

### Running billet as a service on macOS

billet ships `deploy/sh.billet.node.plist` and `deploy/sh.billet.server.plist`. They are **launch agents, not launch daemons**, and that is forced rather than chosen — three things about a Mac make a root daemon the wrong shape, and the first one is the expensive one to discover.

**Virtualization.framework needs an unlocked `login.keychain`.** Since macOS 15 this is an undocumented requirement, recorded in [tart's own FAQ](https://tart.run/faq/): a VM will not run without one, and it fails with `SecKeyCreateRandomKey_ios failed`, `Failed to generate keypair`, or `Interaction is not allowed with the Security Server` — none of which mention a keychain. **A headless SSH session leaves that keychain locked**, so this is exactly the state a remotely-administered Mac is in. A daemon has no login session and therefore no unlocked keychain at all.

That is why a dedicated Mac node wants **automatic login**: a real GUI session at every boot keeps the keychain unlocked. It is a genuine security decision — the disk is unlocked and a session is live whenever the machine is on — and it is the price of running macOS guests with nobody present. The alternative is `security unlock-keychain login.keychain`, which means the password lives wherever that command is driven from. First login must happen through Screen Sharing at least once, because that is what creates the keychain.

The other two are smaller and still fatal: tart's VM store is **per-user** (`TART_HOME`, default `~/.tart`), so a root daemon looks at root's store and finds none of the images you pulled; and the Linux node unit runs as root only because Docker's socket and the Firecracker jailer demand it, neither of which applies to a CLI the operator's own account drives.

Two settings inside the plists are load-bearing, and both were measured by running the agent rather than by reading about it:

- **`PATH`.** A launch agent does not inherit your shell's PATH — launchd's own default is `/usr/bin:/bin:/usr/sbin:/sbin`, which contains no Homebrew prefix. Measured: the node starts, registers, and then refuses all work with `tart: list VMs: exec: "tart": executable file not found in $PATH`, which is the correct refusal (it cannot tell what is already running) and an opaque one. `softnet` is resolved the same way, so the same omission also breaks untrusted isolation.
- **`ExitTimeOut`.** launchd sends `SIGTERM`, waits, then `SIGKILL`s, and its default wait is **five seconds** — measured on macOS 26 by asking `launchctl print` about an agent that sets none, against a `launchd.plist(5)` that says twenty. billet's node answers `SIGTERM` by *draining* — it stops taking work and waits for the jobs already running, for as long as they run; `drain_timeout` only decides when it starts reporting that the drain is long. Without an explicit value, a reboot or a `launchctl bootout` kills a node mid-drain and leaves guests running with their leases renewed by nobody. The plists set 88200 seconds, the same number `TimeoutStopSec` uses on Linux, and a test asserts the two agree. **Never set it to zero**: launchd reads zero as infinity, and its man page warns that such a job can stall shutdown forever.

You do not install these by hand. `billet local up` writes the agents, clears any disabled override, bootstraps them and proves they held their process; `billet local status` reports what launchd actually has loaded; `billet local down` drains the host and stops it; and `billet local uninstall` removes the agents and leaves your config, App key, ledger, identity and CA where they are.

```
CFG=/usr/local/etc/billet/billet.yaml

billet init --profile local-service --provider tart --node-name mac-mini-1
billet github-app create --org <your-org> --config "$CFG"
billet images pull --config "$CFG"    # the macOS image is ~87GB; do it before the first job
billet check --config "$CFG"
billet local up                       # installs, starts and proves both agents
billet local status
```

`--config` on every one of those but the last two: the default is the per-user path, and a `local-service` generation lives where the launch agents read it. `billet local up` and `status` resolve that path themselves.

Run those on the Mac. `billet init --provider tart` is **refused** on anything but Apple silicon, and not merely as a platform check: everything it fills in is read from the machine running the command — the ceiling is measured there, the node name comes from that hostname, and the state, key and lock paths are that platform's rather than the `/usr/local` ones a Mac's launch agents read. There is no `--emit` path either; the `junioryono.billet.host` role is Linux-only, so a Mac is converged by `billet local up` rather than by Ansible.

`--node-name` is the one input this machine cannot supply for itself. A macOS tier has to pin the host it lands on, because Apple's limit is counted per physical machine — and a stock Mac's hostname carries the name from System Settings, with spaces, an apostrophe or a `.local` suffix, none of which is a legal node name. billet refuses rather than sanitising one into existence: this is the name `billet ca issue` will mint a certificate for and the control plane will authorise, so it is not billet's to invent.

The generation serves **macOS guests** by default. `--guest-os linux` gives an arm64 Linux tier instead, and passing both gives one of each — the Linux guest is what does the two things a macOS guest on Apple's hypervisor cannot, a `docker build` and a service container. Each guest kind names its own image (`--macos-image`, `--linux-image`); the defaults are the two published images billet has run real jobs in.

Its tiers are `trust: untrusted`, so the generated config carries `node.tart.untrusted_isolation: softnet` and `billet check` **fails** until the grant above is in place — a node that offers to confine a fork's job on a host that cannot confine it is worse than one that refuses. Pass `--runner-group` and `--workflow` instead for a trusted pool, and the generation omits the isolation rather than promising one nothing uses.

**Run them as the account that will run the node, without `sudo`.** A launch agent lives in a logged-in user's GUI domain; root's domain has no unlocked login keychain for Virtualization.framework and none of the tart images you pulled, so a root-installed agent starts and then fails for reasons naming none of that. `billet local up` refuses to run as root rather than let that happen.

The directories are the one thing that does need `sudo`, because `/usr/local` is root-owned on a stock Mac and launchd creates nothing itself — an agent whose log directory is missing fails to spawn with no log to say so. `billet local up` refuses with the exact command rather than asking for a password:

```
sudo mkdir -p /usr/local/etc/billet /usr/local/var/log/billet \
              /usr/local/var/lib/billet /usr/local/var/run/billet/locks
sudo chown "$(id -un)" /usr/local/etc/billet /usr/local/var/log/billet \
              /usr/local/var/lib/billet /usr/local/var/run/billet/locks
```

The shipped files name `/usr/local/bin/billet`, `/usr/local/etc/billet/billet.yaml` and `/usr/local/var/log/billet/`; launchd performs **no variable substitution**, so those paths are literal. Verified end to end on the reference M2 Max: a launchd-managed node registered with the control plane, launched a guest, ran a real GitHub Actions job to green, and destroyed the guest afterwards.

### Two launchd facts that decide how billet drives it

Both were measured by running real agents, and both read the other way if you reason from the documentation. They live in `internal/lifeops/launchd/reallaunchd_test.go` so they cannot go stale.

**A loaded job is not its plist.** launchd reads a plist exactly once, at bootstrap, and keeps what it read — so replacing the file changes nothing about the running job, and `launchctl print` reports the values it *loaded*. A node can therefore be running a stale five-second `ExitTimeOut` while the file on disk is byte-identical to the one billet ships. `billet local up` compares the **loaded** job — its program, arguments, drain grace and whole environment — and refuses a host whose services drifted, naming `billet local down` as the way through, because updating a loaded job means stopping it and stopping a node is a drain.

**A `bootout` is a request, not a stop.** It returns in *zero seconds* with the process still draining; the service stays in the domain reporting `state = SIGTERMed` with its pid, and the record only disappears when the process finally exits. So neither the command's return nor the service's absence is proof. billet captures the pid first and waits for both facts — the service leaving the domain, and that pid dying. Relatedly, `launchctl kill TERM` on these agents makes launchd **restart** the service you asked it to stop, because both carry `KeepAlive{SuccessfulExit: false}`; billet never uses it.

There is a third that shapes `uninstall`: launchd's **disabled-override database** is durable, keyed by label, and survives both the bootout and the removal of the plist. A label left disabled with no plist to explain it makes a later install bootstrap a service launchd silently refuses to run — with the same `Bootstrap failed: 5: Input/output error` it gives for three other situations. `billet local uninstall` removes the plist, flushes that removal, and only then clears the override.

### Headless operation

Verified against macOS 26 (Tahoe) sources rather than on the mini, which has not arrived. Two of these are recent behaviour changes, so re-verify on the real host before relying on them:

**SSH first, then anything else.** `sudo systemsetup -setremotelogin on`, then confirm with `sudo systemsetup -getremotelogin`. On Tahoe, Screen Sharing after a reboot needs SSH to already be reachable, so enabling SSH is not one option among several — it is the one that makes the others recoverable.

Harden with a drop-in rather than by editing the main config, because macOS owns `/etc/ssh/sshd_config.d/100-macos.conf` and the first value wins on a conflict — so the drop-in has to sort *ahead* of it:

```
sudo tee /etc/ssh/sshd_config.d/000-headless.conf <<'EOF'
PermitRootLogin no
PasswordAuthentication no
EOF
```

**Do not reach for `pmset autorestart`.** It is a silent no-op on Apple Silicon — it accepts the setting and changes nothing — and it is also unnecessary, because Apple Silicon minis power on by themselves when mains power returns. A guide that tells an operator to set it leaves them believing they have configured automatic recovery when they have configured nothing.

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
