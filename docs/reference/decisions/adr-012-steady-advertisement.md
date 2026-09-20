# ADR-012: Every tier advertises what it could run, and capacity is bought when a runner starts

## Status

Accepted. Implemented in v0.11.1 (the advertisement and the purchase) and in the release that carries `server.admission_order` (the waiting order). It replaces the rule this repository had carried since its first commit, that capacity is escrowed before it is advertised.

## Context

Each tier is a GitHub runner scale set, and `X-ScaleSetMaxCapacity` on every long poll is the only thing billet can say to GitHub about how much work it wants. Measured against GitHub.com (2026-08-19, and again during the incident below): work arrives as `JobAssigned` with `runnerRequestId: 0`, with no `JobAvailable` and no `AcquireJobs`, so client-side acquisition is the GHES legacy path; there is no call that declines, releases or returns an assigned job; and a scale set advertising zero is told nothing at all, so a tier that is not advertising is a tier that cannot be given work. GitHub cancels and reassigns an assignment nobody runs up to three times, and cancels a job queued for 24 hours; whether an assignment survives past the third cycle is unmeasured (U4 in issue #140).

billet's original rule was that a listener escrows capacity in the ledger first and advertises exactly what the escrow returned, so what GitHub is told is always backed by a lease. It was never recorded in an ADR and no incident introduced it; the measurements usually cited beside it are about a headroom check made outside the ledger transaction, which is a different problem. Two designs were shipped on that rule and both failed, in opposite directions:

- **v0.10.0** held one idle reservation per tier for as long as the control plane ran, so every tier stayed discoverable. On a nine-tier reference deployment that idled 96 of 120 vCPU and 448 of 480 GiB (#116).
- **v0.11.0** passed a single reservation around the tiers in a sorted round robin to stop the waste (#118). Each idle tier was then advertising for roughly one long poll per rotation — about one poll in eighteen minutes with nine tiers — and on 2026-09-19 a job queued against an idle tier went unassigned for over four hours, and a probe job for five (#140). The deployment could finish work already assigned and could not be given new work at all. Rolling back was refused, because the release carried migrations.

The rule's real content was never "do not overcommit" — the allocator enforces that atomically when a lease is created — but "never let GitHub assign work billet cannot immediately back". The price of that guarantee is the one thing the protocol will not sell: capacity billet advertises is the only channel through which work is offered at all.

## Decision

### The advertisement is a steady ceiling, and nothing backs it

Every tier advertises, on every poll, the most instances of its shape the deployment could ever run at once: `max_concurrent` if set, and `server.max_vcpu` and `server.max_memory` divided by the tier's shape, capped by an explicit `maxCapacity`, and never below the work the tier already has (`Allocator.AdvertisedCeiling`, `Listener.steadyAdvertisement`). It is computed from configuration alone. It does not move with live headroom, which is the property that matters: a number that followed free capacity would fall to zero exactly when the fleet is busy, which is exactly when new work is queueing, and billet would never learn the work existed.

The advertisements of different tiers therefore sum to more than the fleet can run, deliberately. GitHub may assign more work than there is room for, and the surplus waits, assigned, until a lease can be bought for it.

### Capacity is bought when a runner starts

A lease is taken at three points and nowhere else: per offer in `handle` (the legacy acquisition path), per pool launch in `reconcilePool` (`backPoolSlot`, one lease at a time, launched before the next is bought), and per assignment this process holds no promise for (`backAssignment`). Each purchase is the allocator's ordinary atomic `Escrow`, so the deployment ceiling, each node's budget, per-node fit, `max_concurrent`, explicit `reserved` floors, macOS licence slots and the admission seal all still decide it, unchanged. Escrow bought for work that did not use it is released after that message is handled.

Nothing is reserved for an idle tier. An explicit `reserved` floor is the one exception, because an operator asked for it.

### The waiting order is a policy, and it gates purchases only

When several tiers want the room a finished job leaves, `server.admission_order` decides. `fair`, the default, gives it to the tier that has waited longest and holds it until that tier's shape fits; `fill` gives it to anything that fits. A tier's place is dated by its first refusal and released when its demand is served or when GitHub's count says the work is gone.

The order never lowers an advertisement. That distinction is the whole lesson of v0.11.0: admission control that works by withholding advertisement cannot be corrected later, because the work it would have scheduled is never assigned in the first place.

## Consequences

**A job can be assigned to a full fleet and wait.** That is the intended behaviour and it is also GitHub's own model — its reference controller advertises its configured `maxRunners` for the life of the listener and leaves pods `Pending` when the cluster is full. The waiting is bounded by limits billet does not control: GitHub's 24-hour queue limit, and the unmeasured retention of an assignment past its third reassignment. If U4 measures badly, the mitigation is to advertise less than the ceiling under sustained saturation, not to return to rotation.

**Under `fair`, freed room can sit idle while it accumulates.** Bounded by the longest job already running. It cannot deadlock: there is one winner at a time, chosen by a wait that waiting cannot extend. `fill` is the escape for a deployment that would rather have the throughput.

**A seal or a drain buys nothing**, so a job assigned in the poll that was in flight when a seal landed waits until admission reopens. This is a deliberate trade: the host-upgrade fence depends on the seal being absolute for new leases, and a drain that bought capacity could never converge.

**`Escrow` failures now arrive per launch rather than per poll.** A failing allocator still stops the listener, as it did before; it is simply reached more often.

**The status counters change meaning.** "Discovery" is normally zero, because there is no idle escrow to count; what an operator needs instead is what each tier is waiting for, which is what the status report grows.

## Alternatives considered

**Keep escrow-before-advertise and make the rotation faster.** The rotation's window is bounded below by the long-poll length, so a fleet of N tiers cannot give every tier a window more often than N polls apart; at nine tiers that is the design that failed. Making the window per-poll is the same as advertising everywhere, without the honesty.

**Advertise live headroom.** Zero when busy, which is when work queues; and it flickers, so GitHub's view of a tier depends on which poll it read.

**Per-job queueing inside billet.** The wire does not distinguish a re-assignment cancel from a real cancellation, so a local queue drifts into phantom demand or loses real demand. GitHub's `TotalAssignedJobs` is the count billet trusts.
