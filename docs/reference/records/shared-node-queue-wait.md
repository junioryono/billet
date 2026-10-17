# Queue wait on a shared node

What made an organization's 2 and 4 vCPU jobs wait up to 55 minutes for a runner on 2026-10-16 (#523), read from the reference deployment's ledger and joined with GitHub's record of the same jobs. The organization's four pools (untrusted, organization target, answering to `billet-{2,4,8,16}vcpu-ubuntu-2404`) share one Firecracker node, `ubuntu-01`, with billet's own CI (a repository target, six tiers from 2 to 64 vCPU) and two smaller targets. Every number here was read on 2026-10-17, from the control plane's ledger opened read-only (`job_history` and `job_usage`), its journal, and the GitHub API. The ledger kept no GitHub queue time then (#525 adds it), so GitHub's side comes from its API and billet's from the ledger.

## The wait came before the purchase

For the job that waited longest in each tier of each run #523 cites, the wait GitHub records (its `started_at` less its `created_at`) is split at the moment billet assigned the job's lease to a pool slot (the history row's `assigned_at`). A pool slot is assigned in the pass that buys it, unless it took escrow bought earlier for an offer, so this is the purchase or later. Each cell is the whole wait, then the part before that moment and the part after it, to GitHub's start. The first part crosses GitHub's clock and billet's, and it holds GitHub's own hand-over to the scale set too, which rows from before #525 cannot separate:

| Run | Created (UTC) | 2 vCPU | 4 vCPU | 8 vCPU | 16 vCPU |
|---|---|---|---|---|---|
| 38562082619 | 10-14 12:12 | 23s = 1s + 22s | 34s = 17s + 17s | 16s = 1s + 15s | 19s = 1s + 18s |
| 38566718781 | 10-14 12:49 | 22s = 1s + 21s | 29s = 10s + 19s | 16s = 2s + 14s | 15s = 1s + 14s |
| 38600651808 | 10-14 16:44 | 23s = 2s + 21s | 24s = 11s + 13s | 18s = 2s + 16s | 20s = 2s + 18s |
| 38691178303 | 10-15 06:13 | 70s = 1s + 69s | 62s = 27s + 35s | 18s = 1s + 17s | 20s = 1s + 19s |
| 38704090318 | 10-15 08:13 | 4.5m = 4.3m + 13s | 9.6m = 9.4m + 14s | 3.6m = 3.4m + 14s | 2.8m = 2.6m + 13s |
| 38835628763 | 10-16 01:32 | 17.4m = 17.2m + 13s | 31.3m = 31.0m + 16s | 13.9m = 13.6m + 14s | 14.8m = 14.6m + 15s |
| 38836240892 | 10-16 01:38 | 22.2m = 21.5m + 44s | 26.0m = 25.4m + 35s | 8.3m = 8.0m + 22s | 84s = 48s + 36s |
| 38862014092 | 10-16 06:34 | 52.2m = 51.9m + 23s | 13.4m = 13.1m + 16s | 2.0m = 1.8m + 15s | 2.8m = 1.7m + 67s |
| 38863912710 | 10-16 06:54 | 39.9m = 39.4m + 28s | 20.3m = 20.1m + 11s | — | 3.2m = 2.5m + 38s |
| 38869060220 | 10-16 07:43 | 52.9m = 52.3m + 32s | 52.5m = 52.2m + 16s | — | 7.7m = 7.3m + 24s |
| 38914298475 | 10-16 14:15 | 26s = 2s + 24s | 29s = 16s + 13s | 22s = 1s + 21s | 19s = 1s + 18s |
| 38935085960 | 10-16 16:50 | 18.6m = 18.0m + 34s | 55.5m = 55.0m + 32s | 15.5m = 15.2m + 20s | 9.0m = 8.6m + 21s |
| 38947550982 | 10-16 18:28 | 60s = 46s + 14s | 4.3m = 4.1m + 14s | 67s = 53s + 14s | 68s = 53s + 15s |
| 38959159964 | 10-16 20:01 | 4.9m = 4.2m + 38s | 16.6m = 16.3m + 16s | 2.9m = 2.6m + 19s | 2.2m = 1.8m + 29s |

In every slow cell the time went before the lease was assigned, and once it was, GitHub started the job 11 to 67 seconds later. No first part is negative: every lease was bought after GitHub created its job, so no runner stood idle while these jobs queued, and neither the launch nor the runner's registration lagged. Billet's own record of the start trails GitHub's by 2 to 48 seconds, the time to deliver and handle the JobStarted message.

## The node was full

Every Linux tier on `ubuntu-01` charges 4 GiB per vCPU, and the node's `max_memory` is 480 GiB, so memory runs out at 120 of its 180 vCPU. The share of those 480 GiB that leases held, hour by hour (UTC), with the organization's part, billet's own CI's and the other targets':

| Hour | 2026-10-14 | 2026-10-16 |
|---|---|---|
| 05 | 13% | 72% (org 32, billet 40) |
| 06 | 3% | 96% (org 72, billet 21, others 3) |
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

On the 14th the node filled for an hour or two and stood near empty for long stretches. On the 16th it held 94 to 97% from 06:00 to 13:00 and most of the evening; it was at 472 GiB or more for 8.2 hours of the day, against 1.7 to 3.9 hours on each of the five days before. The 3 to 6% left over, 14 to 29 GiB and so room for one to three 2 vCPU jobs, is room between a lease ending and the next purchase, and room the admission order held for a larger waiter; the ledger does not say which. Taken moment by moment, a waiting job's shape fitted the free memory for about half its wait; each fit lasted until the next purchase, by whichever tier the order admitted, and the tier was buying throughout: the 2 vCPU tier bought 34 leases in the 52 minutes run 38869060220's `Detect Changes` waited.

The demand behind it, per day on `ubuntu-01`, from `job_history` (jobs, and GiB-hours charged):

| Day | Organization | billet's own CI | Others |
|---|---|---|---|
| 10-11 | 235, 606 | 557, 2,895 | — |
| 10-12 | 357, 743 | 445, 2,456 | 2, 0 |
| 10-13 | 424, 861 | 688, 3,444 | 108, 27 |
| 10-14 | 776, 1,685 | 475, 2,650 | 55, 27 |
| 10-15 | 709, 1,459 | 276, 1,290 | — |
| 10-16 | 1,974, 5,264 | 809, 3,871 | 7, 16 |

The organization's load tripled by GiB-hours between the 14th and the 16th (2.5 times by jobs), 1,596 of its jobs on the 16th from `pull_request` events, while billet's own CI ran beside it at its own busiest. The step change came from the workload, not from a release. Three rolled out in the window (v0.16.4 on the 14th at 11:27, v0.16.5 on the 15th at 09:20 and v0.16.6 on the 16th at 09:24, each by the stable channel's rollout) and the waits follow the load across all three: v0.16.4 ran the 14th's sub-minute runs and the 15th's 9.6-minute one, and v0.16.6 the 16th's 0.4-minute run at 14:15, with the node a fifth full, and its 55-minute one at 16:50, with it full. The first slow run's trigger commit touched one workflow file.

## Why a small job waited longer than a large one

Partly head-of-line blocking, by design, and partly each tier's own backlog. Under `server.admission_order: fair` (the default) the longest-waiting tier holds freed room until its shape fits, and another tier buys out of turn only when that leaves each waiter ahead room for one lease of its own; on this node the organization's 16 vCPU tier and billet's own 32 and 64 vCPU tiers were waiters of that kind. A tier whose turn it is buys one pool pass, up to four leases on a Firecracker-only tier, before it goes to the back. The ledger does not record which tier the order held room for, so how much of the 2 vCPU tier's wait that cost is not measured; that its shape fitted the free memory for about half its wait is consistent with it.

What the ledger and GitHub do show is each tier's own queue. From 07:00 to 08:20 on the 16th the organization's 2 vCPU label had 20 to 33 jobs queued at GitHub, and its 16 vCPU label 0 to 2. GitHub hands a scale set's new runner to that set's queue roughly in order: of 7,524 pairs of 2 vCPU jobs queued at once, 89% started in the order they were created, against 50% for no order, which says a job waits behind the older ones rather than that GitHub is strictly first-come. So a 2 vCPU job waited behind its own tier's backlog, while a 16 vCPU job, alone in its queue, started with the next 64 GiB the order let it have.

Only room shortens the total. Who carries it can be moved:

- **A target's share.** `max_vcpu` and `max_memory` on a target's block cap what that target's tiers hold between them ([A target's share](../configuration.md#a-targets-share)), so billet's own CI cannot take the organization's room, or the other way round. On the 16th the two together asked for more than the node holds, so a share decides whose jobs wait, not whether any do.
- **`server.admission_order: fill`** starts whatever fits, which shortens the small shapes' wait, and the largest shape may then wait indefinitely; on this node those are billet's own 32 and 64 vCPU tiers.
- **The ceiling.** More memory for `node.max_memory`, or tiers that charge less than 4 GiB per vCPU, raise the 120 vCPU ceiling. The node's measured peak memory per job includes the host's page cache for the guest's disk, so it is not evidence that the guests would fit in less.

An order by a job's age rather than by tier, which billet does not have, would move the wait between jobs too. Dividing contended capacity by weight is #530, and predicting when a host frees up, so a large shape is placed rather than waited for, is #524 and #527. `billet jobs wait` (#525) reports each tier's wait split this way from the ledger alone, with GitHub's hand-over to the scale set separated, for jobs that start after it ships.

## The cold lint runs

The cited `Lint` jobs' cold `golangci-lint` steps took 491 to 714 seconds. Nine of the ten have a usage report, and the host measured no contention in any of them: the job's CPU pressure was a second or less, its I/O pressure 15 seconds or less, its vCPU threads ran at 1.2 to 1.5 instructions a cycle with 16 to 19% of cycles stalled in the frontend, and its disk read was 1.6 GiB in every run. Similar times with the node a fifth committed and full (619 and 612 seconds) rule out the neighbours. That the remaining spread is the work, what a cold build cache leaves `golangci-lint` to type-check at each commit, is inferred; the runs' instruction counts were not compared. Run 38869060220's slow step, after a 604-second restore, is the one with no usage report.

## The Actions cache and the uplink

The organization's pools have no repository scope, so billet serves them no local Actions cache, and every `actions/cache` call goes to GitHub over the site's line: the ledger records the Actions cache as `off` for every one of the pools' jobs assigned from the 16th until it was read, 2,049 of them. Before: in run 38863912710 a 1.3 GB restore took 4 minutes 24 seconds, at 5 to 7 MB/s, and its save 1,043 seconds, at 1.3 MB/s (from GitHub's job log). The node's own guests were not filling its share of the line: in the windows of that restore and save they sent 6 to 10 MB/s and received about 21 MB/s together (each job's tap counters, prorated over the window), under the 400 Mbit/s (50 MB/s) the node's shaper allows each direction at most. What rate the shaper held in those windows, and what the site's other hosts moved, were not read, so whether the line was the limit is not measured. A save at 1.3 MB/s is what the local cache removes, whatever limits it.

The organization has one repository, so a static `cache_scope` naming it makes every job on its pools eligible for the local cache ([An organization target](../../operating/actions-cache.md#an-organization-target)); a fork's pull request, or any job whose authority cannot be proved, still goes to GitHub. Two things come first. The organization's App must hold `actions: read`: its installation held `metadata: read` and `organization_self_hosted_runners: write` only, and default-branch publication is proved from GitHub's record of each run, so without it nothing publishes. And the scope turns on the Go and Bazel caches too, which with nothing published would be cold and, through `GOCACHEPROG`, would replace the build cache the workflow restores itself, so the scope goes on only after the permission. The same restore measured after belongs here once both are done.
