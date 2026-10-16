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
| The guest's wall clock after a restore | 64 to 75 s behind the host, for forks restored 60 to 70 s after the snapshot: it continues from the moment of capture |
| The memory file under the jailer | a hard link into the chroot, root-owned and mode 0444, restored and ran; the jailed uid could read it and could not open it for writing |
| Replacing the memory file by rename while a fork runs from it | the fork kept running and answered requests that touched its memory |
| Truncating the memory file in place while a fork runs from it | the VMM was gone within a second |

## What it means for ADR-017

- **Speed.** Restore is milliseconds and the cost of a fork is its dirty pages, so ten workspaces cost less than one booted guest. A golden's value is in skipping the application's own start, which this probe's golden did not have; the full-stack number is still to be measured.
- **Reseeding.** The kernel reseeds, as documented, but userspace state does not. One Python process produced the same "random" value in every fork, which is the case the restore barrier exists for. `boot_id` cannot be regenerated from userspace, so nothing may use it to tell forks apart.
- **Ordering.** MMDS and logging behave as the record states: logging before load, MMDS after load and before resume.
- **Network and vsock.** Namespaces with one tap name and one address work as Firecracker's clone guidance says. vsock drops established connections at every snapshot, the running VM's own included.
- **Clock.** The clock must be set before anything trusts it.
- **The memory file.** A golden's memory file can reach a jail exactly as the kernel does, by a root-owned, read-only hard link, with no bind mount. Rebuilds must write new files and rename, never rewrite one in place, because a running fork depends on the inode.

Not measured here: an application stack's readiness after restore, the restore barrier itself, a suspension's crash matrix, home isolation, fork copying, the compatibility refusal and admission. Those need the implementation.
