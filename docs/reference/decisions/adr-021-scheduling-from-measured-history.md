# ADR-021: Scheduling learns from the deployment's own history, and a prediction decides nothing

Accepted, 2026-10-25. Issue #524, the first of four (#524, #526, #530, #527) that schedule by measured queue wait. Later sections extend this record as those land.

## Context

The ledger has kept every job's identity and times since migration 56 (repository, workflow, display name, tier) and when its lease was bought, assigned, started and archived. #525 made the queue wait a number a deployment watches. Nothing read the history back. So the scheduler could not tell a host whose jobs end in a minute from one running a job that takes an hour, and that is what holding one host for a shape that fits nowhere needs (#527): the host to hold is the one predicted to free enough room soonest.

Predicting a job's runtime from the history of the same job is well studied in batch scheduling, where predictions feed backfilling schedulers. Smith, Foster and Taylor predicted runtimes from historical runs grouped by similar attributes (JSSPP 1998, "Predicting Application Run Times Using Historical Information"). Tsafrir, Etsion and Feitelson showed that predictions the system makes from a user's recent jobs serve backfilling better than the users' own estimates (IEEE TPDS 18(6), 2007, "Backfilling Using System-Generated Predictions Rather than User Runtime Estimates"). In that literature a prediction tunes the schedule, and the scheduler's correctness never rests on it. billet adopts that rule as an invariant.

## Decision

**A job's runtime is learned under a key and predicted from the most specific level of the key that has seen enough jobs.**

- **The key is the tier, the repository, the workflow file and the job's display name.** The workflow is the workflow reference with its `@ref` cut, so one workflow run from every branch is one key. The target is not in the key because the tier names it (a tier belongs to one target) and the repository's name carries its owner. A job moved to a larger tier is learned afresh there, since its runtime is likely to change with its shape.
- **The levels, most specific first, are the job, its name across its matrix, its tier, and a default.** A matrix job's display name carries its matrix values (`build (ubuntu, 1.22)`), so the second level is the name with its trailing parenthesised suffix removed, matched from the end. A level answers once it has seen 3 jobs (`alloc.MinRuntimeSamples`). With nothing learned the prediction is 10 minutes (`alloc.DefaultRuntime`) from no samples. Each of these is a policy choice, not a measurement. How well the choice predicts is measured in `docs/reference/records/runtime-predictions.md`.
- **A level keeps the count, the median and the 90th percentile** (nearest rank) of the jobs that started in the 14 days before the snapshot (`alloc.RuntimeWindow`) and had finished by it. A job still running at the snapshot teaches nothing, and a runtime that is not positive is not a runtime.
- **The runtime is from the job's start to its lease's archive**, both on billet's clock. The archive is when the lease's room comes back, which is what a host freeing up means, so the teardown after the job is part of it.
- **The controller's copy is rebuilt when it is an hour old on the allocator's clock** (`alloc.RuntimeRefresh`), when asked rather than by a loop. A rebuild is one read through the reader pool (`DB.View`), never the writer slot. One rebuild runs at a time, and once a snapshot exists nobody waits for the next one. A rebuild that cannot read the ledger keeps the last snapshot and says so (`alloc.ErrRuntimesStale`). On the allocator's clock, a replay or a test moves the refresh by moving time.
- **A running lease is predicted to end at its start plus its median, and an overdue one at half its median from now.** A job that has outrun what its kind usually takes is the one most likely to run long, so a host running it must never look like the host about to free up. A lease whose job has not started is predicted as if it started now, and a lease with no job recorded yet from its tier.

**A prediction is an optimisation and never a dependency.** A missing or wrong prediction changes which host a policy prefers. It never decides whether capacity is bought, held or released: that stays with the escrow transaction, the floors, the ceiling and the proofs that compute is gone ("a timer never authorises a teardown"). A wrong prediction picks a worse host, never a wrong outcome.

**Its error is visible per job and per tier.** `billet jobs show` prints a job's runtime beside what a controller refreshing hourly would have predicted when it started: the snapshot as of the hour the job started in. `billet jobs runtimes` grades every job in a window the same way, per tier, reporting the median absolute error, the share within 2x of the prediction and the level each came from. Neither grades a job against itself. Both read the ledger as a report, which creates, locks, claims and migrates nothing.

## Consequences

- No migration: everything is read from columns the ledger already keeps. A row from before migration 56 has no repository, workflow or name, and is learned and predicted under its tier alone.
- The first reader is #527's choice of the host to hold. Until then the controller builds no snapshot, because nothing asks.
- A tier whose jobs vary widely (a monorepo's matrix behind one label) predicts poorly at the tier level, so the job and name levels are where a prediction is useful. On the reference deployment's fortnight to 2026-10-25, 99.4% of 30,253 jobs were predicted from the job's own runs and 78% ran within 2x of the prediction, the 8 and 16 vCPU tiers 84% to 96% (`docs/reference/records/runtime-predictions.md`).
