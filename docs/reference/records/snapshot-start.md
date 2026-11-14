# Starting a runner from a snapshot

How much sooner a GitHub Actions job starts, and how much less memory its guest holds, when billet restores the runner's microVM from a snapshot rather than booting it cold. Measured on 2026-11-14 on the reference deployment's Firecracker node (`ubuntu-01`: EPYC 7763, 128 hardware threads, 499 GiB, Firecracker v1.16.1, guest kernel 6.1.155, golden `ubuntu-2404-x64@g20261109002634`), beside its production guests, with `TestRealSnapshotStartExperiment` (`internal/provider/firecracker`). Each job was real: a `queue` job of the `billet-testbed/jobs` repository, which sleeps for 30 seconds, run by a just-in-time runner registered on the `billet-testbed` organization.

## The question and what would answer it

[Workspace snapshots](workspace-snapshots.md) measured the primitive by hand: a paused load in 4 to 5 ms, and a probe agent answering 31 to 38 ms after its VMM started. Neither of those is a job. The question here is what restoring buys a CI job end to end, through billet's own provider: from billet being asked for a guest to that guest's runner holding its registration, and from there to GitHub starting the job; and what the guest's VMM holds in host memory at the same point.

What would count as success:
- a restored runner holding its registration well under a second after billet is asked, at the median, against the several seconds a cold boot spends booting and setting up its guest;
- GitHub starting the job measurably sooner;
- the restored VMM holding less host memory at the same instruction.

The comparison is billet's existing method, a cold boot of the same golden generation with the same command, run in the same session and interleaved with the restores.

## How it was measured

**The golden.** One guest of 2 vCPUs and 4 GiB was launched from the generation, booted with `clocksource=kvm-clock` (`withKVMClock`). Its JIT configuration was a placeholder, and its command was the experiment's waiter (`snapshotStartWaiter`). The guest's agent did everything it does before a runner starts, then ran the waiter. The waiter:
- reported `waiting` to the host;
- asked the metadata service, with a fresh token each time, for a registration other than the placeholder.

The golden was captured with `Capture` once the host received `waiting`. The builder was left paused, and so kept its tap, until the end.

**The trials.** Twenty trials alternated a cold `Launch` and a `Restore` of that capture, one at a time, cold first. Each was given a JIT configuration minted beforehand for a runner whose label only that trial's job asked for, and that job was already queued. Both arms ran the same waiter:
- a restored guest resumed into its golden's waiter, which found the fork's registration in the metadata `Restore` had installed;
- a cold guest found its registration at once.

The waiter reported `registered` with the guest's clock, then held the runner until the host had read the VMM's `/proc/<pid>/smaps_rollup` and released it. Then it reported `released` in the background, ran the runner, and reported `finished`. Every load passed `clock_realtime`.

**Measured at:**
- `Launch` or `Restore` being called, and when it answered;
- the host receiving the waiter's `registered`, on the host's clock;
- the VMM's memory then, 30 seconds after the release, and at its peak while the job ran;
- GitHub's `started_at` for the job, joined afterwards by the runner's name, which has one-second resolution.

**The guest reports to the host.** The node's firewall drops everything a guest sends the host except DNS, DHCP, the node's cache port and the image verification port. The experiment's first attempt that day polled a listener inside the guest, and failed: the guest leased an address and answered nothing. So the host listened on the image verification port, holding `billet images verify`'s own lock. It kept steps only for the key of the trial in progress, the sha256 of its registration.

**Conditions.** The node ran its production guests throughout. Its one-minute load average was 38 to 63, and is recorded on every trial. Each fork came up on the golden's MAC on a tap of its own, which the bridge's forwarding table confirmed on every restore. The forks were restored 56 to 852 seconds after the capture.

## Results

| Measure (median, min to max, 10 trials each) | Cold boot | Restore |
|---|---|---|
| `Launch` / `Restore` answered | 792 ms (394 to 3,653) | 498 ms (432 to 2,222) |
| Runner had its registration, host's clock | 6,545 ms (5,085 to 13,375) | 756 ms (647 to 2,443) |
| Runner had its registration, after `Launch` / `Restore` answered | 5,335 ms | 219 ms |
| GitHub started the job, after billet was asked | 10 s (8 to 16) | 5 s (4 to 6) |
| Guest's stamp before the host received it (clock offset and trip) | 36 ms (35 to 40) | 53 ms (40 to 73) |
| VMM's PSS while the runner was held at its registration | 651 MiB (646 to 655) | 310 MiB (219 to 345) |
| of which private dirty | 651 MiB | 58 MiB (37 to 82) |
| of which file-backed (the snapshot's memory file) | 2 MiB | 250 MiB (184 to 277) |
| VMM's PSS 30 s into the job | 798 MiB (788 to 821) | 508 MiB (395 to 608) |
| VMM's peak PSS during the job | 799 MiB (797 to 821) | 519 MiB (407 to 608) |
| Jobs GitHub reported as succeeded | 10 of 10 | 10 of 10 |

**The golden.** It booted to its waiter in 8.3 s, and its capture took 18.4 s, writing a 4 GiB memory file.

**Clean teardown.** Every trial's guest was destroyed before the next began. After the run, no jail, clone, golden disk, snapshot, mapping, listener or runner registration was left.

## What it means

- **A restored runner is ready in under a second.** The median was 0.76 s from billet being asked to the runner holding its registration, against 6.5 s for a cold boot. A fork spent a median 219 ms between its `Restore` answering and its registration: the time to resume, read its metadata and report. A cold guest spent 5.3 s booting its kernel and running its agent's setup.
- **GitHub started the job about 5 seconds sooner.** The median was 5 s against 10 s, measured at GitHub's one-second resolution. The roughly 4 s between a runner's registration and the job's start is GitHub's own, and is the same in both arms.
- **The restored guest holds about half the host memory, most of it shareable.** A fork's VMM held 310 MiB at its registration, against 651 MiB cold, and only 58 MiB of that was private dirty. The other 250 MiB was pages of the snapshot's memory file, which every fork restored from one capture maps from the same read-only file. These forks ran one at a time, so each was charged all of those pages. Concurrent forks would share them, as the ten concurrent forks in [Workspace snapshots](workspace-snapshots.md) did: 227 MiB PSS together. During the job the gap stayed at about 290 MiB.
- **The clock was right.** The guest's stamp preceded the host's receipt by 35 to 73 ms in both arms, which is the report's trip, so restored guests kept time with the host as cold ones did. `clock_realtime` on every x86 load and `kvm-clock` in the golden are what ADR-017 relies on, and both are now in the provider.

## What it does not establish

- **The guests were not unique.** The restore barrier [ADR-017](../decisions/adr-017-workspaces.md) requires is not implemented: forks of one golden shared its machine-id, `boot_id`, Docker daemon identity and anything else its userspace held. That is acceptable for a measurement, and is not how billet may run untrusted work.
- **The comparison covers one host, one shape and one kind of job.** A 2 vCPU, 4 GiB guest ran a job that only sleeps, beside a production load average of 38 to 63. A job that builds would spend the same few seconds less at its start, and its own time after that.
- **Snapshot age was bounded.** Forks were restored at most 14 minutes after the capture, within one golden's life, and a golden's age beyond that was not tried.
- **Restore is not wired into scheduling.** Restoring runners for real jobs still needs a golden per tier and image, rebuilt when the image changes, and the restore barrier before untrusted work. This measures what that would buy.
