# Measurement validation

With `node.monitoring` on, a node measures every job it runs from the host: the job's cgroup (CPU, peak memory, disk bytes, pressure), its tap (network bytes), the VMM's threads (guest vCPUs against the VMM's own work) and the package's RAPL energy, shared among jobs by CPU time above the idle baseline. `billet jobs show <lease>` prints what was measured. This page is how an operator proves those numbers on real hardware: against jobs whose answer is known before they run, and against the server's BMC. Each check is one command, and each answers PASS, FAIL or UNMEASURED; a figure billet did not measure is UNMEASURED, never PASS.

The commands are written for the reference host (`ubuntu-01`, an AMD EPYC 7763 running Firecracker, with the control plane on another host); [the reference hardware](../reference/reference-hardware.md) records the facts they rest on. Nothing in the kit is specific to that host except the numbers in the examples.

## What the kit is

| Piece | What it does |
|---|---|
| `.github/workflows/known-answer.yml` | A reusable workflow a repository calls with one of its billet tier labels. Six jobs run one after another on that tier: `baseline` (prepare and stop), `idle` (prepare, then sleep T), `cpu` (`stress-ng --cpu N --cpu-method matrixprod --timeout T`), `memory` (`stress-ng --vm 1 --vm-bytes X --vm-keep --timeout T`), `network` (download a body of known size to `/dev/null`) and `disk` (`fio`, `direct=1`, X bytes written). Each uploads an artifact `known-answer-<run>-<attempt>-<kind>` holding its `expectation.json`. It lives under `.github/workflows` because GitHub resolves a reusable workflow only from there; `actions/` holds composite actions, which are single steps and cannot fan out into jobs. |
| `scripts/known-answer-job.sh` | What each job runs, fetched from the billet revision the caller names. It reads the lease from the runner's name (billet names every runner `billet-<lease>`), does the same preparation for every kind (`apt-get update`, install `stress-ng fio curl`), runs the load, and prints and records the expected figure with the lease, the run id, the attempt and GitHub's job id (`job.check_run_id`). |
| `scripts/knownanswer` | The checker, a Go program: `collect` asks `billet jobs show --json` for every lease the expectations and the power log name; `check` compares each job with its expectation; `energy` reconciles the jobs' attributed energy with the package's RAPL over a window. |
| `scripts/power-log.sh` | Run on the node as root: once a second it logs the package zone's `energy_uj` with the wrap at `max_energy_range_uj` undone, `ipmitool dcmi power reading` through `/dev/ipmi0`, turbostat's PkgWatt when turbostat is installed, and which billet microVMs hold a process (`?` when the cgroup tree could not be read, which is not an empty inventory), to a CSV. The clock, the counter and the inventory are read together before the BMC is asked, since a BMC call can take seconds. When it stops it prints `power-summary.sh`'s comparison. |
| `scripts/power-summary.sh` | RAPL against the BMC and turbostat over a log, or a window of one: means, ratio, offset and a least-squares fit. |

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

`cpu_workers` must not exceed the tier's vCPUs (the job refuses, since more workers than vCPUs share them and cannot run N×T seconds), and `memory_bytes` must sit well inside the tier's memory. `network_url` is any public URL whose body is exactly `network_bytes`; with `network_bytes` given, a body of any other size fails the job rather than recording the wrong answer. The Cloudflare endpoint above is one such URL; it has not been measured from this host.

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

   To give the BMC fit a range wider than CI jobs reach, drain the node, run `sudo ./power-log.sh --out calibration.csv --require-bmc` and, beside it, `stress-ng --cpu 32 --timeout 120s`, then `--cpu 64`, then `--cpu 128`, with a quiet minute between each, and resume the node afterwards.

Exit statuses are the verdict, three ways, for every check: 0 PASS, 1 FAIL, 3 could not tell (UNMEASURED, or a BMC that never answered under `--require-bmc`), and 2 for a command that could not run (bad flags, unreadable inputs, a log that already exists).

## Tolerances, and why each is what it is

Every loaded job is compared net of the same run's idle job, and the idle job net of the baseline. The host measures the microVM's whole life (boot, runner, checkout, package install, upload), so only what differs between two jobs that did everything else identically is the load. The idle job sleeps for the same T as the CPU and memory loads, so the runner's own per-second cost cancels too. A load the tolerance cannot tell from no load at all, such as one worker for one second or a 1 MiB download, whose accepted range holds zero, is UNMEASURED rather than compared.

| Job | Compared | Accepted | Why |
|---|---|---|---|
| idle | CPU (user + system) minus the baseline's | [−3 s, 0.02 CPU × T + 3 s] | A sleeping guest costs its timer ticks and the runner's heartbeat and log streaming, which stay well under 2% of one CPU; ±3 s is the spread two microVMs' identical preparation (`apt-get update` and an install) can differ by. The expected answer is zero; a value growing with T is the finding. |
| cpu | CPU minus the idle job's | [0.95 N×T − 2 s, 1.05 N×T + 2 s] | stress-ng's timeout is exact to well under a second at T = 60 s; the VMM's own threads add a little (1.98 s of 62.35 s on a CI workload, 3%, and less for pure compute); ±2 s covers the preparation's spread. A host that descheduled vCPU threads charges less than N×T, correctly, so run on a host with free cores. |
| memory | the raw peak | [X, idle peak + 1.05 X + 64 MiB] | Compared raw because a peak is a maximum, not a sum: the stress allocation can reuse guest pages the preparation freed, which the host had already counted. The guest held X resident at once, so the peak cannot be below X; it can hardly exceed what the idle job reached plus X, plus 5% and 64 MiB for stress-ng's own pages and the VMM. |
| network | received minus the idle job's | [S − 4 MiB, 1.06 S + 4 MiB] | The guest's view of its tap. Fewer bytes than the body cannot have delivered it; headers add up to about 4.6% (66 bytes on a 1,448-byte segment) when the tap carries MTU-sized frames and about 0.1% with 64 KiB offloaded ones, and TLS framing about 0.1%. ±4 MiB covers the run-to-run spread of the preparation's downloads and the runner's own traffic. |
| disk | written minus the idle job's | [X − 64 MiB, 1.10 X + 64 MiB] | `fio --direct=1` writes X bytes past the guest's page cache; the guest filesystem's metadata and journal add a few percent. ±64 MiB covers the preparation's writes, which the guest flushes on its own schedule, so the idle and disk jobs can differ by what one guest had not yet written when it was destroyed. |
| energy | jobs' attributed active energy over the package's energy above `idle_package_watts` in the intervals a microVM was alive for (seen at the interval's start or end, which takes in its start and its tail), taken interval by interval and never below zero, as the node's monitor takes it; the monitor shares out nothing while no job runs, so quiet intervals are left out | [0.85, 1.02 + allowance] | billet shares the energy above `idle_package_watts` by each job's share of the host's busy CPU time, so the host's own work (billet, kernel networking, Ceph's OSDs serving the jobs' disks) stays unattributed by design; 15% is the most that should be on a host running only the kit. Attribution never hands out more than was measured, so above 1.02 is double counting, except for one thing the log cannot see: the monitor takes the baseline per tick, and its ticks fall between the log's rows, so it can count an excess in a second the log averaged with a dip. The allowance is that dip, measured: how far the quiet ends fell below the baseline per second, times the seconds a microVM was alive. |
| idle baseline | each quiet end's measured package power | `idle_package_watts` ± 5% | turbostat and the package counter agreed within 0.5% on this host's idle (73.1 W and 73.5 W, 2026-09-25); 5% leaves room for temperature. |

The energy check needs the whole window accounted for: every microVM the log saw must have a record with RAPL energy split by an idle baseline (`rapl`, not `rapl-unsplit`), the first and last ten rows must hold no microVM (a job crossing the window's edge has energy on both sides of it), every interval must have a RAPL reading on a clock that only moves forward, every row must have read which microVMs were running, and no interval a microVM was alive for may be longer than one and a half of the monitor's ticks, since the baseline cannot then be taken as the monitor took it (a slow BMC can stretch the log's rows that far; a second log run beside it with `--ipmitool none` keeps them at a second). Otherwise it is UNMEASURED and says which.

## What each result means

**PASS** means billet's figure is inside the stated range of the known answer in that run. Across runs, read the summary: the mean and its 95% confidence interval say where billet sits, and the CV how repeatable it is.

**UNMEASURED** means the comparison could not be made, and the line under it says why: billet did not measure the group (`billet check` on the node says which accounting the jailer was given), no usage report was recorded (the job ran on a node without monitoring, or the report has not arrived), no idle or baseline job in that run to subtract, a job that left no expectation (it failed before uploading, was skipped after an earlier failure, or its artifact was not downloaded), a record that does not carry the counter compared, or no record was collected.

**FAIL**, by job:

- **cpu low**: the cgroup missed time the guest spent (the VMM threads in another cgroup), or the host descheduled vCPU threads; compare the `vmm split` line of `billet jobs show`. **cpu high**: the cgroup charged work that was not the guest's, or the idle reference was unusually cheap; look at that run's idle result.
- **idle high**: a sleeping guest is costing CPU, which grows with T; rerun with a longer `seconds` to see whether it scales.
- **memory below X**: the peak was not read from the microVM's cgroup, or memory accounting was not enabled for it. **memory high**: the peak includes something other than the guest and its VMM, such as host page cache charged to the cgroup.
- **network low**: the tap's direction is reported the wrong way round (the download would then appear as sent), or the counter missed part of the transfer. **network high**: other traffic in the job, or headers on a path without offloads; look at the idle job's figure.
- **disk low**: the host's page cache absorbed the guest's writes and they were written back after the microVM's cgroup was gone, so the cgroup never saw them; this is the likeliest disk finding on a drive the VMM opens without O_DIRECT. **disk high**: write amplification between the guest and the host's block device.
- **energy below 0.85**: more of the host's own work than expected sat outside the jobs, or intervals broke for some job; the `unattributed` line gives it in watts. **energy above 1.02**: energy was attributed twice. **idle baseline FAIL**: `idle_package_watts` is stale; measure it again in a quiet window and correct the node's configuration.
- **A record naming another run**: the lease ran something else, so the job's runner name and billet's record disagree about which job a lease served.

**The BMC comparison** has no PASS: the BMC measures the whole server at the wall (fans, memory, disks, NICs and the power supply's loss) and RAPL the CPU package alone. The offset is roughly the platform's draw beyond the package, the slope how the wall follows the package (above 1 for the supply's loss and the loads that track the CPU), and r² how closely. A BMC reading is often an average over its own sampling period and lags RAPL by a sample or two, so read the fit over steady stretches with `--from` and `--to`. turbostat reads the same package counter, so its PkgWatt should agree with RAPL within a percent or two over the whole log; more than that means one of the two is reading another zone. Row by row the two are approximate: turbostat samples on its own clock, and each row carries the reading it printed since the row before, read beside the RAPL counter.

## Methodology

- **Idle baseline before and after.** Two quiet minutes at each end of the power log. The energy check reports the measured package power over each quiet end against `idle_package_watts`; a drift between them means the host warmed or something else started.
- **One discarded warmup.** The first run meets cold mirrors, caches and images, so its preparation differs from every later run's; `--discard 1` drops the earliest run by id.
- **Five or more measured runs.** The summary gives, per metric, the mean, the 95% confidence interval for the mean (Student's t on n−1 degrees of freedom; past 30 the next row down in the table, so the interval only widens) and the coefficient of variation. A CV above about 5% for cpu, network or disk means the host was not quiet enough to compare, whatever the verdicts say.
- **One run at a time on an otherwise idle tier.** The energy check refuses a window whose ends hold a microVM, and needs a record for every microVM in it, so other work on the node makes it UNMEASURED rather than wrong.

## What is not covered

The kit validates the Firecracker provider on Linux. Tart's process accounting and the docker provider are not exercised, nor are the pressure-stall figures, the OOM count or the guest/VMM thread split, which have no known answer a job can produce. RAPL is itself a model of the package's energy, and the BMC comparison relates the two without saying which is right.
