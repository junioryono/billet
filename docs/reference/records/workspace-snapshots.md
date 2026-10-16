# Workspace snapshots

What Firecracker actually did when one warm guest was snapshotted and restored many times, measured on 2026-10-16 on the reference deployment's Firecracker node (`linux-01`: 128 hardware threads, 499 GiB, host kernel 7.0.0-38-generic, Firecracker v1.16.1), for the facts [ADR-017](../decisions/adr-017-workspaces.md) relies on. The guest was a production golden generation of the `ubuntu-2404-x64` runner image with its paired guest kernel (6.1.155), given a probe agent that answers over vsock and a fixed address on `eth0`. Nothing on the node's own bridges, chroot base or uid range was used.

## Method

A probe template was cloned from the golden into the cache pool under a name billet never gives, so the orphan sweep leaves it alone, and the agent was written into it. A base VM (2 vCPUs, 2 GiB) booted from a clone of that template, with logging, the root drive, `eth0` on a tap in a network namespace of its own, MMDS V2 and vsock configured through the API before `InstanceStart`. Once its agent answered and it had idled for a while, it was paused and given a Full snapshot, its disk was snapshotted in Ceph, and it was killed without being resumed. Forks were each a new Firecracker process in a new namespace with the same tap name and the same guest address, on an RBD clone of the captured disk. Each was:
1. given logging before load;
2. loaded paused from the one snapshot (File memory backend);
3. given a fresh MMDS document;
4. resumed.

Batches of 1, 5 and 10 forks ran concurrently, after the snapshot had aged about a minute. The raw VMM ran outside the jailer for these; the memory-file and pinning checks then ran under the real jailer with an unprivileged uid.

## What was measured

| Question | Measured |
|---|---|
| Cold boot of the golden to its agent answering | 3.1 s (the runner image under systemd, with no application stack) |
| Full snapshot of a running 2 GiB guest | 4.0 s; a 2 GiB memory file and a 26 KB state file |
| `/snapshot/load`, paused | 4 to 5 ms |
| Process start to the fork's agent answering | 31 to 38 ms per fork |
| Wall time for a batch, including the RBD clone and map and the namespace | 1 fork 0.29 s, 5 forks 0.34 s, 10 forks 0.52 s |
| Memory per fork, a few seconds after resume | 11.4 to 11.6 MiB private dirty each; 10 forks were 1,210 MiB RSS but 227 MiB PSS together, against 504 MiB for the running base |
| A VMGenID device in the guest | yes: ACPI `VMGENCTR:00`, bound to the guest kernel's built-in `vmgenid` driver, in the base and in every fork |
| The guest kernel's random output across forks | different in 10 of 10 forks |
| A userspace RNG seeded before the snapshot | **the same value in every fork** (Python's `random`, seeded at agent start: one distinct value across 5 forks and across 10) |
| `/proc/sys/kernel/random/boot_id` across forks | **identical** in every fork, and equal to the base's |
| MMDS after a paused load | `GET /mmds` answered `200 {}`; a `PUT` before resume was what the guest then read |
| Logging and metrics configuration | before load `204`; after load `400` with "The requested operation is not supported after starting the microVM." for both `/logger` and `/metrics` |
| The same tap name and guest address in every fork | every fork answered HTTP 200 from inside its own namespace |
| An established vsock connection across a snapshot of a running VM | the next read returned end of file; a new connection after resume was answered, so the listener survived |
| The guest's wall clock after a restore | 64 to 75 s behind the host, for forks restored 60 to 70 s after the snapshot; consistent with a clock resuming near capture time, though its exact progression was not established |
| The memory file under the jailer | a hard link into the chroot, root-owned and mode 0444, restored and ran; the jailed uid could read it and could not open it for writing |
| Replacing the memory file by rename while a fork runs from it | the fork kept running and answered requests that touched its memory |
| Truncating the memory file in place while a fork runs from it | the VMM was gone within a second |
| `network_overrides` on load, pointing `eth0` at a tap with a different name | the fork answered HTTP 200 through the renamed tap |
| `vsock_override` on load | the agent answered on the overridden socket path |
| `clock_realtime: true` on load, guest on its default clock source | accepted (`204`), but the guest was still 68 to 150 s behind for snapshots aged that long; the guest's clock source was `tsc` (`kvm-clock` available, unused) |
| `clock_realtime: true` on load, golden booted with `clocksource=kvm-clock` (host on `tsc`, Linux 7.0) | the fork was 0.02 s from the host after more than a minute; cold boot to the agent was 3.9 s, and the fork's agent answered 118 ms after its process started |

## What it means for ADR-017

- **Speed and memory.**
  - A paused load took 4 to 5 ms, and a fork's agent answered 31 to 38 ms after its process started.
  - A few seconds after resume, each fork had 11.4 to 11.6 MiB of private dirty memory, and the ten forks together had 227 MiB PSS (the running base had 504 MiB RSS).
  - These cover the probe guest with no application stack. They do not establish the total host memory cost, or memory and readiness under an application's load, which are still to be measured.
- **Reseeding.**
  - The VMGenID driver was bound, and kernel random samples taken a few seconds after resume differed across ten forks. This probe did not establish that VMGenID caused a reseed, or when one happened, because other entropy sources were not controlled.
  - The Python RNG seeded before capture returned the same value in every sampled fork, which shows duplicated userspace state: the case the restore barrier exists for.
  - `boot_id` was identical in the base and in every sampled fork, so it cannot tell these forks apart. Whether userspace can change it was not tested.
- **Ordering.** MMDS and logging behave as the record states: logging before load, MMDS after load and before resume.
- **Network and vsock.**
  - Every tested fork answered HTTP from inside its own namespace, with the same tap name and guest address.
  - In the snapshot-and-resume check, an established vsock connection returned end of file, and a new connection after resume was answered.
  - Restoring with a changed guest CID was not tested. A different socket path through `vsock_override` was, and a different tap name through `network_overrides` was, and both worked.
- **Clock.** Without help, forks restored about a minute after capture were 64 to 75 s behind the host. That is consistent with a clock resuming near capture time, though its exact progression was not established. Firecracker's `clock_realtime` fixed it only when the guest kept time on `kvm-clock`: booting the golden with `clocksource=kvm-clock` brought a fork within 0.02 s. With the guest's default `tsc` it did nothing, which is the case its documentation describes. Only snapshots aged a minute or two were tried.
- **The memory file.** A golden's memory file can reach a jail exactly as the kernel does, by a root-owned, read-only hard link, with no bind mount. Rebuilds must write new files and rename, never rewrite one in place, because a running fork depends on the inode.

Not measured here: an application stack's readiness after restore, the restore barrier itself, a suspension's crash matrix, home isolation, fork copying, the compatibility refusal and admission. Those need the implementation.
