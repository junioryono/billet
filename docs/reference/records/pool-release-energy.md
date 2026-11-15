# The pool's release, priced in idle energy

What releasing a rented host once it has idled past `server.pool.idle_after` saves in idle energy, against a pool that never releases one and against a fleet that keeps the pool's peak on all week, and what it costs in queue wait. Replayed on 2026-11-14 on the shared Linux test host, through billet's real scheduler and the pool's real scaler (`internal/pool`), with `TestPoolReleaseEnergy` (`internal/replay`). Each idle host-hour is priced at the reference EPYC 7763's measured idle draw at the wall.

## The question and what would answer it

The monitoring work's power half promised to pack work onto fewer machines once there was more than one; billet's rented bare-metal pool (ADR-030) does that over time, ordering a host for a shortage and releasing one idle past `idle_after`. A host carrying nothing still draws its idle power, so the saving is the idle host-hours a release removes. The release is worth having if it removes most of the idle hours a pool that keeps its hosts pays for, at a queue-wait cost a deployment would accept: here, a p99 that stays within a lead time or two of the never-released pool's.

## How it was replayed

- **The workload:** a synthetic work week, `WorkWeek`: five working days of `MorningBurst`, 300 jobs a day with seventy percent in an initial burst lasting about an hour and the rest spread evenly over the next seven, runtimes lognormal around six minutes, on four shapes of 2, 4, 8 and 16 vCPU at 4 GiB a vCPU, the smallest the most common; nothing at the weekend. Seed 1, 1,500 jobs. The reference deployment's own week was not replayed: its trace is exported from the production ledger and none was kept from the earlier replays.
- **The fleet:** a pool alone, floor 0, maximum 4, of hosts the reference EPYC's shape (128 vCPU, 512 GiB), the scaler stepping every minute, at two lead times from an order to a joined host: 3 minutes (the replays' usual) and 15 minutes, a longer lead chosen to illustrate a slower source: an AWS metal host recorded from its order to its node's registration on 2026-11-10 took 22 min 25 s, and a representative lead time across hosts has not been measured (`bare-metal-hosts.md`).
- **The configurations:** a pool that never releases (no `idle_after`), and pools that release at `idle_after` 5 minutes (the least the configuration accepts), 20 minutes and 1 hour.
- **The baseline:** a counterfactual fleet without a pool that keeps the never-released pool's peak on for the whole week, carrying that pool's placements, so its busy hours are that pool's. A fleet replayed on its own could place the same jobs differently.
- **What is counted, over one window:** every configuration over the same 168 hours from Monday 09:00, which holds every join, every job and every release tail. A pool host is powered from its join to its release ask plus five minutes, an assumed power-off time taken from the clearance the plane requires (two empty answers across five minutes), which establishes nothing about when the provider powers the host off (on AWS metal each released host was read back as gone about 20 minutes after its termination was requested, `bare-metal-hosts.md`), or to the window's end if never asked; busy while it carried at least one lease, overlapping leases counted once; idle the rest. `PoolPowered` holds that arithmetic, and its tests check it against figures worked out by hand from literal times.
- **The price:** 149.9 W an idle host-hour, the reference host's draw at the wall over five quiet minutes on 2026-10-23 (`cpu-placement.md`, session 2), from the Shelly plug on its power cord.

## Results

**Lead time 3 minutes:**

| Configuration | Hosts joined | Most at once | Powered host-hours | Busy | Idle | Idle energy (kWh) | Idle energy saved against always on | Queue wait p50 | p99 |
|---|---|---|---|---|---|---|---|---|---|
| 2 hosts always on (counterfactual) | 2 | 2 | 336.0 | 41.7 | 294.3 | 44.1 | | | |
| never released | 2 | 2 | 335.7 | 41.7 | 294.0 | 44.1 | 0.0 kWh (0%) | 30 s | 1 min 27 s |
| released at `idle_after` 5 min | 32 | 2 | 47.4 | 40.4 | 6.9 | 1.0 | 43.1 kWh (98%) | 30 s | 4 min 4 s |
| released at `idle_after` 20 min | 10 | 2 | 51.1 | 41.3 | 9.8 | 1.5 | 42.6 kWh (97%) | 30 s | 3 min 20 s |
| released at `idle_after` 1 h | 10 | 2 | 57.8 | 41.3 | 16.4 | 2.5 | 41.7 kWh (94%) | 30 s | 3 min 20 s |

**Lead time 15 minutes:**

