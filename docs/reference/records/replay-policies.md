# Admission orders on replay

How `fair`, `fill` and `hold` divide room between shapes that share hosts (#526, #527), replayed through billet's real listener, allocator, placer and node over the simulated backend at compressed time: the synthetic mix on 2026-10-25, the reference deployment's week on 2026-11-04. Two workloads: a synthetic mix with a whole-host shape beside smaller ones, and the reference deployment's own week, exported from its ledger with `billet jobs export`. Both ran on the shared Linux test host, one replay per policy: the synthetic mix under the host's heavy lock, and the reference week beside other work at low priority, which changes how long a replay takes and not what it measures, because a replay's clock is virtual. The last section says how to repeat them. The last section measures #527's `hold` order, and the `tightest` placement, against them.

## How it was replayed, and what that leaves out

Both comparisons ran under GitHub.com's direct assignment, where each tier's assigned-but-unstarted jobs are the waiters the admission order ranks. Under GHES's offers no tier is ever recorded as waiting, so `fair` and `fill` give the same result: the first wide run, under offers, gave identical tables for both, which is how #686 was found. Until #686 is fixed, `fair` on an offers deployment is `fill`.

What the harness models and what it does not:

- **The guest is simulated.** A runner boots in 30 seconds and its job runs for exactly the duration the trace gives, then the lease is archived. A guest's real boot, pull and teardown are not modelled.
- **The waiters are ordered by event.** The admission queue is on wall time, and the replay compresses time, so waiter order follows the order events are delivered and the stall bypass (`WaiterAllowance`) is never exercised: a listener acts only on an event.
- **A macOS tier is a simulated guest on its own host.** Its shape and its host are kept, and a tier lands only where its backend runs, because every host of a replay is simulated and nothing else would keep a Linux tier off a Mac. What a macOS host limits beyond room, its two VM slots, is modelled only so far as a tier's pin and shape do.
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

The fleet is the deployment's as it registered: one Linux host of 180 vCPU and 480 GiB (Firecracker) and one macOS host of 16 vCPU (Tart), with a ceiling of 196 vCPU and 540 GiB. It has 21 tiers on four targets, from 2 to 64 vCPU. Five run on the macOS host, four of 6 vCPU and one of 4 vCPU and 12 GiB, and the rest on the Linux host. The export predates the fleet document's record of each tier's backends, so they were added from the deployment's configuration, read by shape. The week ran jobs of five shapes, from 2 to 16 vCPU; no job of 32 vCPU or more ran in it. Every job started and finished under both policies, and the ledger proved no overcommit.

| Shape | Policy | Started | p50 | p90 | p99 | p99.99 | Max |
|---|---|---|---|---|---|---|---|
| 2 vCPU | fair | 11,860 | 1m23s | 17m21s | 1h12m49s | 2h21m12s | 2h21m23s |
| 2 vCPU | fill | 11,860 | 31s | 9m13s | 45m40s | 1h07m54s | 1h07m54s |
| 4 vCPU | fair | 2,978 | 30s | 13m02s | 35m12s | 1h03m42s | 1h03m42s |
| 4 vCPU | fill | 2,978 | 30s | 9m23s | 51m03s | 1h10m04s | 1h10m04s |
| 6 vCPU (macOS) | fair | 68 | 30s | 30s | 30s | 30s | 30s |
| 6 vCPU (macOS) | fill | 68 | 30s | 30s | 30s | 30s | 30s |
| 8 vCPU | fair | 4,270 | 12m25s | 1h12m50s | 2h29m38s | 2h43m02s | 2h43m02s |
| 8 vCPU | fill | 4,270 | 9m03s | 30m33s | 55m59s | 1h29m56s | 1h29m56s |
| 16 vCPU | fair | 581 | 1m25s | 5m17s | 10m58s | 20m56s | 20m56s |
| 16 vCPU | fill | 581 | 3m00s | 59m04s | 3h58m59s | 5h39m20s | 5h39m20s |

| Policy | Started | Jobs refused at least once | vCPU charged | Memory charged | Empty host-hours | Over (hours) |
|---|---|---|---|---|---|---|
| fair | 19,757 | 11,880 | 37% | 54% | 171.6 | 166.2 |
| fill | 19,757 | 10,933 | 37% | 54% | 172.3 | 166.2 |

What it says:

- **`fill` starves the widest shape here too.** The deployment's widest jobs that week were 16 vCPU, and under `fill` their p99 is 3h59m and their longest wait 5h39m, against 11 minutes and 21 minutes under `fair`. The 4 vCPU p99 is also better under `fair`, 35 minutes against 51.
- **Under `fair`, the 8 vCPU tier and the small shapes pay.** The 8 vCPU p99 is 2h30m against 56 minutes under `fill`, and its p90 1h13m against 31 minutes, on the most numerous shape above 4 vCPU, 4,270 jobs. The 2 vCPU p99 is 1h13m against 46 minutes, and its longest wait 2h21m against 1h08m. The replay does not say why the 8 vCPU tier is the one that waits; #527's replay, which names the holds it makes, is where to look.
- **The macOS jobs do not wait under either.** Five tiers share a host of their own with few jobs among them, so no 6 vCPU job waited past its boot. That host can account for at most 166 of the 172 empty host-hours, the length of the span; this table does not divide them by host.
- **The week's arrivals set the span, and memory binds before vCPU.** Both policies drained within the week, so both are measured over the same 166 hours and charged the same shares. Memory charged is 54% against 37% of vCPU, because the Linux tiers charge 4 GiB per vCPU against a host of 480 GiB and 180 vCPU, which runs out of memory near 120 vCPU, as [Queue wait on a shared node](shared-node-queue-wait.md) found.
- **The deployment ran `fair` that week**, its configuration's default, and nothing here argues for `fill` instead: `fill` buys the 8 vCPU tier a shorter tail at the price of the 16 vCPU tier's, which is the trade #527's `hold` order is meant to remove.

What it does not say:

- The replayed waits are not checked against the waits the deployment measured that week (`billet jobs wait`). The harness's boot, its event-ordered waiters and its absence of real teardown make them a comparison between policies, not a prediction of either.
- The empty host-hours include a host idle while the last jobs finish and the macOS host between its few jobs; the quantity #527 is judged on, host-hours empty while a job of a shape it fits was queued, is #527's to measure.

## Holding one host: `hold`, and `tightest` (#527)

`hold` gives a waiter whose shape fits nowhere the host its running work is predicted to free soonest, keeps the room its shape needs there from every tier behind it, and leaves every other host open (ADR-026, and `billet-capacity`'s admission reference for the rule in full). These were replayed on 2026-11-04 and 2026-11-05 on the same two workloads, the hold at the rule this PR ships.

### The whole-host mix, over four seeds

One seed's p99 of a hundred whole-host jobs is mostly noise, so the mix was replayed with seeds 1 to 4 (`BILLET_REPLAY_SEED`), each 2,000 jobs on the four 32 vCPU hosts. The p99 queue wait for each seed in turn:

| Shape | fair | fill | hold | hold, tightest |
|---|---|---|---|---|
| 2 vCPU | 1h12m, 51m, 44m, 44m | 8m, 7m, 27m, 14m | 18m, 17m, 10m, 14m | 26m, 13m, 17m, 17m |
| 4 vCPU | 1h15m, 2h06m, 1h58m, 55m | 9m, 15m, 34m, 20m | 1h02m, 26m, 21m, 32m | 40m, 23m, 38m, 36m |
| 8 vCPU | 5h12m, 6h37m, 2h35m, 2h26m | 19m, 17m, 54m, 33m | 1h38m, 59m, 1h09m, 40m | 2h22m, 1h12m, 35m, 41m |
| 16 vCPU | 2h57m, 3h16m, 5h26m, 2h16m | 25m, 29m, 1h12m, 21m | 1h38m, 50m, 2h47m, 1h03m | 1h14m, 1h06m, 3h00m, 56m |
| 32 vCPU | 29m, 49m, 36m, 35m | 1h46m, 1h50m, 3h27m, 2h02m | 57m, 1h03m, 1h00m, 49m | 1h11m, 1h01m, 1h00m, 1h14m |

| Policy | vCPU charged | Empty host-hours | Empty while a job that fits waited | Over (hours) |
|---|---|---|---|---|
| fair | 79 to 83% | 0.5 to 1.8 | 0.1 to 0.7 | 20.4 to 21.3 |
| fill | 83 to 91% | 1.0 to 1.5 | 0.4 to 0.7 | 18.9 to 19.7 |
| hold | 83 to 89% | 0.5 to 1.6 | 0.1 to 0.7 | 19.4 to 20.2 |
| hold, tightest | 83 to 89% | 0.5 to 1.5 | 0.1 to 0.6 | 19.4 to 20.1 |

**Empty while a job that fits waited** is the host-hours a host held nothing while a job that could run there was queued, the waste #527 names, counted for a job the ledger never recorded too and for a tier pinned elsewhere not at all.

What it says:

- **In every seed `hold` beats `fair` on every shape below the widest, and `fill` on the widest.** The 8 vCPU p99 falls from 2h26m to 6h37m under `fair` to 40 minutes to 1h38m, and the whole-host p99 from 1h46m to 3h27m under `fill` to 49 minutes to 1h03m. It charges what `fill` does, and the same jobs take about as long.
- **It gives the widest shape a longer wait than `fair`,** about an hour against half an hour, and the smaller shapes still wait longer than under `fill`. That is the trade, and it is the operator's to choose.
- **`tightest` shows no reliable gain over `pack`,** under `hold` or under `fill`: the seeds disagree on which is better for every shape, so it is offered as a placement and not made the default.

### The reference deployment's week

The same week as above, on the same patched fleet:

| Shape | fair | fill | hold |
|---|---|---|---|
| 2 vCPU | 1h12m49s | 45m40s | 11m39s |
| 4 vCPU | 35m12s | 51m03s | 13m04s |
| 6 vCPU (macOS) | 30s | 30s | 30s |
| 8 vCPU | 2h29m38s | 55m59s | 2h02m38s |
| 16 vCPU | 10m58s | 3h58m59s | 1h43m06s |

Under `hold` 10,797 jobs were refused at least once, against 11,880 under `fair` and 10,933 under `fill`; charged shares and span are those of the other two (37% of vCPU, 54% of memory, 166 hours), and no host was empty while a job that fit it waited.

What it says:

- **On this deployment `hold` trades the widest shape's tail for the small shapes'.** The 2 and 4 vCPU p99 fall to about 12 minutes, under either other order's, and the 8 vCPU p99 sits between them. The 16 vCPU p99 is 1h43m, ten times `fair`'s and less than half `fill`'s.
- **One Linux host is the case `hold` helps least.** A waiter's host is the whole of the room every Linux tier shares, so keeping a 16 vCPU job's room there is close to what `fair` does, without `fair`'s line. `hold-tightest` gave the same table to the second, because with one host placement has nothing to choose. The deployment's configuration keeps `fair`, and `hold` stays opt-in, as was decided before this was measured.
- **The first measurement of this week under `hold` gave the 16 vCPU tier a p99 of 4h09m,** as under `fill`. A waiter behind a lease as large, on a host wide enough for several, holds back only buyers as wide, and the room each such lease left went to narrower work before the waiter read it. A purchase now leaves such a waiter the room it fits in (`Purchase.Keep`), which gave the table above.

### What else was measured, and not shipped

- **Simpler rules for whom a waiter holds back,** each replayed on the whole-host mix with seed 1 and each found wanting. Letting every waiter out of the line starved a shape as large as what the hosts run. Keeping every waiter with no host in it cost the small shapes more than `fair` (2 vCPU p99 3h40m against 1h12m). Moving a hold to whichever host was predicted ready soonest threw away the room it had accumulated and cost the widest shape its turn. Holding the line once a job runs late was measured at two, three, six and twelve times its kind's median: two and three cost the 8 and 16 vCPU tiers, six and twelve gave the same table, so the factor is six (`alloc.HoldLateFactor`).
- **`align`,** placing a job beside others predicted to end with it so a host empties at once, measured on 2026-11-03 against an earlier `hold`: the whole-host p99 went from 29m to 43m and the 4 vCPU p99 from 1h19m to 1h32m, and under `fill` it changed nothing that mattered (1h46m against 1h44m). It did not ship.

What it does not say:

- **A tier spanning a local host and a remote backend** can buy remote while its held host empties, which spends money the hold was meant to save; neither workload has one.
- **Under GHES's offers no tier is recorded waiting** (#686), so `hold`, like `fair`, does nothing there.
- **The replay polls every tier at one virtual instant,** so it cannot show room waiting on a listener that has not woken, and its queue on wall time never reaches the stall or the late rule's timing against a real clock.

## Reproducing it

Each comparison is `TestComparePolicies` in `internal/replay`, run by hand: `BILLET_REPLAY_COMPARE=wide` for the synthetic mix (`BILLET_REPLAY_JOBS` sizes it), or `BILLET_REPLAY_COMPARE=file` with `BILLET_REPLAY_TRACE` and `BILLET_REPLAY_FLEET` naming what `billet jobs export` wrote. `BILLET_REPLAY_PROTOCOL=offer` replays under GHES's offers instead, and `BILLET_REPLAY_POLICY` replays one policy alone (`fair`, `fill`, `hold`, or any of them with `-tightest`), `BILLET_REPLAY_SEED` seeds the synthetic mix, so the policies of a long comparison can run as processes side by side. With `-replay.out DIR` the table is written to `DIR/compare-<mode>[-<policy>].md`. The wide comparison took 21 minutes on the shared host's 16 vCPU. A replay is bound by its ledger's fsyncs, not its CPU: with the ledger on the host's disk the reference week took about five hours a policy, and with `TMPDIR` on tmpfs and the two policies side by side about three, or nearly five (4h46m for `fill`, 5h09m for `fair`) with other replays beside them at low priority.
