# CI capacity

What a pull request's CI costs the fleet it runs on, measured from billet's own runs between 2026-10-13 and 2026-10-21; the options for cutting a Go pull request's time to a verdict, compared on those numbers; and what was chosen (#237). Each runner is a microVM that holds its whole reservation, 4 GiB of memory per vCPU, for as long as its job runs. The fleet billet's CI shares with another organization is bound by memory before vCPU: on 2026-10-16 it had reserved 500 of 540 GiB against 126 of 196 vCPU, with about 50 jobs waiting for room.

## Where a Go pull request's time goes

| run | event | jobs | job-minutes | reserved vCPU-minutes | push to verdict |
|---|---|---|---|---|---|
| 39479210743 | push to main | 48 | 444 | 3,552 | 60 min |
| 39474794039 | push to main | 48 | 419 | 3,367 | 59 min |
| 39361492359 | Go pull request | 45 | 440 | 3,414 | 336 min |
| 39383209647 | Go pull request | 45 | 421 | 3,391 | 153 min |
| 39460063688 | Go pull request | 46 | 434 | 3,485 | 132 min |
| 39349872104 | docs-only pull request | 5 | 22 | 283 | 25 min |

The longest job in every Go run is about 32 minutes (`host-lifecycle (retirement-parity-network)`), so the 2 to 6 hours from push to verdict are queueing: a run waits for room on a fleet it shares. host-lifecycle is 21 of the jobs and about 2,200 of the 3,400 reserved vCPU-minutes, 62%. In the week to 2026-10-20 CI ran 120 times for pull requests (27 of them cancelled as superseded) and 59 times for pushes to main and release branches (21 replaced while pending), so a merge cost about two pull-request runs and one main run.

## What each host-lifecycle group uses

Measured by `scripts/ci-usage.sh`, which samples a job's `/proc/stat` and `/proc/meminfo` every five seconds and reports in the job's summary and its `ci-usage-<group>` artifact. The figures below are from its first version, which counted stolen time (the host running something else on the vCPU) as busy, so each is an upper bound on the cores the job ran on; the sampler now reports stolen time on its own. On the 8 vCPU runner (31 GiB) in run 39539367492 (2026-10-21):

| group | mean cores | peak cores | peak GiB held | minutes |
|---|---|---|---|---|
| `gate-binaries` | 2.65 | 7.67 | 1.49 | 1.6 |
| `endpoint` | 0.95 | 1.23 | 1.09 | 18.6 |
| `guard` | 1.03 | 1.23 | 1.10 | 28.2 |
| `policy` | 1.29 | 7.71 | 1.40 | 17.9 |
| `retirement` | 0.76 | 1.16 | 1.07 | 5.8 |
| `retirement-compatibility` | 1.12 | 1.27 | 1.10 | 15.9 |
| `retirement-node` | 0.98 | 1.20 | 1.07 | 5.0 |
| `retirement-node-settled` | 1.04 | 1.24 | 1.08 | 6.8 |
| `retirement-node-shape` (already on 2 vCPU) | 0.60 | 1.26 | 0.67 | 2.4 |
| `retirement-parity-basic` | 1.13 | 1.28 | 1.11 | 27.2 |
| `retirement-parity-negative` | 1.10 | 1.24 | 1.11 | 14.3 |
| `retirement-parity-network` | 1.10 | 1.26 | 1.12 | 31.9 |
| `retirement-recovery` | 1.06 | 1.27 | 1.09 | 9.2 |
| `retirement-request-cancellation` | 1.01 | 1.21 | 1.08 | 7.8 |
| `retirement-request-collection` | 1.00 | 1.24 | 1.11 | 7.2 |
| `retirement-request-evidence` | 1.07 | 1.45 | 1.11 | 7.2 |
| `retirement-request-retained` | 1.04 | 1.46 | 1.11 | 6.6 |
| `retirement-request-windows` | 1.07 | 1.28 | 1.12 | 8.3 |
| `retirement-resume` | 1.04 | 3.43 | 1.10 | 11.3 |
| `retirement-routes` | 1.06 | 1.29 | 1.09 | 8.3 |
| `shape` | 1.01 | 1.26 | 0.95 | 12.5 |

Every group runs Ansible one case at a time on binaries `gate-binaries` built once, so each keeps about one core busy and holds about a gigabyte. `guard` and `retirement-parity-network`, which #155 expected to need the large runner, are no different. `policy` alone peaks near eight cores, compiling two test binaries for its real-systemd step. `gate-binaries` compiles everything and keeps its 8 vCPU.

On the 2 vCPU runner (8 GiB) in run 39566892000 (2026-10-21):

