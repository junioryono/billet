# The known-answer kit on Tart

What billet measured of jobs whose answer was known before they ran, on the reference deployment's Mac (`macos-01`: Mac mini M5 Pro, 64 GB, macOS 27) under the Tart provider. [Measurement validation](../../operating/measurement-validation.md) built the kit for Firecracker on Linux, and said Tart's process accounting was not exercised. This is that exercise, run on 2026-11-15 from 10:26 to 11:42 UTC.

## The question and what would answer it

Does billet's host-side accounting of an arm64 Linux guest on Tart give the answers a known load should produce?
- **CPU:** N stress-ng workers for T seconds is N×T seconds of CPU.
- **Memory:** a load of X bytes held resident gives a peak of at least X, and at most the same run's idle job's peak plus 1.05 X plus 64 MiB.
- **Network:** a download of S bytes arrives as at least S.
- **Disk:** fio writing X bytes with `O_DIRECT`, and an fsync at its end, writes X.
- **Idle:** a sleeping guest costs almost nothing.

Success is the kit's own verdict, PASS, against the tolerances the kit states and argues for. Those tolerances were set for Firecracker; here they are applied unchanged.

## How it was run

- **The tier:** `billet-ci-4vcpu-ubuntu-2404-arm`, 4 vCPUs and 12 GiB, from the image `ubuntu-runner-arm64-r2.338.2`. It was offered to billet's own repository for this run by Spendify-Money/Platform#3514, and only `macos-01` serves it.
- **The caller:** `known-answer-run-mac.yml` (#892), with T = 60 s, 2 CPU workers, a 6 GiB memory load, the 135,390,644-byte Linux tarball from kernel.org, and 2 GiB written.
- **Six runs, one after another**, of six jobs each, one guest at a time: 36 jobs. The first run was the warmup and was discarded, as the kit's method says (`--discard 1 --runs 5`).
- **The revisions.** The warmup ran billet's `main` at `dedbbc96` and the five measured runs at `e5b4d256`; the caller, the job script and the checker are the same in both.
- **The checker:** `knownanswer collect` and `knownanswer check`, built from `e5b4d256`, ran on the control plane (billet v0.39.0) against its ledger. All 36 records were collected with usage recorded.
- **The evidence** (the expectations, the collected records and the checker's output) was kept on the operator's machine, not in the repository.
- **No energy check.** The kit's energy check reconciles RAPL with a power log, and a Tart node has no RAPL. Billet attributed this node's energy from its wall meter, which the kit does not check.

## Results

The checker's overall verdict was FAIL: 18 PASS, 5 FAIL and 2 UNMEASURED over the 25 measured jobs.

| Job | PASS | FAIL | UNMEASURED | Measured (mean over the runs measured) | Measured ÷ expected |
|---|---|---|---|---|---|
| idle | 4 | 0 | 1 | 1.5 s of CPU in the 61 s window (95% CI ±0.5 s) | |
| cpu | 5 | 0 | 0 | 118.5 s, against 120 s expected (±1.5 s, CV 1.0%) | 0.987 |
| memory | 0 | 5 | 0 | 9,107.6 MiB peak (±15.3 MiB, CV 0.1%) | 1.482 |
| network | 5 | 0 | 0 | 135.8 MiB received (CV 0.0%) | 1.0515 |
| disk | 4 | 0 | 1 | 2,048.6 MiB written (±0.8 MiB, CV 0.0%) | 1.000 |

**The two UNMEASURED.** Each was a load window that billet's per-second series did not cover whole: the sampler missed two reads (`failed_read`) of about two seconds each in one run's 61-second idle window, and 3.1 s of another run's six-second disk window. The kit refuses to judge a window it cannot see whole.

**The memory FAIL, run by run.** Each memory job's peak, against its run's idle job's, and the kit's accepted maximum: the idle peak, plus 1.05 X, plus 64 MiB.

| Run | Idle job's peak | Memory job's peak | Accepted at most | Over by |
|---|---|---|---|---|
| 43705067959 | 2,447.6 MiB | 9,117.7 MiB | 8,962.8 MiB | 154.9 MiB |
| 43706211435 | 2,436.1 MiB | 9,110.3 MiB | 8,951.3 MiB | 159.0 MiB |
| 43707212123 | 2,474.5 MiB | 9,116.4 MiB | 8,989.7 MiB | 126.7 MiB |
| 43708228905 | 2,408.8 MiB | 9,087.1 MiB | 8,924.0 MiB | 163.1 MiB |
| 43709219543 | 2,415.3 MiB | 9,106.4 MiB | 8,930.5 MiB | 175.9 MiB |

Every memory job's peak was 6,642 to 6,691 MiB above its run's idle peak, against the 6,144 MiB the load held: a mean of 527 MiB, or 8.6% of the load, beyond it. The kit allows 5% and 64 MiB.

## What it means

- **CPU, network and disk agree with their known answers on Tart.**
  - **CPU.** Billet measured 98.7% of the 120 CPU-seconds two workers ran.
  - **Network.** It measured the download at 1.0515 of its size, inside the kit's allowance for headers.
  - **Disk.** In the four windows it saw whole, it measured the 2 GiB `fio` wrote a mean of 0.031% high, each within 0.053%. So for this workload, `O_DIRECT` writes ended by an fsync, the writes reached the counter billet reads on macOS within the job's own window, which was not known before. Which of the two made them prompt was not separated.
- **A sleeping guest cost 1.5 s of CPU in a 61-second window.** That is 2.4% of one CPU across its 4 vCPUs, inside the kit's allowance for an idle guest.
- **The memory peak runs high, consistently.** Billet's peak for a Tart guest holding 6 GiB was a mean of 527 MiB more than the load beyond the idle job's peak, the same in every run to within 50 MiB, and over the kit's bound by 127 to 176 MiB, 1.4% to 2.0% of it.
  - The peak billet reads is the VM process's lifetime peak physical footprint, which holds what of guest memory the VM has touched. The guest's own view of its memory was not collected (these records carry no guest report), so whether the guest reused pages for the load or took fresh ones is not known.
  - Where the extra half gigabyte comes from was not measured. It could be the stressor's own pages, the guest kernel's page tables for the load, or what macOS counts in a VM process's footprint beyond guest memory.
  - The tolerance was set from Firecracker, whose host-side peak is a cgroup's. Whether the finding is billet's accounting on Tart or the tolerance's assumption about it is not decided by this run.

## What it does not establish

- **No energy check.** No energy figure was checked, since the kit's energy check needs RAPL, which Tart has not.
- **One tier, one shape.** It covers one Mac, one tier and one image, the arm64 Linux guest. A macOS guest on Tart was not run.
- **The memory finding is unexplained.** Explaining it needs the guest's own view of its memory beside the host's, which this caller did not collect.
