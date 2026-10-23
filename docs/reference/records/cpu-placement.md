# CPU placement and idle power on the reference node

What placing microVMs on host cpus, and capping the cpu while a node is idle, did to job run time and to power. It was measured on 2026-10-23 in three sessions on the reference deployment's Firecracker node, each with the deployment drained so nothing else ran there: 05:54 to 07:04 UTC, 14:44 to 16:07 UTC, and 16:14 to 16:46 UTC.

## The host and how it was measured

- **Host:** `ubuntu-01`.
  - **Hardware:** AMD EPYC 7763, 64 cores and 128 threads in 8 CCDs. Each CCD is 8 cores sharing 32 MB of L3, and cpu N's SMT sibling is cpu N+64.
  - **Software:** Linux 7.0.0-38, `acpi-cpufreq` with `schedutil`, P-states 2.45, 2.0 and 1.5 GHz, boost to 3.53 GHz. Firecracker v1.16.1.
  - **Guest:** the golden generation `ubuntu-2404-x64@g20261023004315`.
  - **Wall meter:** a Shelly Plug US Gen4 on the host's one power cord.
- **Harness:** `TestRealPlacementExperiment` in `internal/provider/firecracker`. It runs real guests through billet's own provider under a separate deployment identity, measured by the node's own sampler. Every guest has 8 GiB, and 8 vCPUs unless a section says otherwise.
- **Workloads:** both need no network.
  - `gostd` is `go build -a std` twice, each from an empty cache: a parallel compile.
  - `memory` is every vCPU copying a 256 MiB buffer 600 times: far larger than any L3.
  - A guest reboots when its workload succeeds, which ends its VMM, so run time is from launch returned to VMM exit.
- **Power:** `scripts/power-log.sh --meter` logged package energy (RAPL) and the wall meter once a second through each session (3,633, 4,289 and 1,681 rows). turbostat agreed with RAPL at 0.999, 0.972 and 0.981.
- **Placements:**
  - `none` is today's unpinned guests.
  - `compact` gives a guest both threads of as few cores as it needs, in the lowest last-level-cache group with room for it.
  - `spread` gives it one thread per core in the emptiest group.
  - `packed` gives it one thread per core, from the lowest group with a free core onward.
- **Runs:**
  - Session 1: `none`, `compact` and `spread` at 4 and 8 guests, both workloads, three repeats in a shuffled order, then two boost runs each way. 40 runs and 232 guests.
  - Session 2: `compact`, `packed` and `spread` the same way, then three boost runs each way under `spread` at 4 and 8 guests. 48 runs and 288 guests.
  - Session 3: `compact`, `packed` and `spread` with 8 guests of 4 vCPUs, both workloads, three repeats, then `compact` and unpinned with 12 guests of 8 vCPUs on the compile, three repeats. 24 runs and 216 guests.
  - No guest failed.

## Results

### No cpu ceiling lowered idle power

Session 2 read each setting three times for 60 s, interleaved and shuffled, 10 s trimmed from each end, after five quiet minutes that read 78.9 W in the package and 149.9 W at the wall.

| Setting | Package, three readings | Package mean | Wall mean |
|---|---|---|---|
| Default (boost on, 2.45 GHz ceiling) | 86.6, 78.8, 78.7 W | 81.4 W | 155.3 W |
| Boost off | 85.8, 78.8, 78.8 W | 81.1 W | 153.9 W |
| Boost off, 2.0 GHz ceiling | 83.7, 81.2, 78.8 W | 81.2 W | 153.9 W |
| Boost off, 1.5 GHz ceiling | 81.9, 79.2, 79.0 W | 80.0 W | 150.5 W |

