# What the microVM costs

How much more time, CPU and energy a CI workload takes in one of billet's Firecracker microVMs than the same bytes in a Docker container or as a plain process on the same host, under the same CPU placement. Measured on 2026-11-14 on the reference deployment's Firecracker node (`ubuntu-01`: EPYC 7763, 128 hardware threads, 499 GiB, boost on, `scaling_max_freq` 2.45 GHz, Firecracker v1.16.1), drained, with the wall meter logged beside RAPL. Two harnesses ran: `TestRealVirtualizationCost` (the container and the process) and `TestRealPlacementExperiment` (the microVM), both in `internal/provider/firecracker`.

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

It is an end-to-end configuration comparison, not an isolation of the hypervisor's instruction overhead. What separates the two is stated under **What each number covers** below.

## How it was measured

**The session.**
- **Draining and idle.** The whole deployment was drained (18:06 to 18:31 UTC), and two quiet minutes gave this session's idle: 168.3 W at the wall and 84.5 W at the package.
- **Runs.** Three repetitions of each workload, the three modes in a shuffled order each time, 20 seconds apart: 18 runs, all of which passed.
- **Instances.** Each run started 4 instances of 8 vCPUs together, on cpus 0-7, 8-15, 16-23 and 24-31.
- **The wall meter** is the Shelly plug on the host's power cord, read once a second (`power-log.sh`).

**The workloads.**
- **gostd** builds the Go standard library from scratch twice (`go build -a std`), each into a fresh cache, with as many processes as the instance has cpus.
- **memory** runs one Python worker per cpu, each copying a 256 MiB buffer 600 times.

**The microVM.** A guest of 8 vCPUs and 8 GiB booted from a Ceph clone of the golden (`TestRealPlacementExperiment`, `spread`). The workload ran when the guest's agent started it, and the guest rebooted itself when the workload ended, which ends its VMM. Its root, and so its `/tmp`, is the clone on Ceph.

**The container.** It ran from an image imported from the golden's own filesystem, keeping only what the workloads use: the base system, its libraries, gcc and its headers, Python and the Go tool cache.
- **Limits:** `--cpuset-cpus` and `--memory`/`--memory-swap` 8 GiB, with the root read-only and no log.
- **Scratch:** `/tmp` on a 64 GiB tmpfs the run mounted.

**The process.** It ran as a systemd transient service, confined the same way:
- `AllowedCPUs` and `MemoryMax` 8 GiB, with swap off;
- in a mount namespace of its own;
- chrooted into a read-only mount of the golden's snapshot, mapped read-only from Ceph;
- with `/tmp` on the same tmpfs.

**Measurement.** Every instance was measured by billet's own sampler (`usage.Monitor`), once a second, through its cgroup. A host instance waited for the sampler to confirm a baseline that read its CPU before its workload started, and held its cgroup until the final reading was taken.

## Results

Medians over the three runs of each run's mean per instance. Each run's three values are in brackets.

| Workload | Mode | Run time per instance | CPU per instance (its cgroup) | Wall energy above idle, per run of 4 | Package energy above idle, per run of 4 | Host memory at peak, per instance |
|---|---|---|---|---|---|---|
| gostd | process | 18.2 s (19.0, 18.2, 18.1) | 107.5 s | 1.62 kJ (1.62, 1.54, 1.88) | 1.53 kJ | 0.86 GiB |
| gostd | container | 18.3 s (18.3, 18.1, 18.3) | 107.5 s | 2.03 kJ (2.11, 1.77, 2.03) | 1.81 kJ | 0.86 GiB |
| gostd | microVM | 37.7 s (51.5, 37.4, 37.7) | 136.5 s | 2.85 kJ (2.85, 2.35, 2.99) | 2.41 kJ | 3.19 GiB |
| memory | process | 75.6 s (76.4, 75.6, 75.6) | 598.5 s | 8.23 kJ (9.15, 8.23, 8.23) | 5.72 kJ | 4.03 GiB |
| memory | container | 76.4 s (76.4, 76.7, 76.3) | 602.6 s | 9.14 kJ (8.95, 9.62, 9.14) | 6.36 kJ | 4.03 GiB |
| memory | microVM | 82.2 s (82.2, 81.6, 83.7) | 612.8 s | 9.42 kJ (9.42, 9.37, 9.70) | 6.40 kJ | 5.61 GiB |

