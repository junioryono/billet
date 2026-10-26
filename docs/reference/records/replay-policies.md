# Admission orders on replay

How `fair` and `fill` divide room between shapes that share hosts (#526), replayed through billet's real listener, allocator, placer and node over the simulated backend at compressed time, on 2026-10-25. Two workloads: a synthetic mix with a whole-host shape beside smaller ones, and the reference deployment's own week, exported from its ledger with `billet jobs export`. Both ran on the shared Linux test host, one replay per policy: the synthetic mix under the host's heavy lock, and the reference week beside other work at low priority, which changes how long a replay takes and not what it measures, because a replay's clock is virtual. The last section says how to repeat them. This is the baseline #527's `hold` order is judged against.

## How it was replayed, and what that leaves out

Both comparisons ran under GitHub.com's direct assignment, where each tier's assigned-but-unstarted jobs are the waiters the admission order ranks. Under GHES's offers no tier is ever recorded as waiting, so `fair` and `fill` give the same result: the first wide run, under offers, gave identical tables for both, which is how #686 was found. Until #686 is fixed, `fair` on an offers deployment is `fill`.

What the harness models and what it does not:

- **The guest is simulated.** A runner boots in 30 seconds and its job runs for exactly the duration the trace gives, then the lease is archived. A guest's real boot, pull and teardown are not modelled.
- **The waiters are ordered by event.** The admission queue is on wall time, and the replay compresses time, so waiter order follows the order events are delivered and the stall bypass (`WaiterAllowance`) is never exercised: a listener acts only on an event.
- **A macOS tier is a Linux guest on its pinned host.** Its shape and its host are kept; the backend is not.
- **A host is room, not a machine.** Placement charges vCPU and memory against the host's declared capacity, which is what billet's own placement does; memory bandwidth, disks and the uplink are not modelled.

The tables give each shape's queue wait: from the job's arrival (GitHub's queue time) to its start, nearest rank. **Still queued** and **oldest queued** are the jobs that had arrived and not started when a replay with a horizon ended, which the percentiles leave out. **Jobs refused at least once** counts the jobs billet declined to buy a runner for at least once, each job once, the one figure the harness counts rather than reads from the ledger. **vCPU charged** and **memory charged** are the mean share of the deployment's room that leases held, time-weighted over the policy's own span, from its first charge to its last end (or to the horizon, for a replay that had one), which **Over** gives in hours: the same charged hours over a longer span are a smaller share. **Empty host-hours** are the hours a host held nothing over that span.

## A whole-host shape beside smaller ones

`WideShapeMix` with seed 1: 2,000 jobs on four 32 vCPU hosts with tiers of 2, 4, 8, 16 and 32 vCPU, at 4 GiB per vCPU, offered at 85% of the room. Five percent of the jobs are whole-host; the rest are spread over the smaller tiers, the smallest the most common. Runtimes are lognormal around a median of 8 minutes (σ 0.6), and arrivals are a Poisson stream. Every job started and finished in both replays, and the ledger proved no overcommit.

| Shape | Policy | Started | p50 | p90 | p99 | p99.99 | Max |
|---|---|---|---|---|---|---|---|
| 2 vCPU | fair | 913 | 14m09s | 53m28s | 1h12m08s | 1h17m00s | 1h17m00s |
| 2 vCPU | fill | 913 | 30s | 2m32s | 7m33s | 10m24s | 10m24s |
| 4 vCPU | fair | 455 | 25m32s | 52m00s | 1h14m41s | 1h20m58s | 1h20m58s |
| 4 vCPU | fill | 455 | 30s | 3m28s | 8m59s | 13m26s | 13m26s |
| 8 vCPU | fair | 317 | 2h07m01s | 4h35m04s | 5h12m09s | 5h18m36s | 5h18m36s |
| 8 vCPU | fill | 317 | 30s | 7m12s | 19m05s | 20m16s | 20m16s |
| 16 vCPU | fair | 211 | 45m38s | 2h23m19s | 2h57m21s | 3h04m46s | 3h04m46s |
| 16 vCPU | fill | 211 | 3m14s | 17m39s | 25m26s | 26m41s | 26m41s |
| 32 vCPU | fair | 104 | 12m37s | 22m48s | 28m35s | 33m11s | 33m11s |
| 32 vCPU | fill | 104 | 51m43s | 1h29m39s | 1h46m21s | 1h50m06s | 1h50m06s |

No job was still queued at the end of either replay, so the table leaves out the backlog columns.

| Policy | Started | Jobs refused at least once | vCPU charged | Memory charged | Empty host-hours | Over (hours) |
|---|---|---|---|---|---|---|
| fair | 2,000 | 1,747 | 81% | 81% | 1.0 | 21.1 |
| fill | 2,000 | 764 | 87% | 87% | 1.0 | 19.7 |

What it says:

- **`fill` starves the whole-host shape, as #527 says it would.** A 32 vCPU job needs an empty host, and under `fill` a host never empties while smaller jobs keep arriving: its p99 is 1h46m against 28m under `fair`.
- **`fair` buys the wide shape its turn with every other shape's.** Holding the line for the 32 vCPU waiter holds freed room on every host it shares, so every smaller shape waits behind it: the 8 vCPU p99 goes from 19 minutes to 5h12m, and the median 2 vCPU job from 30 seconds (the boot) to 14 minutes. The room held idle shows in the time it took: the same 2,000 jobs ran over 21.1 hours under `fair` against 19.7 under `fill`, which is the 6 points less vCPU charged, and `fair` refused 2.3 times as many jobs at least once.
- **Neither empties a host for long.** Both left one host-hour empty in all. The room `fair` holds is a slice of every host the wide shape could use, never a whole one, which is the waste #527's `hold` order replaces by holding one host.

## The reference deployment's week

The reference deployment's ledger, exported on 2026-10-25 with `billet jobs export --since 168h` by a binary from #526's branch with the controller's schema, run beside the live control plane as the ledger's owner and removed afterwards. Only the trace and the fleet left the host, with stand-ins for every owner, repository, workflow, job name and run. The window held 20,192 assigned jobs, from 2026-10-18 20:51 to 2026-10-25 18:37 UTC. The export left out 406 that never started, 20 whose completion was not after their start and 9 of tiers the configuration no longer declares, and 989 jobs had no GitHub queue time on record and arrive when billet was assigned them. That left 19,757 jobs. The export ran before two later changes to it, neither of which changes which jobs the trace holds: it now leaves out unread a pool slot no job reached, which the 406 may include, and gives an owner never recorded a stand-in rather than the trace's default.

The fleet is the deployment's as it registered: one Linux host of 180 vCPU and 480 GiB (Firecracker) and one macOS host of 16 vCPU (Tart), with a ceiling of 196 vCPU and 540 GiB. It has 21 tiers on four targets, from 2 to 64 vCPU, four of them 6 vCPU macOS tiers pinned to the macOS host. The week ran jobs of five shapes, from 2 to 16 vCPU; no job of 32 vCPU or more ran in it. Every job started and finished under both policies, and the ledger proved no overcommit.

| Shape | Policy | Started | p50 | p90 | p99 | p99.99 | Max |
|---|---|---|---|---|---|---|---|
| 2 vCPU | fair | 11,860 | 30s | 8m21s | 21m09s | 32m56s | 33m08s |
| 2 vCPU | fill | 11,860 | 30s | 4m13s | 22m16s | 48m06s | 48m39s |
| 4 vCPU | fair | 2,978 | 30s | 7m54s | 17m19s | 28m57s | 28m57s |
| 4 vCPU | fill | 2,978 | 30s | 6m37s | 38m35s | 1h01m01s | 1h01m01s |
| 6 vCPU (macOS) | fair | 68 | 1m25s | 9m51s | 15m02s | 15m02s | 15m02s |
| 6 vCPU (macOS) | fill | 68 | 1m28s | 8m27s | 36m34s | 36m34s | 36m34s |
| 8 vCPU | fair | 4,270 | 7m28s | 44m48s | 1h38m33s | 2h03m38s | 2h03m38s |
| 8 vCPU | fill | 4,270 | 6m19s | 21m22s | 40m28s | 1h11m39s | 1h11m39s |
| 16 vCPU | fair | 581 | 30s | 4m24s | 8m10s | 13m05s | 13m05s |
| 16 vCPU | fill | 581 | 30s | 27m39s | 2h13m12s | 4h22m00s | 4h22m00s |

| Policy | Started | Jobs refused at least once | vCPU charged | Memory charged | Empty host-hours | Over (hours) |
|---|---|---|---|---|---|---|
| fair | 19,757 | 9,782 | 37% | 54% | 87.9 | 166.2 |
| fill | 19,757 | 8,237 | 37% | 54% | 89.0 | 166.2 |

What it says:

- **`fill` starves the widest shape here too.** The deployment's widest jobs that week were 16 vCPU, and under `fill` their p99 is 2h13m and their longest wait 4h22m, against 8 minutes and 13 minutes under `fair`. The 4 and 6 vCPU tiers' p99 are also better under `fair` (17 minutes against 39, and 15 against 37).
- **Under `fair`, the 8 vCPU tier pays.** Its p99 is 1h39m against 40 minutes under `fill`, and its p90 45 minutes against 21. It is the most numerous shape above 4 vCPU, 4,270 jobs. The 2 vCPU tier's p90 doubles (8 minutes against 4), while its p99 is about the same and its longest wait shorter. The replay does not say why the 8 vCPU tier is the one that waits; #527's replay, which names the holds it makes, is where to look.
- **The week's arrivals set the span, and memory binds before vCPU.** Both policies drained within the week, so both are measured over the same 166 hours and charged the same shares. Memory charged is 54% against 37% of vCPU, because all but one of the Linux tiers charge 4 GiB per vCPU (one 4 vCPU tier charges 12 GiB) against a host of 480 GiB and 180 vCPU, which runs out of memory near 120 vCPU, as [Queue wait on a shared node](shared-node-queue-wait.md) found.
- **The deployment ran `fair` that week**, its configuration's default, and nothing here argues for `fill` instead: `fill` buys the 8 vCPU tier a shorter tail at the price of the 16 vCPU tier's, which is the trade #527's `hold` order is meant to remove.

What it does not say:

- The replayed waits are not checked against the waits the deployment measured that week (`billet jobs wait`). The harness's boot, its event-ordered waiters and its absence of real teardown make them a comparison between policies, not a prediction of either.
- The empty host-hours include a host idle while the last jobs finish and the macOS host between its few jobs; the quantity #527 is judged on, host-hours empty while a job of a shape it fits was queued, is #527's to measure.

## Reproducing it

Each comparison is `TestComparePolicies` in `internal/replay`, run by hand: `BILLET_REPLAY_COMPARE=wide` for the synthetic mix (`BILLET_REPLAY_JOBS` sizes it), or `BILLET_REPLAY_COMPARE=file` with `BILLET_REPLAY_TRACE` and `BILLET_REPLAY_FLEET` naming what `billet jobs export` wrote. `BILLET_REPLAY_PROTOCOL=offer` replays under GHES's offers instead, and `BILLET_REPLAY_POLICY` replays one policy alone, so the policies of a long comparison can run as processes side by side. With `-replay.out DIR` the table is written to `DIR/compare-<mode>[-<policy>].md`. The wide comparison took 21 minutes on the shared host's 16 vCPU. A replay is bound by its ledger's fsyncs, not its CPU: with the ledger on the host's disk the reference week took about five hours a policy, and with `TMPDIR` on tmpfs and the two policies side by side about three hours, 3h05m for `fill` and 3h14m for `fair`.