- **No setting lowered idle power.** Every setting read high in the first round and within 2.5 W of the quiet reading after it, so the spread between settings (under 1.5 W in the means) is smaller than the spread within one. Session 1's single readings (78.8 to 82.7 W) agree.
- **That fits what is known of this chip:** an idle core sits in a C-state whatever its ceiling, and most of the idle draw is outside the cores (about 69 of 73.5 W was uncore in the package's own split in September).
- **So capping a node's frequency while it is idle, which `node.power` does, showed no saving on this host.** Whatever it saves would have to show in how a host leaves and enters idle, which this did not measure.
- **This host idled at 78.9 W in sessions 1 and 2, and at 85.5 W in session 3,** which began seven minutes after session 2's last guest, against 69.9 W in the calibration run at 02:50 the same night. In session 2's quiet minutes the cores were idle (vmstat read 100% idle, about 2,000 to 2,500 interrupts a second, and no microVM ran), so the difference is not processes doing work. What it is was not measured.

### node.power's mechanism on the real host

`TestRealCapAndRestoreOnThisHost` (`internal/power`) ran the controller against the host's own sysfs in session 2: it capped the idle node, read what changed, restored through `RestoreBeforeLaunch` as a launch does, and checked every value was back.

| Settings | Cycles | Restore before a launch | Values wrong afterwards |
|---|---|---|---|
| Boost off | 5 | 19.2 to 22.3 ms | none |
| Boost off and a 1.5 GHz ceiling | 5 | 20.0 to 21.8 ms | none |

- **A launch waits about 20 ms for the full ceiling:** every policy's `scaling_max_freq` and boost switch and the global boost switch, all compared afterwards with what the host held before.
- **Turning boost off moves only the boost switch here:** `scaling_max_freq` read 2.45 GHz while boost was off, and 1.5 GHz only when the ceiling was set.

### Spread was fastest on the compile; compact was slowest everywhere

Session 2, means of three repeats. Run time is per guest. Energy is the whole host over the run's window.

| Workload | Guests | Placement | Run time | sd | Package | Wall | IPC |
|---|---|---|---|---|---|---|---|
| gostd | 4 | compact | 45.2 s | 0.1 | 6.15 kJ | 9.99 kJ | 0.93 |
| gostd | 4 | packed | 31.8 s | 0.2 | 4.94 kJ | 7.66 kJ | 1.33 |
| gostd | 4 | spread | 32.0 s | 1.9 | 5.01 kJ | 7.87 kJ | 1.33 |
| gostd | 8 | compact | 55.2 s | 5.8 | 9.05 kJ | 14.07 kJ | 0.93 |
| gostd | 8 | packed | 40.7 s | 1.5 | 7.97 kJ | 12.15 kJ | 1.27 |
| gostd | 8 | spread | 40.2 s | 1.8 | 7.95 kJ | 12.02 kJ | 1.27 |
| memory | 4 | compact | 103.3 s | 0.1 | 14.43 kJ | 24.85 kJ | 0.08 |
| memory | 4 | packed | 81.0 s | 0.3 | 12.87 kJ | 21.40 kJ | 0.10 |
| memory | 4 | spread | 81.4 s | 0.2 | 12.57 kJ | 20.95 kJ | 0.10 |
| memory | 8 | compact | 153.4 s | 0.3 | 24.43 kJ | 41.44 kJ | 0.05 |
| memory | 8 | packed | 147.6 s | 0.8 | 28.30 kJ | 38.20 kJ | 0.06 |
| memory | 8 | spread | 147.4 s | 0.0 | 28.18 kJ | 37.99 kJ | 0.06 |

Session 1, with unpinned guests in place of `packed`:

| Workload | Guests | Placement | Run time | sd | Package | Wall | IPC |
|---|---|---|---|---|---|---|---|
| gostd | 4 | none | 36.9 s | 6.6 | 5.44 kJ | 8.60 kJ | 1.26 |
| gostd | 4 | compact | 46.5 s | 1.3 | 6.17 kJ | 10.02 kJ | 0.93 |
| gostd | 4 | spread | 32.5 s | 0.9 | 4.91 kJ | 7.66 kJ | 1.33 |
| gostd | 8 | none | 44.7 s | 3.7 | 8.66 kJ | 13.35 kJ | 1.16 |
| gostd | 8 | compact | 54.0 s | 5.1 | 9.01 kJ | 13.92 kJ | 0.93 |
| gostd | 8 | spread | 41.5 s | 0.5 | 7.93 kJ | 11.94 kJ | 1.28 |
| memory | 4 | none | 79.4 s | 0.8 | 12.34 kJ | 20.47 kJ | 0.12 |
| memory | 4 | compact | 103.5 s | 0.1 | 14.14 kJ | 24.38 kJ | 0.08 |
| memory | 4 | spread | 82.2 s | 0.5 | 12.70 kJ | 21.05 kJ | 0.10 |
| memory | 8 | none | 148.7 s | 1.0 | 27.00 kJ | 43.12 kJ | 0.06 |
| memory | 8 | compact | 154.5 s | 0.3 | 24.98 kJ | 42.20 kJ | 0.05 |
| memory | 8 | spread | 148.5 s | 0.2 | 28.36 kJ | 38.10 kJ | 0.06 |

- **The two sessions agree:** every `compact` and `spread` configuration they share ran within 3% of the other session's time.
- **`spread` against unpinned guests on the compile (session 1):** 12% shorter run time and 10% less package energy at 4 guests, 7% and 8% at 8 guests, and a standard deviation of 0.5 to 0.9 s against 3.7 to 6.6 s unpinned.
- **`compact` was the slowest in every configuration of both sessions:** 41% longer than `spread` on the compile at 4 guests, with IPC 0.93 against 1.33. It gives a guest's 8 vCPUs only 4 physical cores, both SMT siblings of each.
- **With 8-vCPU guests, `packed` and `spread` are the same placement on this host:** each gives every guest the 8 cores of one whole CCD (the recorded cpu sets show it), and they ran the same. So this matrix separates nothing between them; session 3 does, below.
- **The memory workload stalls on memory** (IPC 0.05 to 0.12, about 35 last-level misses per 1,000 instructions); that it is bound by memory bandwidth is an interpretation of those counters, since the harness measures no bandwidth. At 8 guests every placement but `compact` ran the same, and `compact` used the least package energy but the most wall energy, so the energy outside the package differed between placements in a way these instruments cannot place.

### Sharing a CCD's L3 against sharing SMT siblings

Session 3 used 8 guests of 4 vCPUs, so the three placements differ in one thing at a time. The recorded cpu sets show the layouts. `spread` gives each guest 4 cores of a CCD of its own (8 CCDs, no shared L3, no shared cores). `packed` puts two guests in each of 4 CCDs (a shared L3, no shared cores). `compact` puts four guests in each of 2 CCDs on both threads of 2 cores each (a shared L3 and shared cores).

| Workload | Placement | Run time | sd | Package | Wall | IPC |
|---|---|---|---|---|---|---|
| gostd | compact | 57.7 s | 1.0 | 8.94 kJ | 13.86 kJ | 0.91 |
| gostd | packed | 50.5 s | 1.1 | 8.60 kJ | 13.07 kJ | 1.33 |
| gostd | spread | 49.6 s | 0.9 | 8.51 kJ | 13.10 kJ | 1.38 |
| memory | compact | 103.5 s | 0.3 | 14.36 kJ | 24.71 kJ | 0.09 |
| memory | packed | 80.0 s | 0.0 | 12.67 kJ | 21.01 kJ | 0.11 |
| memory | spread | 76.5 s | 0.2 | 12.70 kJ | 20.62 kJ | 0.12 |

- **Sharing a CCD's L3 cost little:** `packed` ran the compile 1.8% longer than `spread`, about one standard deviation, and the memory copy 4.6% longer.
- **Sharing SMT siblings is what costs:** `compact` ran the compile 16% and the memory copy 35% longer than `spread`, with IPC 0.91 against 1.38.
- **Leaving whole CCDs idle saved no energy:** `packed` left four CCDs with nothing on them and used the same package and wall energy as `spread` to within 1% on both workloads. That is consistent with the idle result above: on this host an idle core costs little whether or not its CCD has work.

### At 75% of the threads, compact and unpinned tie

Session 3 then ran 12 guests of 8 vCPUs, 96 of 128 threads, on the compile. `spread` and `packed` can give whole cores to only 8 such guests, and a guest the placement cannot fit runs unpinned, so they were not run; `compact` fits all 12.

| Placement | Run time | sd | Package | Wall | IPC |
|---|---|---|---|---|---|
| none | 56.5 s | 6.2 | 11.18 kJ | 16.90 kJ | 1.00 |
| compact | 55.4 s | 1.6 | 11.03 kJ | 16.65 kJ | 0.91 |

- **At this load the two ran the same,** within 2% in time and energy: 96 vCPUs need more threads than the host has cores, so unpinned guests share SMT siblings too.
- **`compact`'s run times varied a quarter as much,** a standard deviation of 1.6 s against 6.2 s.

### Boost off saved energy under compact and cost it under spread

| gostd | Boost | Runs | Run time | Package | Wall |
|---|---|---|---|---|---|
| 4 guests, `compact` (session 1) | on | 2 | 44.7 s | 6.12 kJ | 9.97 kJ |
| 4 guests, `compact` (session 1) | off | 2 | 44.1 s | 5.18 kJ | 8.70 kJ |
| 4 guests, `spread` (session 2) | on | 3 | 31.8 s | 4.94 kJ | 7.74 kJ |
| 4 guests, `spread` (session 2) | off | 3 | 47.8 s | 5.99 kJ | 9.97 kJ |
| 8 guests, `spread` (session 2) | on | 3 | 40.9 s | 7.84 kJ | 11.84 kJ |
| 8 guests, `spread` (session 2) | off | 3 | 67.1 s | 9.30 kJ | 15.07 kJ |

- **Under `spread`, boost off made the compile 50% slower at 4 guests and 64% slower at 8, and it used 21% and 19% more package energy.** The longer run cost more than the lower clock saved.
- **Under `compact`, boost off cost no time and saved 15% of package energy,** in two runs: a guest whose cores share SMT siblings gained little from boost to begin with.
- **So turning boost off is not a saving on this host for a job that has whole cores to itself,** which is what `spread` gives it. `node.power` turns boost off only while a node is idle, where the section above found it saves nothing, and restores it before a launch.

### Energy attribution

| Measure, over session 2's 48 runs | Mean | Range |
|---|---|---|
| Energy charged to guests ÷ package energy above idle, the run's window | 0.890 | 0.728 to 0.983 |
| The same, over the seconds some guest was alive | 0.895 | 0.743 to 0.984 |
| Guests' cgroup CPU ÷ the host's busy CPU | 0.932 | 0.847 to 0.991 |

- **The energy billet did not charge to a job follows the host CPU no job's cgroup used,** which is how billet shares it: by each job's share of the host's busy CPU, leaving the host's own work unattributed ([measurement validation](../../operating/measurement-validation.md)). Across session 2's 48 runs, the attribution ratio and the guests' share of the host's busy CPU correlate at r = 0.95. The compile left more outside (CPU share 0.91, ratio 0.85) than the memory copy (0.97 and 0.96), and boost off more than boost on (0.88 and 0.80 against 0.92 and 0.88).
- **Two other candidates are ruled out as the main cause.** Counting only the seconds some guest was alive moved the ratio by half a point. And session 2's idle baseline was its own measured 78.9 W, where session 1 used the configured 69.7 W, and the mean came out the same (0.890 in both).
- **What used that CPU was not measured per process.** On this host the storage cluster serving the guests' disks runs outside every guest's cgroup, and the compile writes far more to disk than the memory copy, so it is the likeliest, but not shown.
- **Against the kit's per-run tolerance of 0.85 to 1.02,** 34 of session 2's 48 runs were inside (25 of session 1's 40).

## What this does not establish

- **Real CI jobs were not run:** two synthetic workloads at a few loads, one host, one day.
- **Few repeats:** three per configuration, two or three for boost. The 95% intervals are wide where the standard deviation is high.
- **Why this host idled 9 W, and in session 3 16 W, above the calibration run** that night. Each session's attribution used its own measured idle.
- **Which host process used the CPU** the attribution did not charge to a job.
- **Not measured:** amd-pstate, BIOS power settings, C-state limits, and how fast a host leaves and enters idle under each setting.
