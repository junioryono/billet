# CPU placement and idle power on the reference node

What placing microVMs on host cpus, and capping the cpu while a node is idle, did to job run time and to power, measured on 2026-10-23 between 05:54 and 07:04 UTC on the reference deployment's Firecracker node, with the deployment drained so nothing else ran there.

## The host and how it was measured

- **Host:** `ubuntu-01`.
  - **Hardware:** AMD EPYC 7763, 64 cores and 128 threads in 8 CCDs. Each CCD is 8 cores sharing 32 MB of L3, and cpu N's SMT sibling is cpu N+64.
  - **Software:** Linux 7.0.0-38, `acpi-cpufreq` with `schedutil`, P-states 2.45, 2.0 and 1.5 GHz, boost to 3.53 GHz. Firecracker v1.16.1.
  - **Guest:** the golden generation `ubuntu-2404-x64@g20261023004315`.
  - **Wall meter:** a Shelly Plug US Gen4 on the host's one power cord.
- **Harness:** `TestRealPlacementExperiment` in `internal/provider/firecracker`, from #650. It runs real guests through billet's own provider under a separate deployment identity, measured by the node's own sampler. Every guest has 8 vCPUs and 8 GiB.
- **Workloads:** both need no network.
  - `gostd` is `go build -a std` twice, each from an empty cache: a parallel compile.
  - `memory` is every vCPU copying a 256 MiB buffer 600 times: far larger than any L3.
  - A guest reboots when its workload succeeds, which ends its VMM, so run time is from launch returned to VMM exit.
