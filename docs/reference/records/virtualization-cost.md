# What the microVM costs

How much more time, CPU and energy two CI workloads took in billet's Firecracker microVMs than the same bytes in Docker containers or as plain processes on the same host, under the same CPU placement. Measured on 2026-11-14 on the reference deployment's Firecracker node (`ubuntu-01`: EPYC 7763, 128 hardware threads, 499 GiB, boost on, `scaling_max_freq` 2.45 GHz, Firecracker v1.16.1), drained, with the wall meter logged beside RAPL. Two harnesses ran: `TestRealVirtualizationCost` (the container and the process) and `TestRealPlacementExperiment` (the microVM), both in `internal/provider/firecracker`.

## The question and what would answer it

billet isolates every job in a microVM. The isolation is the reason to use one, and its price is the question here: per job, the extra wall time, CPU and energy against running the same work without a VM.

Each comparison used:
- the same workload bytes (`experimentBodies`);
- the same cpus, one thread of each of a CCD's first eight cores per instance, as `spread` places an 8-vCPU guest on an empty host;
- the same nominal memory, 8 GiB, swap off;
- the same root filesystem, the golden generation `ubuntu-2404-x64@g20261109002634`.

**The comparison** is the configuration as each would really run:
- the microVM: as billet runs it, booted from a clone of the golden on Ceph;
- the container: from an image of that golden's own files;
- the process: in a chroot of them.

It is an end-to-end configuration comparison, not an isolation of the hypervisor. The kernels, storage, memory limits and measurement intervals differ between the modes, as stated under **What each number covers**.

## How it was measured

**The session.**
- **Draining.** The whole deployment was drained (18:06 to 18:31 UTC).
- **Runs.** Three repetitions of each workload, the three modes in a shuffled order each time, 20 seconds apart: 18 runs, all of which passed.
- **Instances.** Each run started 4 instances of 8 vCPUs together, on cpus 0-7, 8-15, 16-23 and 24-31.
- **The wall meter** is the Shelly plug on the host's power cord, logged about once a second (`power-log.sh`), beside the package's RAPL counter.
- **Energy is integrated over the time between samples**, which were a median 1.07 s apart and up to 1.83 s, never counted as one second each.

**Idle.** Idle is the last 90 seconds of two quiet minutes before the first run: 168.1 W at the wall and 78.9 W at the package. The wall readings in those 90 seconds ranged from 148.9 to 198.5 W. As a sensitivity check, every run's energy is also given above the ten seconds before its own start, which ranged from 150.7 to 184.0 W at the wall across runs.

**The workloads.**
- **gostd** builds the Go standard library from scratch twice (`go build -a std`), each into a fresh cache, with as many processes as the instance has cpus. Its instructions per cycle were 1.33 in the microVM.
- **memory** runs one Python worker per cpu, each copying a 256 MiB buffer 600 times. It stalls on memory, at 0.10 instructions per cycle in the microVM. It is not a compute-bound load.

**The microVM.** A guest of 8 vCPUs and 8 GiB booted from a Ceph clone of the golden (`TestRealPlacementExperiment`, `spread`). The workload ran when the guest's agent started it, and the guest rebooted itself when the workload ended, which ends its VMM. Its root, and so its `/tmp`, is the clone on Ceph.

**The container.** It ran from an image imported from the golden's own filesystem, keeping only what the workloads use: the base system, its libraries, gcc and its headers, Python and the Go tool cache.
- **Limits:** `--cpuset-cpus` and `--memory`/`--memory-swap` 8 GiB, with the root read-only and no log.
- **Scratch:** `/tmp` on a 64 GiB tmpfs the run mounted.

**The process.** It ran as a systemd transient service, confined the same way:
- `AllowedCPUs` and `MemoryMax` 8 GiB, with swap off;
- in a mount namespace of its own;
- chrooted into a read-only mount of the golden's snapshot, mapped read-only from Ceph;
- with `/tmp` on the same tmpfs.

**Measurement.** Every instance was measured by billet's own sampler (`usage.Monitor`), about once a second, through its cgroup. A host instance waited for the sampler to confirm a baseline that read its CPU before its workload started, and held its cgroup until the final reading was taken.

## Results

Medians over the three runs of each run's mean per instance. Each run's three values are in brackets.