| group | mean cores | peak cores | peak GiB held | minutes on 2 vCPU | minutes on 8 vCPU |
|---|---|---|---|---|---|
| `gate-binaries` (8 vCPU) | 2.64 | 7.38 | 1.44 | 1.8 | 1.6 |
| `endpoint` | 0.81 | 1.30 | 0.95 | 20.8 | 18.6 |
| `guard` | 0.88 | 1.32 | 0.95 | 30.1 | 28.2 |
| `policy` | 0.95 | 1.99 | 1.82 | 22.6 | 17.9 |
| `retirement` | 0.74 | 1.18 | 0.83 | 5.8 | 5.8 |
| `retirement-compatibility` | 0.87 | 1.29 | 0.96 | 18.3 | 15.9 |
| `retirement-node` | 0.92 | 1.30 | 0.94 | 5.5 | 5.0 |
| `retirement-node-settled` | 0.91 | 1.32 | 0.94 | 7.8 | 6.8 |
| `retirement-node-shape` | 0.36 | 1.37 | 0.74 | 5.5 | 2.4 |
| `retirement-parity-basic` | 1.05 | 1.32 | 0.97 | 28.1 | 27.2 |
| `retirement-parity-negative` | 0.95 | 1.29 | 0.96 | 16.1 | 14.3 |
| `retirement-parity-network` | 1.03 | 1.31 | 0.99 | 32.0 | 31.9 |
| `retirement-recovery` | 0.85 | 1.33 | 0.94 | 10.9 | 9.2 |
| `retirement-request-cancellation` | 0.97 | 1.30 | 0.95 | 8.1 | 7.8 |
| `retirement-request-collection` | 0.82 | 1.30 | 0.97 | 8.8 | 7.2 |
| `retirement-request-evidence` | 0.99 | 1.35 | 0.96 | 6.7 | 7.2 |
| `retirement-request-retained` | 0.75 | 1.34 | 0.98 | 8.9 | 6.6 |
| `retirement-request-windows` | 0.76 | 1.28 | 0.98 | 11.3 | 8.3 |
| `retirement-resume` | 0.91 | 1.32 | 0.98 | 12.6 | 11.3 |
| `retirement-routes` | 0.75 | 1.32 | 0.94 | 11.1 | 8.3 |
| `roles` | 1.02 | 1.34 | 0.82 | 18.0 | not measured |
| `shape` | 0.90 | 1.25 | 0.81 | 14.1 | 12.5 |

Every group passed. Memory never came near the runner's 7.8 GiB: the most any group held was 1.82 GiB. CPU did, in one place: `policy` used both cores (a peak of 1.99) for the few minutes it compiles. Every other group peaked at 1.37 cores or less. The longest group is still `retirement-parity-network` at 32.0 minutes against 31.9 on 8 vCPU, inside its 45-minute timeout. `policy` took 22.6 minutes against 17.9, which is what a compile on two cores instead of eight would cost. The rest moved by -0.5 to +3.0 minutes. That is comparable to what two runs of one group differ by on a shared fleet: `retirement-node-shape`, on 2 vCPU in both runs, moved by +3.1. One pair of runs cannot separate that variance from a small slowdown, and the `ci-usage` reports of later runs will. `roles` has no 8 vCPU figure because the baseline run was cancelled before it reported. Counted from GitHub's job records over the 20 groups both runs completed, host-lifecycle reserved 2,038 vCPU-minutes (257 job-minutes) on 8 vCPU and 579 (290 job-minutes) on 2 vCPU. The move therefore freed 1,459 vCPU-minutes, or 5,836 GiB-minutes, measured. With `roles` at its 2 vCPU time, about 1,570 vCPU-minutes are freed on every run that selects host-lifecycle.

## The options

Each option is judged on what it does to a Go pull request's verdict and a docs pull request's, what it reserves per pull-request run, what coverage it moves or loses, its exposure to flakes, and what it costs to keep. Unless a row says otherwise, the docs path is unchanged (25 minutes, 283 vCPU-minutes).

| option | Go verdict | reserved per PR run | coverage | flakes | cost to keep | verdict |
|---|---|---|---|---|---|---|
| **Run every host-lifecycle group on 2 vCPU** (#155) | shorter by the queue it frees: a quarter of host-lifecycle's reservation | about 1,850 vCPU-minutes instead of 3,400 (host-lifecycle 579 instead of 2,038 over the 20 groups measured in both runs), and the same in GiB at 4 a vCPU | unchanged: every group runs every case it ran | none added | a sampler that reports each group's use every run | **chosen** |
| Run host-lifecycle after merge, or nightly | shorter by host-lifecycle's whole reservation | about 1,200 | a pull request's green check no longer covers the roles, the guard or retirement; a break is found on main, after it merged | fewer per PR | a second gate and a rule for what main does when it fails | declined: CI is the gate |
| Select host-lifecycle by the Go import closure of the commands its shards run | unchanged | unchanged | none | none | a closure kept in step with the shards | ruled out: the shards run the real `billet` binary, whose closure is nearly all of `internal/`, and 50 of the last 60 merged pull requests selected host-lifecycle |
| Select host-lifecycle only for test files of the packages its steps compile | unchanged | unchanged | none | none | a rule per package | declined: of the last 60 merged pull requests, it would have spared none |
| Cancel a pull request's run when it merges | unchanged | 72 run-minutes in 30 merges | none | none | a workflow trigger | not worth it now: 2 of 51 pull-request runs ran on past their merge |
| Batch pull requests in a merge queue | unchanged per run; fewer main runs | unchanged | a batch that fails needs bisecting | a flake fails the batch | a queue and its configuration | declined: at a few merges a day there is little to batch |
| Cheaper slow cases (the R42 resume cases' three passes, `r3-lazy-*`) | a shorter longest job, about 32 minutes now | small | each case's coverage has to be kept | none | rewriting role scenarios | left for later: it shortens the longest job, not the queue |
| Rebalance the groups | a shorter longest job | none | none | none | the shard checker's partition | left for later, for the same reason |
| More capacity | shorter while unshared | unchanged | none | none | money | the owner's decision; right-sizing first shows what is needed |

## What would change the choice

A group whose `ci-usage` report shows it near its runner's two cores or eight gibibytes for long stretches goes back to a larger one. If the fleet stops being short of memory before vCPU, the saving is in vCPU alone and smaller. If merges grow to dozens a day, a merge queue starts to pay. A change that makes the role scenarios compile Go again, as every group did before `gate-binaries`, puts their runner size back in question.