- **Power:** `scripts/power-log.sh --meter` logged package energy (RAPL) and the wall meter once a second through the whole session, 3,633 rows. turbostat agreed with RAPL at 0.999.
- **The matrix:**
  - Placements: `none` (today's unpinned guests), `compact` and `spread`.
  - Loads: 4 and 8 guests, that is 32 and 64 vCPUs on 128 threads.
  - Both workloads, three repeats, in a shuffled order within each repeat. 36 runs and 216 guests, and 40 runs and 232 guests with the boost runs below; none failed.
- **Idle settings:** each held for two minutes, 15 s trimmed from each end, then two runs with boost off beside two with it on.

## Results

### No cpu ceiling lowered idle power

| Setting | Package (RAPL) | Wall |
|---|---|---|
| Default (boost on, 2.45 GHz ceiling) | 78.8 W | 150.5 W |
| Boost off | 80.5 W | 152.1 W |
| Boost off, 2.0 GHz ceiling | 82.7 W | 157.1 W |
| Boost off, 1.5 GHz ceiling | 81.8 W | 154.4 W |

- **No setting lowered idle power.** Each capped setting read 1.7 to 3.9 W higher in the package and 1.6 to 6.6 W higher at the wall than the default. With one two-minute reading per setting and no measure of its spread, that difference is not shown to be real; what is shown is that none was lower. That fits what is known of this chip: an idle core sits in a C-state whatever its ceiling, and most of the idle draw is outside the cores (about 69 of 73.5 W was uncore in the package's own split in September).
- **So capping a node's frequency while it is idle, which `node.power` (#651) does, showed no saving on this host.** Whatever it saves would have to show in how a host leaves and enters idle, which this did not measure.
- **The idle package read about 79 W in this session**, against 69.9 W in the calibration run at 02:50 the same night. The difference is not explained here; background work after a busy night is the likeliest cause. Every setting above was read in the same half hour, so they compare with each other.

### Spread was fastest and used the least energy on the compile

Means of the three repeats. Run time is per guest. Energy is the whole host over the run's window.

| Workload | Guests | Placement | Run time | sd | Package | Wall | IPC | Misses per 1,000 instructions |
|---|---|---|---|---|---|---|---|---|
| gostd | 4 | none | 36.9 s | 6.6 | 5.44 kJ | 8.60 kJ | 1.26 | 12.75 |
| gostd | 4 | compact | 46.5 s | 1.3 | 6.17 kJ | 10.02 kJ | 0.93 | 13.28 |
| gostd | 4 | spread | 32.5 s | 0.9 | 4.91 kJ | 7.66 kJ | 1.33 | 12.54 |
| gostd | 8 | none | 44.7 s | 3.7 | 8.66 kJ | 13.35 kJ | 1.16 | 13.06 |
| gostd | 8 | compact | 54.0 s | 5.1 | 9.01 kJ | 13.92 kJ | 0.93 | 13.31 |
| gostd | 8 | spread | 41.5 s | 0.5 | 7.93 kJ | 11.94 kJ | 1.28 | 12.69 |
| memory | 4 | none | 79.4 s | 0.8 | 12.34 kJ | 20.47 kJ | 0.12 | 33.32 |
| memory | 4 | compact | 103.5 s | 0.1 | 14.14 kJ | 24.38 kJ | 0.08 | 31.28 |
| memory | 4 | spread | 82.2 s | 0.5 | 12.70 kJ | 21.05 kJ | 0.10 | 33.68 |
| memory | 8 | none | 148.7 s | 1.0 | 27.00 kJ | 43.12 kJ | 0.06 | 37.70 |
| memory | 8 | compact | 154.5 s | 0.3 | 24.98 kJ | 42.20 kJ | 0.05 | 32.33 |
| memory | 8 | spread | 148.5 s | 0.2 | 28.36 kJ | 38.10 kJ | 0.06 | 39.57 |

- **`spread` compared with unpinned guests on the compile:**
  - At 4 guests: 12% shorter run time and 10% less package energy.
  - At 8 guests: 7% shorter run time and 8% less package energy.
  - Its run times also varied far less, a standard deviation of 0.5 to 0.9 s against 3.7 to 6.6 s unpinned.
- **`compact` was the slowest in every configuration.** It gives a guest's 8 vCPUs only 4 physical cores, both SMT siblings each, so the compile's IPC fell from 1.33 to 0.93 and it ran longer, and on the compile it used more package energy than unpinned at both loads. Here packing work onto fewer cores lost to finishing sooner. Why the freed cores saved too little to make up for it was not measured; the idle result above is consistent with idle cores costing little.
- **The memory workload stalls on memory** (IPC 0.05 to 0.12, about 35 last-level misses per 1,000 instructions). That it is bound by memory bandwidth is an interpretation of those counters; the harness measures no bandwidth.
  - At 4 guests, `spread` ran 3.5% longer than unpinned (82.2 s against 79.4 s) and `compact` 30% longer. At 8 guests unpinned and `spread` ran the same.
  - At 8 guests, `spread` used the least wall energy (38.1 kJ against 43.1 unpinned) but not the least package energy (28.4 against 27.0 kJ). So the energy outside the package differed between placements; these instruments cannot say which part of the host it was.
- **Not separated:** `compact` as built changes two things at once, sharing SMT siblings and using fewer CCDs. A placement that used fewer CCDs without sharing siblings was not run.
### Boost off cost the compile nothing and saved energy, in two runs

| gostd, 4 guests, compact | Run time | Package | Wall |
|---|---|---|---|
| Boost on (2 runs) | 44.7 s | 6.12 kJ | 9.97 kJ |
| Boost off (2 runs) | 44.1 s | 5.18 kJ | 8.70 kJ |

- **Turning boost off under load saved 15% of package energy and 13% at the wall, with no change in run time,** for a compile under `compact`.
- **Two runs each is a lead, not a result.** Most of the host's cores were idle, and a job whose cores share SMT siblings may not reach boost clocks anyway. It wants repeating under `spread` and at more load before boost is turned off for a load.

### Energy attribution

- **Over the 40 runs, the energy billet's sampler charged to the guests was a mean 0.890 of the package's energy above the configured idle baseline** (69.7 W), ranging from 0.763 to 0.980, all measured in the same window.
- **This is the first check of attribution on a quiet host with jobs in it.** Earlier runs had other CI at both ends of the log (UNMEASURED) or a wrong baseline.
- **Against the kit's per-run tolerance of 0.85 to 1.02, 25 of the 40 runs were inside and 15 below it;** the mean is inside.
- **The unattributed tenth has more than one candidate, and this did not separate them:**
  - the host's own work while guests ran (the storage cluster serving their disks is outside every guest's cgroup);
  - the idle baseline: this session idled about 9 W above the configured 69.7 W;
  - the harness's windows: a run's window opens before its launches, and each guest joins the sampler only once its launch returns.

## What this does not establish

- **Real CI jobs were not run:** two synthetic workloads at two loads, one host, one night.
- **Few repeats:** three per configuration and two for boost. The 95% intervals are wide where the standard deviation is high.
- **One idle reading per setting**, in a session whose idle was 9 W above the night's earlier calibration.
- **SMT and CCD count not separated:** no placement shares fewer CCDs without sharing SMT siblings.
- **Not measured:** amd-pstate, BIOS power settings, C-state limits, and `node.power`'s own mechanism on this host.