| Workload | Mode | Run time per instance | CPU per instance, as sampled | Host memory at peak, per instance | Wall energy above idle, per run of 4 | Package energy above idle, per run of 4 | Wall / package above each run's own pre-run baseline |
|---|---|---|---|---|---|---|---|
| gostd | process | 18.2 s (19.0, 18.2, 18.1) | 107.5 s | 0.86 GiB | 1.75 kJ (1.75, 1.70, 2.00) | 1.52 kJ | 2.01 / 1.66 kJ |
| gostd | container | 18.3 s (18.3, 18.1, 18.3) | 107.5 s | 0.86 GiB | 2.22 kJ (2.26, 1.93, 2.22) | 1.81 kJ | 2.26 / 1.78 kJ |
| gostd | microVM | 37.7 s (51.5, 37.4, 37.7) | 136.5 s | 3.19 GiB | 3.08 kJ (3.08, 2.55, 3.24) | 2.41 kJ | 2.86 / 2.33 kJ |
| memory | process | 75.6 s (76.4, 75.6, 75.6) | 598.5 s | 4.03 GiB | 9.02 kJ (9.91, 8.97, 9.02) | 5.66 kJ | 9.90 / 5.89 kJ |
| memory | container | 76.4 s (76.4, 76.7, 76.3) | 602.6 s | 4.03 GiB | 9.97 kJ (9.80, 10.53, 9.97) | 6.30 kJ | 11.29 / 6.95 kJ |
| memory | microVM | 82.2 s (82.2, 81.6, 83.7) | 612.8 s | 5.61 GiB | 10.13 kJ (10.13, 10.12, 10.65) | 6.38 kJ | 9.65 / 6.02 kJ |

**The microVM's launch.** From `Launch` being called to its VMM running took a median 2.4 s (gostd) and 2.3 s (memory) per guest, outside its run time.

**Energy windows.** Each run's energy is over its own window, from its start to its last instance's exit. The wall log has every second of every window but one: the third memory microVM run is missing one meter reading. Its wall figures therefore cover 86.3 of its 87.4 seconds, and are low by an estimated 0.13 kJ.

**The first microVM run.** The first gostd microVM run took 51.5 s, against 37.4 and 37.7 s for the other two. It was the session's first boot of the generation; why it was slower was not measured.

## What each number covers

- **Run time** is measured over a different interval in each kind of mode:
  - a host mode's starts when its workload is let go and ends when it exits;
  - the microVM's starts when its VMM is running and ends when the guest has rebooted. That includes the guest kernel's boot, the agent's setup, the workload and the shutdown.

  None of these was timed apart, so the microVM's extra time is not divided between its start and end and the workload.
- **CPU** is the cgroup's total as billet's sampler recorded it:
  - the microVM's covers the guest's vCPUs and the VMM's own threads;
  - a host instance's covers its workload and the wrapper that held the cgroup.

  Every microVM instance's series carries a `failed_read` gap: the sampler's last read, after the VMM exited, could not be made, so its total may miss up to its last sample interval. The host instances carry only `final_energy`, the last interval's energy not being apportioned.
- **Energy above idle** is the whole host's at the wall and at the package, over each run's window. It is not billet's attribution to each instance.
- **Host memory at peak** measures different things in each mode:
  - the microVM's is its VMM's, which holds the guest's touched RAM, its page cache included;
  - a host instance's is its cgroup's, where the pages a compile wrote to the tmpfs are charged to the instance that wrote them.

  The 8 GiB is guest RAM for the microVM and a cgroup limit for the others.
- **Storage differed, and was not measured.** The microVM's root and scratch are a Ceph clone. The host modes read the golden from a read-only mount or an image on the host's disk, and wrote to a tmpfs. Neither the bytes the workloads wrote nor any writeback was measured, so storage may account for part of every difference here.

## What it means

- **On gostd, a short compile, the microVM took about twice as long.**
  - Its run time was 19.5 s longer than the process's, against 18.2 s of work.
  - It used 29 CPU-seconds more per instance.
  - At the wall it drew 1.3 kJ more above idle for 4 guests, or 0.9 kJ above each run's own baseline.

  How that splits between the guest's boot and shutdown and the workload is not measured.
- **On memory, a memory-stalled copy, the microVM used 2.4% more CPU** than the process (612.8 s against 598.5 s per instance), and its run time was 6.6 s longer.
  - Its wall energy against the other two is not resolved. Above the quiet idle the microVM drew the most at the wall (10.13 kJ against 9.97 and 9.02).
  - Above each run's own baseline it drew the least at the wall (9.65 kJ against 11.29 and 9.90). At the package, above the same baselines, it sat between the process and the container (6.02 kJ against 5.89 and 6.95).
  - The idle wall readings alone ranged over about 50 W.

  This result cannot establish the overhead on a compute-bound job.
- **The container and the process took about the same time and CPU, within about 1%.** Their energy differed by less than its spread across baselines.
- **The microVM holds more host memory**, 3.2 GiB per guest against 0.86 GiB on gostd, because a guest's own page cache and the RAM its kernel touched stay in its VMM. billet charges a tier for its guest RAM, not for what the VMM's resident set grows to, so this is headroom the node needs, not a cost a job is billed.

## What it does not establish

- **One host, one shape.** It covers one host, 4 instances of 8 vCPUs, and two workloads. The memory workload is not compute-bound, and gostd's bottleneck was not classified.
- **No separation of the overhead.** It does not separate the hypervisor's own overhead from the guest's boot, its kernel, or its storage, and it measured no storage traffic.
- **Too few runs for small differences.** Each figure is three runs, and the energy baseline varied by tens of watts, so a difference smaller than a mode's own spread is not a finding.
- **No restore.** The microVM's start is billet's cold boot. Starting from a snapshot instead was not tested on these workloads.
