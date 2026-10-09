---
name: billet-providers-local
description: "Load when touching internal/provider (the contract), internal/provider/docker, firecracker, tart or simulated, the guest launcher scripts, or when a launch, list, destroy or adoption behaves differently on a real host than in the fake. Every rule here was learned by running a real guest."
---

# Local providers: docker, firecracker, tart

## What this area is

`internal/provider` is the narrow contract: `Accepts(trust)`, `Kind`, `Launch`, `Find`, `List`, `Destroy`, with `Spec`, `Instance`, `Teardown` (`TeardownRequested` is the zero value and the safe one; only `TeardownStopped` permits a release), `TrustClass` (zero value `TrustUnknown`, refused), `InstanceName`/`LeaseOf` (an instance is named `billet-<lease>` so reconciliation after a crash needs no side table), and the optional capabilities (`VolumeAttacher`, `GuestVolumeLocator`, `InterruptionSource`, `StagedCredentialReaper`, `QuotaReporter`, `UsageSource`: where the host reads an instance's counters, implemented by firecracker, docker and tart; a target carries the VMM pid's start time, which firecracker reads between two `pidOwner` proofs and the sampler checks after every thread read, and docker's cgroup is accepted only when its last element names the container, because a pid reused between two reads is otherwise charged to the wrong job. A tart VM has no cgroup: its guest runs in a Virtualization.framework XPC process parented to launchd, not to `tart run`, which holds almost none of the CPU (macOS 27, tart 2.37.0, 2026-09-30), so tart's target is `Process: true` with that pid, found as the one process whose executable is the VM service and which holds `vms/<name>/disk.img` open, proved again after its start is read, and only for a lease's name whose directory carries this deployment's ownership marker, with no symlink at the directory or the disk (lsof follows one) and the same disk inode before and after, since the store also holds an operator's own VMs. `lsof -t` exits 1 both for no opener and for an error and suppresses its warnings, so billet passes `+w` and refuses any warning on either exit status; it sees only this user's processes and skips the rest silently. A holder whose executable cannot be read is skipped only on ESRCH, because skipping an unreadable one could leave another VM looking unique. A process's lifetime energy is the job's only from a reading within three intervals of the end. The sampler reads it through `proc_pid_rusage` via a raw `proc_info` call (x/sys has no wrapper, cgo would end the static binary): CPU times arrive in mach ticks at `hw.tbfrequency` (24 MHz on Apple silicon, so read as nanoseconds they are 41.7 times too small), and `proc_pidpath`'s call answers zero, not a length). A container's network is its own `eth0`, read from `/proc/<init pid>/net/dev` (its namespace's table, the container's view, so no rx/tx swap), with the init's start time read between two proofs that its cgroup names the container and checked again after every read, and only when `HostConfig.NetworkMode` gives it a namespace of its own (not `host`, `none`, `container:` or `ns:`), since a pid proof says nothing about whose namespace it is in. An optional capability may not carry a safety invariant. A provider imports nothing above itself (depguard). `docker` exists so `billet init` works on a laptop. `firecracker` runs every guest under the jailer with a Ceph-backed root disk. `tart` drives the `tart` CLI and Apple's Virtualization.framework; `realtart_test.go`, `realguest_test.go`, `realfirecracker_test.go` and `realdocker_test.go` pin what the real tools do.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Every backend: [references/contract.md](references/contract.md)

- **`Accepts` is asked before anything expensive.**
- **`List` errors rather than answering short.**
- **One process writes the launch verdict from a closed vocabulary.**
- **A cancelled command is not a returned one.**
- **Never quote the guest.**

### Firecracker: [references/firecracker.md](references/firecracker.md)

- **Every guest runs under the jailer**
- **`jailer --daemonize` exits 0 for a VM that died on startup**
- **The jailer creates a per-VM cgroup only when given a `--cgroup`, and the two cgroup forms cannot coexist on a host**
- **There is no API action that kills a microVM.**
- **The kernel is linked into each jail and stays root-owned.**
- **Two defects survived every unit test and died on the first real launch**

### Tart: [references/tart.md](references/tart.md)

- **`tart run` is the VM, and nothing believes its exit status.**
- **Tart has no labels, so ownership is a marker file and the order is the invariant.**
- **`List` cross-checks tart's JSON against the vms directory and asks `tart get` about each absence.**
- **`tart stop` requests a stop.**
- **The store lock closes the delete race.**
- **The registration travels on stdin, and the guest agent kills its exec session's process group.**
- **softnet admits the host, so billet blocks `@host`.**
- **softnet blocks the guest's resolver, so billet configures one and proves resolution.**
- **The runner gets GitHub's locale.**
- **A moving image tag resolves to its pulled digest**
- **SIGKILL re-adopts; SIGTERM drains.**

### Simulated: [references/simulated.md](references/simulated.md)

- **A sixth backend that starts no compute, for measuring placement rather than running it.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A change to a backend's `List` or `Destroy`.** `List` errors rather than answering short, because reconciliation frees every lease absent from it ([references/contract.md](references/contract.md)); ask what makes a row appear and what could make that stop being true while the compute runs on. Prove it in the backend's `real*_test.go` as well as the fake.

## Where the tests are

- There is no shared contract suite; each backend carries its own tests of the contract points above (refusal of unknown and untrusted work, `List` erroring rather than answering short, an idempotent and proving `Destroy`, the name shape, identity scoping, the registration off argv and out of every log). `docker/realdocker_test.go`, `firecracker/realfirecracker_test.go` (`TestTheRealHostIdentityIsOneNewWillAccept`, launch and destroy leave nothing), `tart/realtart_test.go` (lifecycle, error phrasings, list shape, two deployments kept apart, the store lock ignored by tart), `tart/realguest_test.go` (Linux `setsid` and macOS perl, softnet resolver, Xcode).
- `internal/provider/firecracker/*_test.go`: `TestAPidFileThatIsNotAPidStopsTeardown` asserts the jail, the disk and the tap are still there, not merely that an error came back.

## Related skills

`billet-capacity` (what `List` and `Destroy` prove), `billet-security` (trust gates and the pid rule), `billet-guest-images` (the kernel pair and images pull for tart), `billet-lifecycle` (the Mac agent), `billet-storage-and-cache` (the Ceph root disk).