| Configuration | Hosts joined | Most at once | Powered host-hours | Busy | Idle | Idle energy (kWh) | Idle energy saved against always on | Queue wait p50 | p99 |
|---|---|---|---|---|---|---|---|---|---|
| 3 hosts always on (counterfactual) | 3 | 3 | 504.0 | 42.1 | 461.9 | 69.2 | | | |
| never released | 3 | 3 | 502.5 | 42.1 | 460.4 | 69.0 | 0.2 kWh (0%) | 30 s | 12 min 1 s |
| released at `idle_after` 5 min | 33 | 4 | 46.6 | 39.7 | 7.0 | 1.0 | 68.2 kWh (98%) | 1 min 5 s | 25 min 33 s |
| released at `idle_after` 20 min | 17 | 4 | 54.8 | 42.1 | 12.7 | 1.9 | 67.3 kWh (97%) | 33 s | 25 min 33 s |
| released at `idle_after` 1 h | 17 | 4 | 67.7 | 42.1 | 25.6 | 3.8 | 65.4 kWh (94%) | 33 s | 25 min 33 s |

Every configuration ran all 1,500 jobs.

## What it says

- **Releasing removed 94 to 98% of the idle host-hours a kept pool pays for.** A week of this work keeps two or three hosts busy for about 42 host-hours between them; kept on, they idle 294 to 460 more, about 44 to 69 kWh at the wall. Released at any of the three values, the idle hours fell to 7 to 26, 1 to 4 kWh. Most of a kept host's week is nights and the weekend, which is where the saving is.
- **A shorter `idle_after` saves a little more and orders far more.** At 5 minutes the pool released between a day's bursts and ordered again for the next: 32 and 33 hosts joined in the week, against 10 and 17 at 20 minutes and an hour. Each order is a lead time a job may wait for, and on a source that bills by the hour, a minimum charge; the energy difference between 5 and 20 minutes was under 1 kWh in either run.
- **The cost is in the tail, and it grows with the lead time.** At a 3-minute lead the release added about two to three minutes to the p99 (1 min 27 s kept, 3 min 20 s to 4 min 4 s released). At 15 minutes the p99 went from 12 minutes kept to 25 minutes 33 seconds released, about one more lead time, which is within the target above. That it is each morning's first burst waiting a lead time for a host a kept pool still had from the day before is the likely reading, and was not traced job by job. The median moved by at most about 35 seconds.
- **On this workload, 20 minutes is the value to start from:** within 1 kWh of the most saving, about a third to a half of the orders of 5 minutes, and the same p99 as an hour.

## What it does not say

- **The saving is in idle energy only.** Active energy, a host's boot and the provider's own overhead are not counted; busy host-hours were within 2.5 of each other across configurations, and their active energy was not measured. It is not the total electricity saved, and nothing here is a bill.
- **The baseline is a counterfactual.** No fleet without a pool was replayed; the always-on row keeps the never-released pool's peak and its placements.
- **The harness releases early in simulated time, by an amount not measured.** The plane times a release's clearance on the wall clock while a replay's ledger runs on simulated time, so a released host leaves the pool's room as soon as the plane's loop next answers, not five simulated minutes later. The accounting does not use that release: a host is powered to its ask plus five minutes. But room freed early changes what the pool decides after it, when it orders, which host a job lands on and when the next release is asked, so the release rows' orders, queue waits and placements, and through them their powered and idle hours, may differ from a real pool's by an amount this replay does not bound. Every figure here, the recommendation included, is conditional on that and on the assumed power-off time.
- **One synthetic week, one seed.** The reference deployment's week, a weekend with work, other seeds, and a floor above zero were not replayed. The pool's maximum of 4 was reached only at a 15-minute lead.
- **A fixed price for every idle hour.** The reference EPYC idled at 149.9 W in one five-minute reading; a rented host's idle draw is its own, and session 3 of the same record read 161.3 W after a run.

## Reproducing it

On the shared Linux test host, from a checkout of the commit that added this record, under the heavy lock:

```bash
flock /srv/billet/heavy.lock env BILLET_REPLAY_POOL_ENERGY_OUT=results/pool-energy-lead-3m.md BILLET_REPLAY_LEAD=3m \
  go test -count=1 -timeout 6h -run '^TestPoolReleaseEnergy$' ./internal/replay
```

`BILLET_REPLAY_JOBS_PER_DAY`, `BILLET_REPLAY_SEED` and `BILLET_REPLAY_POOL_MAX` change the workload and the pool. Each run took about 30 to 37 minutes.
