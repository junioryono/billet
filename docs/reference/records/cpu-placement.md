# CPU placement and idle power on the reference node

What placing microVMs on host cpus, and capping the cpu while a node is idle, did to job run time and to power. It was measured on 2026-10-23 in three sessions on the reference deployment's Firecracker node, each with the deployment drained so nothing else ran there: 05:54 to 07:04 UTC, 14:44 to 16:07 UTC, and 16:14 to 16:46 UTC.

## The host and how it was measured

- **Host:** `ubuntu-01`.
  - **Hardware:** AMD EPYC 7763, 64 cores and 128 threads in 8 CCDs. Each CCD is 8 cores sharing 32 MB of L3, and cpu N's SMT sibling is cpu N+64.
  - **Software:** Linux 7.0.0-38, `acpi-cpufreq` with `schedutil`, P-states 2.45, 2.0 and 1.5 GHz, boost to 3.53 GHz. Firecracker v1.16.1.
  - **Guest:** the golden generation `ubuntu-2404-x64@g20261023004315`.
  - **Wall meter:** a Shelly Plug US Gen4 on the host's one power cord.
- **Harness:** `TestRealPlacementExperiment` in `internal/provider/firecracker`. It runs real guests through billet's own provider under a separate deployment identity, measured by the node's own sampler. A guest has 1 GiB per vCPU, and 8 vCPUs unless a section says otherwise.
- **Workloads:** both need no network.
  - `gostd` is `go build -a std` twice, each from an empty cache: a parallel compile.
  - `memory` is every vCPU copying a 256 MiB buffer 600 times: far larger than any L3.
  - A guest reboots when its workload succeeds, which ends its VMM, so run time is from launch returned to VMM exit.
- **Power:** `scripts/power-log.sh --meter` logged package energy (RAPL), turbostat and the wall meter through each session: 3,633, 4,288 and 1,680 intervals, a row about every 1.15 s. A row's package power is its energy over its own interval, and wall energy is the meter's watts times that interval; the meter's own energy counter over the same intervals agrees with it per run within a median 1.3% and at most 5.1%. turbostat's energy agreed with RAPL's to 0.999, 0.999 and 1.000.
- **Placements:**
  - `none` is today's unpinned guests.
  - `compact` gives a guest both threads of as few cores as it needs, in the lowest last-level-cache group with room for it.
  - `spread` gives it one thread per core in the emptiest group.
  - `packed` gives it one thread per core, from the lowest group with a free core onward.
- **Runs:**
  - Session 1: `none`, `compact` and `spread` at 4 and 8 guests, both workloads, three repeats in a shuffled order, then two boost runs each way under `compact`. 40 runs and 232 guests.
  - Session 2: `compact`, `packed` and `spread` the same way, then three boost runs each way under `spread` at 4 and 8 guests. 48 runs and 288 guests.
  - Session 3: `compact`, `packed` and `spread` with 8 guests of 4 vCPUs, both workloads, three repeats, then `compact` and unpinned with 12 guests of 8 vCPUs on the compile, three repeats. 24 runs and 216 guests.
  - No guest failed.
- **Statistics:** means of the repeats, with the standard deviation (sd). A difference is called real here only where Welch's two-sided t-test gives p < 0.05; with three runs a side, a difference inside the spread of one placement's runs is not shown either way.

## Results

### No cpu ceiling showed an idle saving

Session 2 read each setting three times for 60 s, interleaved and shuffled, 10 s trimmed from each end, after five quiet minutes that read 70.0 W in the package and 149.9 W at the wall.

| Setting | Package, rounds 1, 2, 3 | Wall, rounds 1, 2, 3 |
|---|---|---|
| Default (boost on, 2.45 GHz ceiling) | 77.1, 70.1, 69.9 W | 165.9, 150.3, 149.6 W |
| Boost off | 76.2, 69.8, 69.8 W | 163.7, 148.8, 149.1 W |
| Boost off, 2.0 GHz ceiling | 74.1, 71.8, 69.6 W | 158.9, 154.0, 148.8 W |
| Boost off, 1.5 GHz ceiling | 72.1, 69.7, 69.5 W | 154.3, 148.7, 148.4 W |

