# Measurement validation

With `node.monitoring` on, a node measures every job it runs from the host: the job's cgroup (CPU, peak memory, disk bytes, pressure), its tap (network bytes), the VMM's threads (guest vCPUs against the VMM's own work) and the package's RAPL energy, shared among jobs by CPU time above the idle baseline. `billet jobs show <lease>` prints what was measured. This page is how an operator proves those numbers on real hardware: against jobs whose answer is known before they run, and against the server's BMC. Each check is one command, and each answers PASS, FAIL or UNMEASURED; a figure billet did not measure is UNMEASURED, never PASS.

The commands are written for the reference host (`ubuntu-01`, an AMD EPYC 7763 running Firecracker, with the control plane on another host); [the reference hardware](../reference/reference-hardware.md) records the facts they rest on. Nothing in the kit is specific to that host except the numbers in the examples.

## What the kit is

| Piece | What it does |
|---|---|
| `.github/workflows/known-answer.yml` | A reusable workflow a repository calls with one of its billet tier labels. Six jobs run one after another on that tier: `baseline` (prepare and stop), `idle` (prepare, then sleep T), `cpu` (`stress-ng --cpu N --cpu-method matrixprod --timeout T`), `memory` (`stress-ng --vm 1 --vm-bytes X --vm-keep --timeout T`), `network` (download a body of known size to `/dev/null`) and `disk` (`fio`, `direct=1`, X bytes written). Each uploads an artifact `known-answer-<run>-<attempt>-<kind>` holding its `expectation.json`. It lives under `.github/workflows` because GitHub resolves a reusable workflow only from there; `actions/` holds composite actions, which are single steps and cannot fan out into jobs. |
| `scripts/known-answer-job.sh` | What each job runs, fetched from the billet revision the caller names. It reads the lease from the runner's name (billet names every runner `billet-<lease>`), does the same preparation for every kind (`apt-get update`, install `stress-ng fio curl`), runs the load, and prints and records the expected figure with the lease, the run id, the attempt and GitHub's job id (`job.check_run_id`). |
| `scripts/knownanswer` | The checker, a Go program: `collect` asks `billet jobs show --json` for every lease the expectations and the power log name; `check` compares each job with its expectation; `energy` reconciles the jobs' attributed energy with the package's RAPL over a window. |
| `scripts/power-log.sh` | Run on the node as root: once a second it logs the package zone's `energy_uj` with the wrap at `max_energy_range_uj` undone, `ipmitool dcmi power reading` through `/dev/ipmi0`, turbostat's PkgWatt when turbostat is installed, and which billet microVMs hold a process (`?` when the cgroup tree could not be read, which is not an empty inventory), to a CSV. The clock, the counter and the inventory are read together before the BMC is asked, since a BMC call can take seconds. With `--meter CMD` it also logs a wall meter's watts and cumulative watt-hours (below). When it stops it prints `power-summary.sh`'s comparison. |
| `scripts/power-summary.sh` | RAPL against the BMC, the wall meter and turbostat over a log, or a window of one: means, ratio, offset and a least-squares fit, and the meter's own energy over the window. |

## How a job is matched to billet's record

The lease is the key. The job reads it from `RUNNER_NAME`, which billet sets to `billet-<lease>` when it registers the runner, so nothing the job says about itself is trusted to make the match. The checker then requires billet's record of that lease to name the same workflow run id the job saw; a record naming another run is a FAIL, because the lease ran something other than the job. GitHub's job id is printed on both sides and compared as a note, never a verdict: billet records the job id the scale-set message carries, and whether that is the same number as the workflow's `job.check_run_id` has not been measured. The first run of the kit answers it, in the `github job id` line under each result.

## Before you start

`billet jobs show --json` is new with this kit, so the control plane needs a billet that has it; v0.12.44 does not. The node needs nothing new.

On the node (`ubuntu-01`), install the tools and load the in-band IPMI driver:

```bash
sudo apt-get install -y stress-ng iperf3 ipmitool
sudo modprobe -a ipmi_si ipmi_devintf
ls -l /dev/ipmi0
sudo ipmitool dcmi power reading
```

`ipmitool` is the one the kit needs: the BMC is reachable only on the LAN, and in-band through `/dev/ipmi0` it needs neither. `stress-ng` on the host is for the BMC calibration below; the guests install their own. `iperf3` is not used by the kit: the network job downloads from a public URL because the guest network refuses private, link-local and CGNAT destinations, so a LAN iperf3 server is out of a guest's reach. The DCMI reading must say `Power reading state is: activated`; a deactivated reading is logged as empty, never as zero.

Confirm monitoring is on, as it was on 2026-10-09 (`interval: 1s`, `rapl: true`, `idle_package_watts: 73.5`):

```bash
sudo grep -A4 'monitoring:' /etc/billet/billet.yaml
```

Build the checker where Go is, for the control plane's platform, and copy it there with the two scripts the node needs:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o knownanswer ./scripts/knownanswer
scp knownanswer control-plane:
scp scripts/power-log.sh scripts/power-summary.sh ubuntu-01:
```

In the repository whose runners are under test, add a caller. The tier must admit it: a trusted tier admits only the workflows its runner group allows, so add this file's ref there.

```yaml
name: known-answer
on: workflow_dispatch
jobs:
  known-answer:
    uses: junioryono/billet/.github/workflows/known-answer.yml@<tag-or-commit>
    with:
      runner_label: billet-8vcpu
      billet_ref: <the same tag-or-commit>
      seconds: 60
      cpu_workers: 4
      memory_bytes: 2147483648
      network_url: https://speed.cloudflare.com/__down?bytes=104857600
      network_bytes: 104857600
      disk_bytes: 2147483648
```

`runner_label` must name a tier only the node under test serves: each loaded job is compared with its run's idle job, which must have run on the same host, and a pair from two hosts is UNMEASURED. `cpu_workers` must not exceed the tier's vCPUs (the job refuses, since more workers than vCPUs share them and cannot run N×T seconds), and `memory_bytes` must sit well inside the tier's memory. `network_url` is any public URL whose body is exactly `network_bytes`; with `network_bytes` given, a body of any other size fails the job rather than recording the wrong answer. The Cloudflare endpoint above is one such URL; it has not been measured from this host.

## The checks, in order

Every step names the host it runs on. `$REPO` is the repository with the caller.

1. **Node: start the power log** and leave it running through every run. Let it log two quiet minutes before the first job and two after the last: those minutes are the idle baseline the energy check measures before and after.

   ```bash
   sudo ./power-log.sh --out power.csv --require-bmc
   ```

2. **Anywhere with `gh`: run the workflow six times, one after another.** The first is the warmup and is discarded; the other five are measured. A run's jobs already run one at a time; waiting for each run keeps two runs from sharing the host. The loop stops at the first run that fails or whose artifacts do not download, so a failure is looked at rather than averaged away.

   ```bash
   for i in 1 2 3 4 5 6; do
     gh workflow run known-answer.yml -R "$REPO" || break
     sleep 10
     id=$(gh run list -R "$REPO" -w known-answer.yml -L 1 --json databaseId -q '.[0].databaseId') || break
     gh run watch "$id" -R "$REPO" --exit-status || { echo "run $id failed"; break; }
     gh run download "$id" -R "$REPO" -p 'known-answer-*' -D "expectations/$id" || { echo "run $id did not download"; break; }
   done
   ```

3. **Node: stop the power log** with Ctrl-C after the two quiet minutes. It prints the RAPL and BMC summary (the BMC check, below). Copy `power.csv` and `expectations/` to the control plane.

4. **Control plane: collect billet's records** for every lease the expectations and the log name.

   ```bash
   ./knownanswer collect --out records --expectations expectations --power-log power.csv --config /etc/billet/billet.yaml
   ```

   It exits 1 if any lease could not be collected and says which; a usage report arrives as a job's compute is destroyed, so a lease collected too early has none and is collected again by running the same command.

5. **Control plane: the known-answer check.**

   ```bash
   ./knownanswer check --expectations expectations --records records --discard 1 --runs 5
   ```

   `--runs` is required: a run whose baseline failed before uploading left no expectation at all, so only the count asked for can notice it is missing, and fewer measured runs than asked for is UNMEASURED.

6. **Control plane: the energy check.**

   ```bash
   ./knownanswer energy --power-log power.csv --records records --idle-watts 73.5
   ```

7. **Node: the BMC check**, if you want it again or over a window (epoch seconds, from the log's first column):

   ```bash
   ./power-summary.sh --require-bmc power.csv
   ./power-summary.sh --require-bmc --from 1791540000 --to 1791540600 power.csv
   ```

   To give the BMC fit a range wider than CI jobs reach, drain the node, run `sudo ./power-log.sh --out calibration.csv --require-bmc` and, beside it, `stress-ng --cpu 32 --timeout 120s`, then `--cpu 64`, then `--cpu 128`, with a quiet minute between each, and resume the node afterwards. On a host whose BMC reports no power reading, as the reference host's does, the [wall meter](#checking-rapl-at-the-wall) takes its place.

Exit statuses are the verdict, three ways, for every check: 0 PASS, 1 FAIL, 3 could not tell (UNMEASURED, or a BMC that never answered under `--require-bmc`), and 2 for a command that could not run (bad flags, unreadable inputs, a log that already exists).

## Tolerances, and why each is what it is

Every loaded job is compared net of the same run's idle job, and the idle job net of the baseline. The host measures the microVM's whole life (boot, runner, checkout, package install, upload), so only what differs between two jobs that did everything else identically is the load. The idle job sleeps for the same T as the CPU and memory loads, so the runner's own per-second cost cancels too. A load the tolerance cannot tell from no load at all, such as one worker for one second or a 1 MiB download, whose accepted range holds zero, is UNMEASURED rather than compared.

| Job | Compared | Accepted | Why |
|---|---|---|---|
| idle | CPU charged in the job's own load window (its sleep), from billet's per-second series; with a record collected without one, CPU (user + system) minus the baseline's | window: [0, 0.02 CPU × T + 0.01 CPU × N × T + 3 s]; without: [−3 s, 0.02 CPU × T + 3 s] | A sleeping guest costs its timer ticks and the runner's heartbeat and log streaming, which stay well under 2% of one CPU. Its idle vCPUs also cost the host something the guest never sees, mostly KVM halt polling: beyond what the guest agent reported busy, runs 11 and 12 (2026-10-14) charged 0.0082 and 0.0037 CPU per vCPU across 8-vCPU sleeps, so the window allows 0.01 per vCPU, N being the record's own count. ±3 s is the spread two microVMs' identical preparation can differ by, which only the whole-lease rule needs: run 12 failed it at 9.5 s while its sleep was charged 3.71 s. The expected answer is zero; a value growing with T is the finding. |
| cpu | CPU minus the idle job's | [0.95 N×T − 2 s, 1.05 N×T + 2 s] | stress-ng's timeout is exact to well under a second at T = 60 s; the VMM's own threads add a little (1.98 s of 62.35 s on a CI workload, 3%, and less for pure compute); ±2 s covers the preparation's spread. A host that descheduled vCPU threads charges less than N×T, correctly, so run on a host with free cores. |
| memory | the raw peak | [X, idle peak + 1.05 X + 64 MiB] | Compared raw because a peak is a maximum, not a sum: the stress allocation can reuse guest pages the preparation freed, which the host had already counted. The guest held X resident at once, so the peak cannot be below X; it can hardly exceed what the idle job reached plus X, plus 5% and 64 MiB for stress-ng's own pages and the VMM. |
| network | received in the job's own load window, from billet's per-second series (`jobs show --window`); with a record collected without one, received minus the median unloaded job's (of baseline, idle, cpu, memory and disk; the idle job's alone when fewer than three can be read on the host) | [S − 4 MiB, 1.06 S + 4 MiB] | The guest's view of its tap. Fewer bytes than the body cannot have delivered it; headers add up to about 4.6% (66 bytes on a 1,448-byte segment) when the tap carries MTU-sized frames and about 0.1% with 64 KiB offloaded ones, and TLS framing about 0.1%. ±4 MiB covers the run-to-run spread of the preparation's downloads and the runner's own traffic. |
| disk | written in the job's own load window, from billet's per-second series (`jobs show --window`); with a record collected without one, written minus the median unloaded job's (of baseline, idle, cpu and network; the idle job's alone when fewer than three can be read on the host) | [X − 64 MiB, 1.10 X + 64 MiB] | `fio --direct=1` writes X bytes past the guest's page cache; the guest filesystem's metadata and journal add a few percent. ±64 MiB covers the preparation's writes, which the guest flushes on its own schedule, so the idle and disk jobs can differ by what one guest had not yet written when it was destroyed. |
| energy | jobs' attributed active energy over the package's energy above `idle_package_watts` in the intervals a microVM was alive for (seen at the interval's start or end, which takes in its start and its tail), taken interval by interval and never below zero, as the node's monitor takes it; the monitor shares out nothing while no job runs, so quiet intervals are left out | [0.85 × (measured − clipped) ÷ measured, 1.02 + allowance ÷ measured] | billet shares the energy above `idle_package_watts` by each job's share of the host's busy CPU time, so the host's own work (billet, kernel networking, Ceph's OSDs serving the jobs' disks) stays unattributed by design; 15% is the most that should be on a host running only the kit. Attribution never hands out more than was measured, so above 1.02 is double counting, except for one thing the log cannot see: the monitor takes the baseline per tick, and its ticks fall between the log's rows, so it can count an excess in a second the log averaged with a dip. The two clocks also disagree the other way, so each bound gives way: the lower by what this log clipped at the baseline in the busy intervals (an interval it saw as a dip the monitor may have split into a smaller excess and a dip), the upper by the larger of that and the quiet ends' dips below the baseline per second times the busy seconds. Both are measured from the log, and they are a statistical allowance for noise, not a bound: power that changes within one row in a way the log cannot see can still move the monitor's figure past them. |
| idle baseline | each quiet end's measured package power | `idle_package_watts` ± 5% | turbostat and the package counter agreed within 0.5% on this host's idle (73.1 W and 73.5 W, 2026-09-25); 5% leaves room for temperature. |

Network and disk take their reference from the median of the run's unloaded jobs rather than from the idle job alone, because one job whose setup did less than the others otherwise decides the verdict: on 2026-10-10 the idle job of a run downloaded 57.3 MiB in setup where the run's four other unloaded jobs downloaded 97.1 to 97.2 MiB, and a network job measured at its known answer failed against it (#461). The result line names the job the median fell on; with an even count it is the lower of the middle two, so the reference is always one job's record rather than an average no job measured. Only jobs whose records name the same host count, and an unloaded job whose record is another run's fails network and disk as it fails every check that subtracts it.

Idle, disk and network read their own load windows instead, when the record carries one. `knownanswer collect` asks `billet jobs show --json --window` for the seconds between the job's `load_started_at` and one second past its `load_finished_at` (the job stamps both with `date`, to the second), and `check` compares what billet's series shows charged (idle), written (disk) or received (network) in them with the expected answer, subtracting nothing. Every job installs the same packages first, and that setup wrote 1,360 to 1,915 MiB a job across the runs of 2026-10-12 and 13, more than six times the check's tolerance: run 9's disk job failed at 1,287.5 MiB on the median rule while its own fio step wrote 2.4 GiB. Setup downloads vary the same way: run 11 (2026-10-14) failed network at 210.2 MiB against 161.4 on the median rule while the job's own window held 166.7 to 169.8 MiB, and passed disk at 2,048.2 MiB against 2,048 on its window. The verdict is taken on what the series knows for certain, not on its split of the edge intervals by time: billet also reports the whole sample intervals inside the window and the whole ones over it, PASS needs both inside the tolerance, FAIL both outside it on one side, and anything between is UNMEASURED with what the edges could hold. The idle, disk and network jobs flush their setup and wait two seconds before their first stamp, and wait two seconds after their second (disk and network are also quiet for two seconds at each end of their load inside their stamps; idle's load is itself quiet), so no edge interval the bounds reach holds its setup, its load's neighbours or what follows, and the two agree. A window that is not the job's, that the series does not cover whole, that carries no counter, or that billet could not integrate is UNMEASURED with the reason; one the series does not cover whole names each interval inside it the sampler did not see, with its span and cause, as `billet jobs show` lists them. Run 15's idle window (2026-10-15) was 1,998 ms short because writeback moved its cgroup's `io.stat` through every pass of one read and the sampler dropped that sample; it now records a lower bound there instead ([the configuration reference](../reference/configuration.md)). A control plane whose billet predates `--window` refuses the flag by name, and `collect` asks it again without one, so its records keep the median rule and the report says which rule it applied.

The energy check needs the whole window accounted for: every microVM the log saw must have a record with RAPL energy split by an idle baseline (`rapl`, not `rapl-unsplit`), the first and last ten rows must hold no microVM (a job crossing the window's edge has energy on both sides of it), every interval must have a RAPL reading on a clock that only moves forward, every row must have read which microVMs were running, and no interval a microVM was alive for may be longer than one and a half of the monitor's ticks, since the baseline cannot then be taken as the monitor took it (a slow BMC can stretch the log's rows that far; a second log run beside it with `--ipmitool none` keeps them at a second). Energy above the baseline that is no more than 1% of the window's once this log's clipping is taken off is too little to compare, since the lower bound would then hold an attribution of nothing. Any of these makes the check UNMEASURED, and it says which.

## What each result means

**PASS** means billet's figure is inside the stated range of the known answer in that run. Across runs, read the summary: the mean and its 95% confidence interval say where billet sits, and the CV how repeatable it is.

**UNMEASURED** means the comparison could not be made, and the line under it says why: billet did not measure the group (`billet check` on the node says which accounting the jailer was given), no usage report was recorded (the job ran on a node without monitoring, or the report has not arrived), no idle or baseline job in that run to subtract, a job that left no expectation (it failed before uploading, was skipped after an earlier failure, or its artifact was not downloaded), a record that does not carry the counter compared, a reference job that ran on another host, or no record was collected.

**FAIL**, by job:

- **cpu low**: the cgroup missed time the guest spent (the VMM threads in another cgroup), or the host descheduled vCPU threads; compare the `vmm split` line of `billet jobs show`. **cpu high**: the cgroup charged work that was not the guest's, or the idle reference was unusually cheap; look at that run's idle result.
- **idle high**: a sleeping guest is costing CPU, which grows with T; rerun with a longer `seconds` to see whether it scales.
- **memory below X**: the peak was not read from the microVM's cgroup, or memory accounting was not enabled for it. **memory high**: the peak includes something other than the guest and its VMM, such as host page cache charged to the cgroup.
- **network low**: the tap's direction is reported the wrong way round (the download would then appear as sent), or the counter missed part of the transfer. **network high**: other traffic in the job, or headers on a path without offloads; look at the idle job's figure.
- **disk low**: the host's page cache absorbed the guest's writes and they were written back after the microVM's cgroup was gone, so the cgroup never saw them; this is the likeliest disk finding on a drive the VMM opens without O_DIRECT. **disk high**: write amplification between the guest and the host's block device.
- **energy below 0.85**: more of the host's own work than expected sat outside the jobs, or intervals broke for some job; the `unattributed` line gives it in watts. **energy above 1.02**: energy was attributed twice. **idle baseline FAIL**: `idle_package_watts` is stale; measure it again in a quiet window and correct the node's configuration.
- **A record naming another run**: the lease ran something else, so the job's runner name and billet's record disagree about which job a lease served.

**The BMC comparison** has no PASS: the BMC measures the whole server at the wall (fans, memory, disks, NICs and the power supply's loss) and RAPL the CPU package alone. The offset is roughly the platform's draw beyond the package, the slope how the wall follows the package (above 1 for the supply's loss and the loads that track the CPU), and r² how closely. A BMC reading is often an average over its own sampling period and lags RAPL by a sample or two, so read the fit over steady stretches with `--from` and `--to`. turbostat reads the same package counter, so its PkgWatt should agree with RAPL within a percent or two over the whole log; more than that means one of the two is reading another zone. Row by row the two are approximate: turbostat samples on its own clock, and each row carries the reading it printed since the row before, read beside the RAPL counter.

## Checking RAPL at the wall

The energy check proves that billet shares out what RAPL reports, not that RAPL is right: RAPL is the package's own model of its energy, and on the reference host it is the only figure there is, since its BMC reports no power reading (measured 2026-10-09). A meter at the wall is the independent check. It measures the whole server (the package, memory, fans, disks, NICs and the power supply's loss), so it is not expected to equal RAPL; what is checked is that the wall follows the package along a straight line, and how steeply.

### What to buy

A plug-in or inline meter that the node can read on the LAN with no cloud service in the path, which reports both the instantaneous watts and a cumulative energy counter. Before buying, check four things against its datasheet rather than its listing:

- **Stated accuracy** across the range this host draws, from idle to all threads busy. A percentage of reading is what matters; a percentage of full scale on a meter rated far above the server is much worse at idle.
- **Update rate**: a new reading at least once a second. A meter that refreshes every few seconds answers the same number for several rows, which flattens the fit; check whether its API says when the reading was taken.
- **Counter resolution**: watt-hours or finer. A counter in hundredths of a kilowatt-hour moves once every 10 Wh, a large fraction of a twenty-minute run's energy.
- **Every power cord of the chassis.** Check whether the server has a redundant pair of power supplies (look at the back, or `sudo dmidecode -t 39`, which lists each supply the firmware knows). A redundant pair shares the load, so a meter on one cord sees about half of it: both cords must be metered, by two meters whose readings the meter command adds, or by one meter feeding a strip that carries both cords. The meter must also be rated for the current the supplies can draw.

### The meter command

`power-log.sh --meter CMD` runs CMD with `bash -c` once a sample, after reading RAPL and before the BMC, bounded by `--meter-timeout` (default 1 second, and a second more for a command that ignores SIGTERM), so a meter that hangs never holds the log. CMD prints one line: the watts, and the counter in watt-hours after a space, as plain decimals of at most six places, up to 999999 W and 999999999 Wh (`152.4 8812.031`, or `152.4` for a meter with no counter). An answer that exits non-zero, arrives after the bound, or is anything else (two lines, a unit, a comma, an exponent, a carriage return) leaves both `meter_watts` and `meter_wh` empty, never zero. No file the command or anything it starts writes can grow past 64 KiB, so a command that writes a large response to a file instead of a pipe fails rather than fills the disk. A meter whose API gives kilowatt-hours must be converted to watt-hours in the command. With two meters, the command prints the sum and must fail if either fails, since half the server is not a reading of it.

Write the command for the meter you have and try it by hand first; it should print one line, and its number should match the meter's own display. For a meter with an HTTP JSON API the shape is:

```bash
METER='set -o pipefail; curl -fsS --max-time 0.8 http://<meter-address>/<status-path> | jq -er "\"\(.<watts-field>) \(.<watt-hours-field>)\""'
bash -c "$METER"
```

`set -o pipefail` and `jq -e` make a failed request a failed command rather than an empty line, and `--max-time` keeps curl inside the bound.

### The run

It takes about twenty minutes, during which the node runs no jobs. A drain is deployment-wide, so every tier's jobs queue until the resume.

1. **Control plane: drain**, and wait until nothing runs.

   ```bash
   billet drain --reason "wall meter calibration" --wait --timeout 30m
   ```

2. **Node: start the log** with the BMC off, since it has no reading here and each call would stretch the rows past a second.

   ```bash
   sudo ./power-log.sh --out wall.csv --ipmitool none --meter "$METER"
   ```

3. **Node, a second shell: the step load**, two minutes at each of 0, 16, 32, 64 and 128 threads (the EPYC 7763 has 64 cores and 128 threads), recording when each step starts:

   ```bash
   for n in 0 16 32 64 128; do
     echo "$n $(date +%s)" >>steps.txt
     if [ "$n" -eq 0 ]; then sleep 120; else stress-ng --cpu "$n" --cpu-method matrixprod --timeout 120s; fi
   done
   echo "end $(date +%s)" >>steps.txt
   sleep 120
   ```

   The last two minutes are quiet again, to see the host return to the idle it started at.

4. **Node: stop the log** with Ctrl-C. It prints the summary of the whole log.

5. **Control plane: resume.**

   ```bash
   billet resume
   ```

6. **Node: each step on its own**, leaving out the first and last 15 seconds of each, where the meter and RAPL can disagree about when the step began:

   ```bash
   awk 'NR > 1 { print n, start, $2 } { n = $1; start = $2 }' steps.txt | while read -r n from to; do
     echo "== $n threads"; ./power-summary.sh --from $((from + 15)) --to $((to - 15)) wall.csv
   done
   ```

### What each result means

- **`fit: meter = a W + b x RAPL, r^2`** over the whole log is the result. `a` is the wall's draw with the package at zero, extrapolated: the platform beyond the package. `b` is how many watts the wall rises per watt the package rises; it should be above 1, for the supply's loss and the regulators, fans and memory that rise with the CPU. `r^2` says how closely the wall follows RAPL along that line across the steps.
- **The steps on their own**: each step's `meter: ... mean X W against RAPL Y W` is a point on that line. If the wall's rise over the 0-thread step, divided by RAPL's, is about `b` at every step, the wall and RAPL keep one relationship across the load, and billet's figures scale to the wall by `b` for a load like stress-ng's on this host as configured. If the ratio changes with the load, the relationship is not one line, and the run alone does not say why: RAPL's model may drift at that load, or the supply's efficiency, the fans or the memory may change with it while RAPL is right. Record which step departs and by how much, and treat a conversion by `b` as holding only for the loads and conditions it was measured under.
- **`meter fit: N rows used, M skipped for an empty meter cell, K for no RAPL interval`**: rows the meter did not answer are left out, never counted as zero. More than a few skipped means the meter answered late or malformed: try the command by hand, and raise `--meter-timeout` if it is slow but sound, since each row then stretches to the time it takes.
- **`meter energy`** is the meter's own energy over the window, from its counter when the counter has two readings and never goes backwards (a meter that restarted resets it), and otherwise each row's watts over the interval ending at it, summed over the intervals that have a reading; the line says which, and why the counter was not used. The counter is the meter's own integration at its own rate, so it is the better figure when it can be used; the integrated watts see only the instants the log asked.
- **Exit 3** means the meter never answered alongside a RAPL reading, or RAPL measured no interval: there is nothing to compare. The meter's own lines are still printed.

A known-answer run logged with `--meter` also gets the wall in the energy check: `knownanswer energy` reports the wall's idle at each quiet end, its energy above that idle over the intervals a microVM was alive for, and what share of it the package's RAPL and the jobs' attributed energy are. Those lines have no verdict and never change the check's: no tolerance between the package and the whole server has been measured yet. A busy interval the meter did not answer makes the wall's line could-not-tell rather than a smaller number, and a microVM whose record could not be used makes the jobs' share could-not-tell while the package's share is still given. With the step load's `b` in hand, a RAPL/wall share far from `1/b` while jobs ran says the jobs' load related the wall to the package differently from stress-ng's: power drawn outside the package (memory, disks, the network) is one explanation, and a supply or fans at another operating point are others.

## Methodology

- **Idle baseline before and after.** Two quiet minutes at each end of the power log. The energy check reports the measured package power over each quiet end against `idle_package_watts`; a drift between them means the host warmed or something else started.
- **One discarded warmup.** The first run meets cold mirrors, caches and images, so its preparation differs from every later run's; `--discard 1` drops the earliest run by id.
- **Five or more measured runs.** The summary gives, per metric, the mean, the 95% confidence interval for the mean (Student's t on n−1 degrees of freedom; past 30 the next row down in the table, so the interval only widens) and the coefficient of variation. A CV above about 5% for cpu, network or disk means the host was not quiet enough to compare, whatever the verdicts say.
- **One run at a time on an otherwise idle tier.** The energy check refuses a window whose ends hold a microVM, and needs a record for every microVM in it, so other work on the node makes it UNMEASURED rather than wrong.

## What is not covered

The kit validates the Firecracker provider on Linux. Tart's process accounting and the docker provider are not exercised, nor are the pressure-stall figures, the OOM count or the guest/VMM thread split, which have no known answer a job can produce. RAPL is itself a model of the package's energy, and the BMC comparison relates the two without saying which is right.
