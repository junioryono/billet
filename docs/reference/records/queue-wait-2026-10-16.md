# Queue wait on a shared node

What made an organization's 2 and 4 vCPU jobs wait up to 55 minutes for a runner on 2026-10-16 (#523), read from the reference deployment's ledger and joined with GitHub's record of the same jobs. The organization's four pools (untrusted, organization target, answering to `billet-{2,4,8,16}vcpu-ubuntu-2404`) share one Firecracker node, `ubuntu-01`, with billet's own CI (a repository target, six tiers from 2 to 64 vCPU) and two smaller targets. Every number here was read on 2026-10-17, from the control plane's ledger opened read-only (`job_history` and `job_usage`), its journal, and the GitHub API. The ledger kept no GitHub queue time then (#525 adds it), so GitHub's side comes from its API and billet's from the ledger.

## The wait came before the purchase

For the job that waited longest in each tier of each run #523 cites, the wait GitHub records (`started_at` less `created_at`) is split at the moment billet bought the lease that ran the job (the history row's `assigned_at`, which a pool slot writes in the pass that buys it) and at billet's record of the start. Each cell is the whole wait, then the time to the purchase plus the time from the purchase to the start:

| Run | Created (UTC) | 2 vCPU | 4 vCPU | 8 vCPU | 16 vCPU |
|---|---|---|---|---|---|
| 38562082619 | 10-14 12:12 | 0.4m: 0.0m + 31s | 0.6m: 0.3m + 19s | 0.3m: 0.0m + 27s | 0.3m: 0.0m + 27s |
| 38566718781 | 10-14 12:49 | 0.4m: 0.0m + 27s | 0.5m: 0.2m + 60s | 0.3m: 0.0m + 53s | 0.2m: 0.0m + 55s |
| 38600651808 | 10-14 16:44 | 0.4m: 0.0m + 28s | 0.4m: 0.2m + 58s | 0.3m: 0.0m + 55s | 0.3m: 0.0m + 56s |
| 38691178303 | 10-15 06:13 | 1.2m: 0.0m + 75s | 1.0m: 0.4m + 53s | 0.3m: 0.0m + 56s | 0.3m: 0.0m + 56s |
| 38704090318 | 10-15 08:13 | 4.5m: 4.3m + 39s | 9.6m: 9.4m + 54s | 3.6m: 3.4m + 55s | 2.8m: 2.6m + 53s |
| 38835628763 | 10-16 01:32 | 17.4m: 17.2m + 29s | 31.3m: 31.0m + 56s | 13.9m: 13.6m + 54s | 14.8m: 14.6m + 50s |
| 38836240892 | 10-16 01:38 | 22.2m: 21.5m + 47s | 26.0m: 25.4m + 50s | 8.3m: 8.0m + 53s | 1.4m: 0.8m + 62s |
| 38862014092 | 10-16 06:34 | 52.2m: 51.9m + 61s | 13.4m: 13.1m + 50s | 2.0m: 1.8m + 55s | 2.8m: 1.7m + 111s |
| 38863912710 | 10-16 06:54 | 39.9m: 39.4m + 34s | 20.3m: 20.1m + 33s | — | 3.2m: 2.5m + 55s |
| 38869060220 | 10-16 07:43 | 52.9m: 52.3m + 39s | 52.5m: 52.2m + 21s | — | 7.7m: 7.3m + 55s |
| 38914298475 | 10-16 14:15 | 0.4m: 0.0m + 31s | 0.5m: 0.3m + 57s | 0.4m: 0.0m + 55s | 0.3m: 0.0m + 57s |
| 38935085960 | 10-16 16:50 | 18.6m: 18.0m + 44s | 55.5m: 55.0m + 54s | 15.5m: 15.2m + 54s | 9.0m: 8.6m + 58s |
| 38947550982 | 10-16 18:28 | 1.0m: 0.8m + 24s | 4.3m: 4.1m + 54s | 1.1m: 0.9m + 55s | 1.1m: 0.9m + 56s |
| 38959159964 | 10-16 20:01 | 4.9m: 4.2m + 47s | 16.6m: 16.3m + 65s | 2.9m: 2.6m + 58s | 2.2m: 1.8m + 59s |

In every slow cell the time went before billet bought the lease, and once it had, the runner started in 19 to 111 seconds. Neither the launch nor the runner's registration lagged, and no scale set advertised idle runners while jobs queued: a pool member is bought for work GitHub has assigned, and these were bought late.

## The node had no room

Every Linux tier on `ubuntu-01` charges 4 GiB per vCPU, and the node's `max_memory` is 480 GiB, so memory runs out at 120 of its 180 vCPU. The share of those 480 GiB that leases held, hour by hour (UTC), with the organization's and billet's own CI's parts:

| Hour | 2026-10-14 | 2026-10-16 |
|---|---|---|
| 05 | 13% | 72% (org 32, billet 40) |
| 06 | 3% | 96% (org 72, billet 21) |
| 07 | 48% | 97% (org 55, billet 42) |
| 08 | 72% | 96% (org 67, billet 29) |
| 09 | 83% | 95% (org 54, billet 41) |
| 10 | 97% | 97% (org 44, billet 52) |
| 11 | 65% | 96% (org 50, billet 46) |
| 12 | 32% | 96% (org 49, billet 48) |
| 13 | 15% | 79% (org 23, billet 55) |
| 14 | 1% | 22% |
| 15 | 33% | 14% |
| 16 | 53% | 49% |
| 17 | 29% | 96% (org 81, billet 15) |
| 18 | 51% | 94% (org 51, billet 43) |
| 19 | 0% | 43% |
| 20 | 0% | 97% (org 52, billet 44) |
| 21 | 0% | 97% (org 55, billet 42) |

On the 14th the node filled for an hour or two and stood near empty for long stretches. On the 16th it held 94 to 97% from 06:00 to 13:00 and most of the evening; it was at 472 GiB or more for 8.2 hours of the day, against 1.7 to 3.9 hours on each of the five days before. The 3 to 6% left over is the gap between a lease ending and the next purchase, and fragments too small for the shapes waiting. Taken moment by moment, a waiting job's shape fitted the free memory for about half its wait, but each fit lasted only until the next scale-set message bought into it, and the tier was buying throughout: the 2 vCPU tier bought 34 leases in the 52 minutes run 38869060220's `Detect Changes` waited.

The demand behind it, per day on `ubuntu-01`, from `job_history` (jobs, and GiB-hours charged):

| Day | Organization | billet's own CI | Others |
|---|---|---|---|
| 10-11 | 235, 606 | 557, 2,895 | — |
| 10-12 | 357, 743 | 445, 2,456 | 2, 0 |
| 10-13 | 424, 861 | 688, 3,444 | 108, 27 |
| 10-14 | 776, 1,685 | 475, 2,650 | 55, 27 |
| 10-15 | 709, 1,459 | 276, 1,290 | — |
| 10-16 | 1,974, 5,264 | 809, 3,871 | 7, 16 |

The organization's load tripled between the 14th and the 16th, 1,596 of its jobs on the 16th from `pull_request` events, while billet's own CI ran beside it at its own busiest. The step change came from the workload, not from a release. Three rolled out in the window (v0.16.4 on the 14th at 11:27, v0.16.5 on the 15th at 09:20 and v0.16.6 on the 16th at 09:24, each by the stable channel's rollout) and the waits follow the load across all three: v0.16.4 ran the 14th's sub-minute runs and the 15th's 9.6-minute one, and v0.16.6 the 16th's 0.4-minute run at 14:15, with the node a fifth full, and its 55-minute one at 16:50, with it full. The first slow run's trigger commit touched one workflow file.

## Why a small job waited longer than a large one

Not head-of-line blocking. Under `server.admission_order: fair` (the default) the tiers waiting on a node buy in turn, one purchase each, and a large shape holds freed room only while it is the longest waiter. What differed was each tier's own queue at GitHub. From 07:00 to 08:20 on the 16th the organization's 2 vCPU label had 20 to 33 jobs queued, and its 16 vCPU label 0 to 2. GitHub hands a new runner of a scale set to that set's queue roughly in order (of 7,524 pairs of 2 vCPU jobs queued at once, 89% started in the order they were created), so a 2 vCPU job waited behind its own tier's backlog while a 16 vCPU job, alone in its queue, started with the next 64 GiB that freed. A tier's wait grows with its own backlog: taking turns is fair between tiers, not between jobs.

What changes that now:

- **A target's share.** `max_vcpu` and `max_memory` on a target's block cap what that target's tiers hold between them ([A target's share](../configuration.md#a-targets-share)), so billet's own CI cannot take the organization's room, or the other way round. On the 16th the two together asked for more than the node holds, so a share decides whose jobs wait, not whether any do.
- **`server.admission_order: fill`** starts whatever fits, which shortens the small shapes' wait at the cost of the largest ones'.
- **The ceiling.** More memory for `node.max_memory`, or tiers that charge less than 4 GiB per vCPU, raise the 120 vCPU ceiling. The node's measured peak memory per job includes the host's page cache for the guest's disk, so it is not evidence that the guests would fit in less.

Dividing contended capacity by weight is #530; predicting when a host frees up, so a large shape is placed rather than waited for, is #524 and #527. `billet jobs wait` (#525) reports each tier's wait split the way the table above is, from the ledger alone, for jobs assigned after it shipped.

## The cold lint runs

The cited `Lint` jobs' cold `golangci-lint` steps took 491 to 714 seconds. The host measured no contention in any of them: the job's CPU pressure was a second or less, its I/O pressure 15 seconds or less, its vCPU threads ran at 1.2 to 1.5 instructions a cycle with 16 to 19% of cycles stalled in the frontend, and its disk read was 1.6 GiB in every run. The 491-second run and a 619-second one ran with the node a fifth committed (39 and 35 of 180 vCPU), a 612-second one with it full. The spread is the work, what a cold build cache leaves `golangci-lint` to type-check, not the guest or its neighbours. Run 38869060220's slow step, after a 604-second restore, has no usage report to compare.

## The Actions cache and the uplink

The organization's pools have no repository scope, so billet serves them no local Actions cache, and every `actions/cache` call goes to GitHub over the site's line. The line is not what is slow: in the windows of the cited restore and save, the node's guests together sent 6 to 10 MB/s and received about 21 MB/s (each job's tap counters, prorated over the window), against the 400 Mbit/s (50 MB/s) each way the node is held to. A save at 1.3 MB/s is one transfer's throughput to GitHub's storage, which the local cache removes rather than speeds up.

The organization has one repository, so a static `cache_scope` naming it gives every job on its pools the local cache, and a per-job scope would add nothing there ([Transparent Actions cache](../../operating/actions-cache.md#an-organization-target)). Two things come first. The organization's App must hold `actions: read`: its installation held `metadata: read` and `organization_self_hosted_runners: write` only, and default-branch publication is proved from GitHub's record of each run, so without it nothing publishes. And the scope turns on the Go and Bazel caches too, which with nothing published would be cold and, through `GOCACHEPROG`, would replace the build cache the workflow restores itself, so the scope goes on only after the permission. A 1.3 GB restore measured before and after belongs here once both are done.