- **No setting showed a saving.** In the last round the four package readings were within 0.4 W of each other (69.5 to 69.9 W) and of the quiet minutes, and the wall readings within 1.2 W. Across all three rounds the differences are not significant (the default against the 1.5 GHz ceiling, 1.9 W apart on average, p 0.51), so a saving of a watt or two is not ruled out either; one this small would be under 3% of the host's idle package power. Whatever capping a node's frequency while it is idle, which `node.power` does, saves beyond that would have to show in how a host leaves and enters idle, which this did not measure.
- **Every setting read 2 to 7 W higher in the first round,** though the quiet minutes just before it read 70.0 W. What the host was doing then was not measured.
- **That fits what is known of this chip:** an idle core sits in a C-state whatever its ceiling, and most of the idle draw is outside the cores (about 69 of 73.5 W was uncore in the package's own split in September).
- **The host idled at 70.1 and 70.0 W in sessions 1 and 2,** as the calibration run at 02:50 had read (69.9 W). Session 3, which began seven minutes after session 2's last guest, read 76.3 W package and 161.3 W wall in its quiet minutes; why was not measured.

### node.power's mechanism on the real host

`TestRealCapAndRestoreOnThisHost` (`internal/power`) ran the controller against the host's own sysfs in session 2. It capped the idle node, read what changed, restored through `RestoreBeforeLaunch` as a launch does, and checked every value was back.

| Settings | Cycles | Restore before a launch | Values wrong afterwards |
|---|---|---|---|
| Boost off | 5 | 19.2 to 22.3 ms | none |
| Boost off and a 1.5 GHz ceiling | 5 | 20.0 to 21.8 ms | none |

- **A launch waits about 20 ms for the full ceiling:** every policy's `scaling_max_freq` and boost switch and the global boost switch, all compared afterwards with what the host held before.
- **Turning boost off moves only the boost switch here:** `scaling_max_freq` read 2.45 GHz while boost was off, and 1.5 GHz only when the ceiling was set.

### Compact was slower than spread on every load spread could hold

Session 2. Run time is per guest. Energy is the whole host over the run's window.

| Workload | Guests | Placement | Run time | sd | Package | Wall | IPC |
|---|---|---|---|---|---|---|---|
| gostd | 4 | compact | 45.2 s | 0.1 | 6.15 kJ | 11.23 kJ | 0.93 |
| gostd | 4 | packed | 31.8 s | 0.2 | 4.94 kJ | 8.61 kJ | 1.33 |
| gostd | 4 | spread | 32.0 s | 1.9 | 5.01 kJ | 8.84 kJ | 1.33 |
| gostd | 8 | compact | 55.2 s | 5.8 | 9.05 kJ | 15.84 kJ | 0.93 |
| gostd | 8 | packed | 40.7 s | 1.5 | 7.97 kJ | 13.68 kJ | 1.27 |
| gostd | 8 | spread | 40.2 s | 1.8 | 7.95 kJ | 13.52 kJ | 1.27 |
| memory | 4 | compact | 103.3 s | 0.1 | 14.43 kJ | 27.93 kJ | 0.08 |
| memory | 4 | packed | 81.0 s | 0.3 | 12.87 kJ | 24.08 kJ | 0.10 |
| memory | 4 | spread | 81.4 s | 0.2 | 12.57 kJ | 23.59 kJ | 0.10 |
| memory | 8 | compact | 153.4 s | 0.3 | 24.43 kJ | 46.69 kJ | 0.05 |
| memory | 8 | packed | 147.6 s | 0.8 | 28.30 kJ | 50.71 kJ | 0.06 |
| memory | 8 | spread | 147.4 s | 0.0 | 28.18 kJ | 50.32 kJ | 0.06 |

Session 1, with unpinned guests in place of `packed`:

| Workload | Guests | Placement | Run time | sd | Package | Wall | IPC |
|---|---|---|---|---|---|---|---|
| gostd | 4 | none | 36.9 s | 6.6 | 5.44 kJ | 9.69 kJ | 1.26 |
| gostd | 4 | compact | 46.5 s | 1.3 | 6.17 kJ | 11.28 kJ | 0.93 |
| gostd | 4 | spread | 32.5 s | 0.9 | 4.91 kJ | 8.61 kJ | 1.33 |
| gostd | 8 | none | 44.7 s | 3.7 | 8.66 kJ | 15.03 kJ | 1.16 |
| gostd | 8 | compact | 54.0 s | 5.1 | 9.01 kJ | 15.68 kJ | 0.93 |
| gostd | 8 | spread | 41.5 s | 0.5 | 7.93 kJ | 13.43 kJ | 1.28 |
| memory | 4 | none | 79.4 s | 0.8 | 12.34 kJ | 23.11 kJ | 0.12 |
| memory | 4 | compact | 103.5 s | 0.1 | 14.14 kJ | 27.48 kJ | 0.08 |
| memory | 4 | spread | 82.2 s | 0.5 | 12.70 kJ | 23.74 kJ | 0.10 |
| memory | 8 | none | 148.7 s | 1.0 | 27.00 kJ | 49.08 kJ | 0.06 |
| memory | 8 | compact | 154.5 s | 0.3 | 24.98 kJ | 47.57 kJ | 0.05 |
| memory | 8 | spread | 148.5 s | 0.2 | 28.36 kJ | 50.72 kJ | 0.06 |

- **The two sessions agree:** every `compact` and `spread` configuration they share ran within 3.5% of the other session's time.
- **`compact` ran longer than `spread` in every configuration of both sessions** (p ≤ 0.05 in all but one): at 4 guests, 41 to 43% longer on the compile, with IPC 0.93 against 1.33, and 26 to 27% longer on the memory copy. It gives a guest's 8 vCPUs only 4 physical cores, both SMT siblings of each.
- **On the memory copy at 8 guests, `compact` used less energy than `spread`:** 12 to 13% less package energy and 6 to 7% less wall energy for 4% more time, in both sessions (p ≤ 0.012). On this memory-bound load at half the host's threads, packing guests onto fewer cores saved energy, at a small cost in time. On the compile at 4 guests it cost both time and energy.
- **`spread` against unpinned guests (session 1):** on the compile, `spread`'s mean was 12% shorter at 4 guests and 7% at 8, but unpinned runs varied so much (sd 3.7 to 6.6 s against 0.5 to 0.9 s) that neither difference is significant over three runs (p 0.36 and 0.28). What is shown is that `spread`'s run times were far steadier. On the memory copy at 4 guests, unpinned guests ran 3.5% faster than `spread` (p 0.009).
- **With 8-vCPU guests, `packed` and `spread` are the same placement on this host:** each gives every guest the 8 cores of one whole CCD (the recorded cpu sets show it), and they ran the same. So this matrix separates nothing between them; session 3 does, below.
- **The memory workload stalls on memory** (IPC 0.05 to 0.12, about 35 last-level misses per 1,000 instructions); that it is bound by memory bandwidth is an interpretation of those counters, since the harness measures no bandwidth.

### Sharing a CCD's L3 against sharing cores

Session 3 used 8 guests of 4 vCPUs, so each placement gives its guests a different layout, as the recorded cpu sets show:

- `spread` gives each guest 4 cores of a CCD of its own: 8 CCDs, no shared L3, no shared cores.
- `packed` puts two guests in each of 4 CCDs: a shared L3, no shared cores.
- `compact` puts four guests in each of 2 CCDs, on both threads of 2 cores each: an L3 shared four ways, and shared cores.

| Workload | Placement | Run time | sd | Package | Wall | IPC |
|---|---|---|---|---|---|---|
| gostd | compact | 57.7 s | 1.0 | 8.94 kJ | 15.59 kJ | 0.91 |
| gostd | packed | 50.5 s | 1.1 | 8.60 kJ | 14.73 kJ | 1.33 |
| gostd | spread | 49.6 s | 0.9 | 8.51 kJ | 14.71 kJ | 1.38 |
| memory | compact | 103.5 s | 0.3 | 14.36 kJ | 27.82 kJ | 0.09 |
| memory | packed | 80.0 s | 0.0 | 12.67 kJ | 23.67 kJ | 0.11 |
| memory | spread | 76.5 s | 0.2 | 12.70 kJ | 23.55 kJ | 0.12 |

- **Sharing a CCD's L3 between two guests made no measurable difference on the compile** (`packed` 1.9% longer than `spread`, p 0.32), and cost 4.6% on the memory copy (p 0.001).
- **`compact`, which adds shared cores and a more crowded L3, ran 14% and 29% longer than `packed`** on the compile and the memory copy (p ≤ 0.001), and 16% and 35% longer than `spread`. Sharing cores and crowding four guests into one L3 happen together in `compact`, so this does not say how much each one costs.
- **Leaving whole CCDs idle showed no energy saving:** `packed` left four CCDs with nothing on them, and its package and wall energy were within 1.1% of `spread`'s, with no difference detected (p ≥ 0.24 on both workloads). That is consistent with the idle result above: on this host an idle core costs little whether or not its CCD has work.

### At 75% of the threads, compact and unpinned showed no difference

Session 3 then ran 12 guests of 8 vCPUs, 96 of 128 threads, on the compile. `spread` and `packed` can give whole cores to only 8 such guests, and a guest the placement cannot fit runs unpinned, so they were not run; `compact` fits all 12.

| Placement | Run time | sd | Package | Wall | IPC |
|---|---|---|---|---|---|
| none | 56.5 s | 6.2 | 11.18 kJ | 19.01 kJ | 1.00 |
| compact | 55.4 s | 1.6 | 11.03 kJ | 18.77 kJ | 0.91 |

- **At this load their means were within 2% in time and energy, and no difference was detected** (p ≥ 0.73), though with unpinned runs this variable the test could miss one of about a quarter either way. 96 vCPUs need more threads than the host has cores, so unpinned guests share SMT siblings too.
- **`compact`'s run times varied a quarter as much,** a standard deviation of 1.6 s against 6.2 s.

### Boost off saved energy under compact and cost it under spread

| gostd | Boost | Runs | Run time | Package | Wall |
|---|---|---|---|---|---|
| 4 guests, `compact` (session 1) | on | 2 | 44.7 s | 6.12 kJ | 11.24 kJ |
| 4 guests, `compact` (session 1) | off | 2 | 44.1 s | 5.18 kJ | 9.81 kJ |
| 4 guests, `spread` (session 2) | on | 3 | 31.8 s | 4.94 kJ | 8.70 kJ |
| 4 guests, `spread` (session 2) | off | 3 | 47.8 s | 5.99 kJ | 11.25 kJ |
| 8 guests, `spread` (session 2) | on | 3 | 40.9 s | 7.84 kJ | 13.33 kJ |
| 8 guests, `spread` (session 2) | off | 3 | 67.1 s | 9.30 kJ | 17.02 kJ |

- **Under `spread`, boost off made the compile 50% slower at 4 guests and 64% slower at 8, and it used 21% and 19% more package energy and 29% and 28% more at the wall** (p ≤ 0.013). The longer run cost more than the lower clock saved.
- **Under `compact`, boost off saved 15% of package energy** (p 0.044, two runs each), and its run time was within 1.5% of boost on, with no difference detected (two runs cannot rule out one of a quarter either way). A guest whose cores share SMT siblings may gain little from boost to begin with.
- **So turning boost off is not a saving on this host for a job that has whole cores to itself,** which is what `spread` gives it. `node.power` turns boost off only while a node is idle, where the section above found it saves next to nothing, and restores it before a launch.

### Energy attribution

The harness charges each guest a share of the package energy above an idle baseline it is given, as the node's sampler does: in each interval, the energy above the baseline is shared by each job's share of the host's busy CPU, and the host's own work is left unattributed by design ([measurement validation](../../operating/measurement-validation.md)).

- **Session 1 is the check:** its baseline was the configured 69.7 W, against 70.1 W measured. Over its 40 runs, the energy charged to guests was a mean 0.891 of the package's energy above the baseline (0.772 to 0.976), and 26 runs were inside the kit's per-run tolerance of 0.85 to 1.02.
- **Sessions 2 and 3 were given baselines about 9 W too high,** 78.9 and 85.5 W against 70.0 and 76.3 W measured, by the analysis error described below. Their ratios (0.891 and 0.911) are against those baselines, so they are not what billet would attribute at the right one.
- **What sessions 2 and 3 do show is that the shortfall tracks the host's own CPU.** Their ratio follows the guests' share of the host's busy CPU run by run (r = 0.955 and 0.916), with the guests' cgroups using 0.932 and 0.937 of it, and the compile left more outside them than the memory copy. That follows from how the energy is shared, so it confirms the mechanism rather than testing it independently. It is not the whole of the shortfall: the harness starts sampling a guest only once its launch returns, the sampler leaves a job's last interval unattributed, and it charges nothing for an interval it cannot read consistently, and these results do not record how much each of those missed.
- **What used that CPU was not measured per process.** On this host the storage cluster serving the guests' disks runs outside every guest's cgroup, and the compile writes far more to disk than the memory copy, so it is the likeliest, but not shown.

### A correction in this record

An earlier revision of this record read each log row's energy as one second's power and summed the wall's watts as joules. The rows are about 1.15 s apart, so its idle readings were about 9 W too high (it reported a 79 W idle and an unexplained 9 W above the calibration run), and its wall energies were too low by as much as a quarter, which reversed its reading of the memory copy at 8 guests. The session scripts computed the harness's idle baseline the same way, which is why sessions 2 and 3 ran with the baselines above. Every figure here is recomputed with each row's own interval.

## What this does not establish

- **Real CI jobs were not run:** two synthetic workloads at a few loads, one host, one day.
- **Few repeats:** three per configuration, two or three for boost, so differences of a few percent are often not significant.
- **Why the first idle round, and session 3, read higher** than the quiet minutes that came before them.
- **Which host process used the CPU** the attribution did not charge to a job, how much of the shortfall the harness's own windows account for, and billet's attribution at the right baseline in sessions 2 and 3.
- **Not measured:** amd-pstate, BIOS power settings, C-state limits, and how fast a host leaves and enters idle under each setting.