- **The microVM's launch.** From `Launch` being called to its VMM running took a median 2.4 s (gostd) and 2.3 s (memory) per guest, outside its run time.
- **Energy windows.** Each run's energy is over its own window, from its start to its last instance's end, with this session's idle subtracted every second.
- **The first microVM run.** The first gostd microVM run took 51.5 s, against 37.4 and 37.7 s for the other two. It was the session's first boot of the generation, and is the one run whose reads of the golden were not yet cached.

## What each number covers

- **Run time** is measured over a different interval in each kind of mode:
  - a host mode's starts when its workload is let go and ends when it exits;
  - the microVM's starts when its VMM is running and ends when the guest has rebooted. That includes the guest kernel's boot, the agent's setup, the workload and the shutdown.

  So the microVM's figure carries a fixed start and end that the others do not. The difference between the modes is that fixed cost plus anything the VM adds to the workload itself.
- **CPU** is everything the instance's cgroup ran over its life:
  - for the microVM: the guest's vCPUs, booting and shutting down included, and the VMM's own threads;
  - for the host modes: the workload, and the wrapper that held the cgroup before and after it.
- **Energy above idle** is the whole host's at the wall and at the package, over each run's window. It is not billet's attribution to each instance.
- **Host memory at peak** measures different things in each mode:
  - the microVM's is its VMM's, which holds the guest's touched RAM, its page cache included;
  - a host instance's is its cgroup's, where the pages a compile wrote to the tmpfs are charged to the instance that wrote them.

  The 8 GiB is guest RAM for the microVM and a cgroup limit for the others.
- **Storage differed.** The microVM's root and scratch are a Ceph clone, while the host modes read the golden from a read-only mount or an image on the host's disk and wrote to a tmpfs. Both workloads read their tools once and wrote little: gostd's caches stay in page cache, and memory writes nothing.

## What it means

- **A steady, CPU-bound job pays a few percent.** On the memory workload, the microVM used 2.4% more CPU than the process (612.8 s against 598.5 s per instance), 3.1% more wall energy than the container and 14% more than the process, whose own runs spread from 8.23 to 9.15 kJ. That includes its boot and shutdown, so the hypervisor's own cost on the work itself is at most that.
- **A short job pays mostly for the boot.** On gostd, whose work takes 18 s, the microVM took about 19 s longer and 1.2 kJ more at the wall than the process for 4 guests, about 0.3 kJ each. Its CPU was 29 s more per guest, which is the guest's boot, its agent and its shutdown. That cost is fixed per job, and the longer the job the smaller its share: about 8% of the memory workload's run time against half of gostd's.
- **The container is not free either.** It drew more wall energy than the plain process on both workloads: 2.03 against 1.62 kJ on gostd and 9.14 against 8.23 kJ on memory, both medians. The process's own runs ranged 1.54 to 1.88 and 8.23 to 9.15 kJ, so the difference is about the size of the run-to-run spread. The time and CPU of the two were the same within 1%.
- **The microVM holds more host memory**, 3.2 GiB per guest against 0.86 GiB on gostd, because a guest's own page cache and the RAM its kernel touched stay in its VMM. billet charges a tier for its guest RAM, not for what the VMM's resident set grows to, so this is headroom the node needs, not a cost a job is billed.

## What it does not establish

- **One host, one shape.** It covers one host, 4 instances of 8 vCPUs, and two workloads that do little I/O. A job that reads and writes its disk heavily would add the difference between a Ceph clone and the host's own disk, which this does not separate from the VM.
- **No idle VM.** The microVM's boot is billet's cold boot. [Starting a runner from a snapshot](snapshot-start.md) measured what restoring instead of booting saves at a job's start, which is most of gostd's gap here.
- **Three runs each.** Each figure is three runs, so a difference smaller than a mode's own spread, as between the container and the process, is not a finding.
