# Runtime predictions on the reference deployment

How well a job's runtime is predicted from the deployment's own history (#524, ADR-021), measured on the reference deployment's ledger on 2026-10-25 at 15:42 UTC. The reference controller then ran v0.28.0. The report was `billet jobs runtimes --since 336h --json`, from a binary built from #524's branch, whose ledger schema is v0.28.0's. It ran beside the live control plane as the billet user, with the ledger opened as a report (nothing created, locked, claimed or migrated), and took 8 seconds. The binary was removed afterwards.

## What was graded

Every job that started from 2026-10-11 15:42 to 2026-10-25 15:42 UTC and had finished: 30,253 jobs on 17 tiers, none left out by the read's bound. Each was predicted as a controller refreshing hourly would have predicted it, from the snapshot as of the hour it started in. That snapshot learned from the jobs that started in the 14 days before that hour and had finished by it, so no job was graded against itself or against a job still running when it started. The runtime is from the job's start to its lease's archive, both on billet's clock, so it includes the guest's teardown.

The error is the runtime less the predicted median, so a positive one ran longer than predicted, and the table gives its absolute value's median. **Within 2x** means the runtime was between half and twice the prediction.

## The result

| Tier | Jobs | Median error | Within 2x | From the job's own runs |
|---|---|---|---|---|
| `billet-2vcpu-ubuntu-2404` | 5,007 | 29s | 82% | 4,918 (85 across the matrix, 4 tier) |
| `billet-8vcpu-ubuntu-2404` | 7,417 | 41s | 96% | 7,415 (2 tier) |
| `billet-16vcpu-ubuntu-2404` | 340 | 1m05s | 96% | 340 |
| `billet-4vcpu-ubuntu-2404-arm` | 844 | 35s | 83% | 844 |
| `billet-6vcpu-macos-26` | 100 | 1m30s | 90% | 99 (1 tier) |
| `billet-image-builder-ubuntu-2404` | 16 | 33m12s | 88% | 16 |
| organization target, 2 vCPU | 10,383 | 8.4s | 61% | 10,337 (16 across the matrix, 30 tier) |
| organization target, 4 vCPU | 4,411 | 1m09s | 78% | 4,410 (1 tier) |
| organization target, 8 vCPU | 878 | 1m50s | 84% | 877 (1 tier) |
| organization target, 16 vCPU | 632 | 3m13s | 89% | 632 |
| a smaller target, 2 vCPU | 104 | 6.1s | 56% | 77 (20 tier, 7 default) |
| a smaller target, 4 vCPU | 53 | 46s | 47% | 35 (18 default) |
| another smaller target, 2 vCPU | 41 | 5.2s | 51% | 40 (1 tier) |
| another smaller target, 4, 6 (macOS) and 8 vCPU | 6 each | 13s, 38s, 3m34s | 100% each | all |
| a test tier of another session, 2 vCPU | 9 | 8m50s | 33% | none (4 tier, 5 default) |

The first six rows are billet's own CI, whose labels the repository publishes. The other targets' tiers are named by shape only.

Across all 30,253 jobs, 78% ran within 2x of the prediction. 30,058 (99.4%) were predicted from the job's own runs, 101 from the job's runs across its matrix, 64 from the tier and 30 from the 10 minute default.

## What it says

- **The wide shapes are the ones predicted best.** Every 8 and 16 vCPU tier with more than a handful of jobs ran 84% to 96% of its jobs within 2x, with a median error of one to three minutes (the smaller target's 8 vCPU tier, with six jobs, ran all six within 2x). Those are the leases whose ends decide when a host has room for a whole-host shape, which is what #527 asks the prediction for.
- **The 2 vCPU tiers are close in time and loose in ratio.** The organization target's 2 vCPU tier is off by a median of 8.4 seconds but only 61% within 2x: its jobs are seconds long, so a few seconds' difference is a large ratio. For choosing a host that difference does not matter, because a host's room comes back in the order its long leases end.
- **The fallbacks are rarely needed on a busy deployment.** Fewer than 1% of predictions came from the tier or the default. The ones that did are on tiers with a handful of jobs: a target that runs a few jobs a day, and another session's test tier. There the 10 minute default is far off (8m50s on the test tier), and that error is visible per job in `billet jobs show`.
- **The matrix level earns its place, narrowly.** 101 jobs had fewer than three runs of their own matrix cell and three or more across the matrix: new matrix values, mostly on billet's own 2 vCPU tier.
- **A long job's error is long in minutes and small in ratio.** The guest-image builder was off by a median of 33 minutes and still ran 88% of its jobs within 2x.

## What it does not say

- It grades the median alone. The overdue rule (once a job is past its median, half its median again or as long again as it has overrun, whichever is longer) and the p90 are not graded here; #527's replay judges whether they choose better hosts.
- A job's runtime is its lease's, from start to archive. A job whose teardown was slow looks longer than the job itself, and the record does not separate the two.
- The window holds two weeks of one deployment's work. A deployment whose jobs change shape week to week would predict worse, and the report on that deployment would say so.
