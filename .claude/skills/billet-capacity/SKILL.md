---
name: billet-capacity
description: "Load when touching internal/alloc, internal/server/listener*.go, internal/nodeplane, internal/node or internal/nodeclient/loop.go; when changing what a tier advertises; when adding a lease phase, a placement rule or a teardown path; and when a bug looks like double-booked capacity, a job that never launched, capacity that never came back, or a container nobody destroyed."
---

# Capacity, leases and custody

## What this area is

`internal/alloc` is the allocator: the capacity ledger, the lease state machine (`validTransitions`), placement, floors, the compute barrier, quarantine, force operations, enrollment and revocation records. `internal/server` runs one listener per tier against GitHub's scale-set API and consumes escrow; `internal/nodeplane` dispatches commands to hosts over the wire and `internal/node` executes them, holding custody of what it cannot account for. The one sentence everything below serves: billet must never promise a machine it does not have, and capacity is handed back only on proof that the compute is gone.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Buying capacity: [references/admission.md](references/admission.md)

- **Capacity is bought when a runner starts, and it is a vector.**
- **Nothing is reserved for an idle tier, and there is no rotation (#140).**
- **A target's share is a narrower deployment ceiling (#192).**
- **Status separates ownership from phase.**
- **A commitment made to a remote service cannot be revoked by a local timer.**
- **A pool member GitHub cannot route work to is retired on GitHub's per-registration word, never on the aggregate.**

### Leases: [references/leases.md](references/leases.md)

- **The lease state machine is written down and terminal phases have no successors.**
- **A remote lease is charged for the shape, not the tier request.**
- **The epoch is a fence and a reclaim bumps it.**
- **A heartbeat pass's budget starts after it holds the mutex.**
- **Exactly one party renews a lease, and every defect here was a moment when that count was zero.**
- **An inventory that shrinks is capacity that gets resold.**
- **billet attributes a lost job; it never re-runs one.**
- **A reserved fleet's claim is released by a proof.**

### Custody, quarantine, drains and sessions: [references/custody-and-drains.md](references/custody-and-drains.md)

- **Compute billet cannot account for is a named state, never resold.**
- **`Recover` runs at every registration, not only at start, so what it finds includes this process's own jobs (#227).**
- **A drain waits for compute, not for entries.**
- **Quarantine holds capacity for compute nobody has accounted for.**
- **Time warns; it does not authorise a teardown.**
- **A listener's drain does not wait for an anonymous pool slot that never started a job (#220).**
- **A drain is two barriers, and the second asks the machines.**
- **An unsealed server stop is a handoff, not a drain (#365, #368).**
- **A node hands over on a stop only when its config says so (#374).**
- **A clean exit withdraws; silence still means nothing.**
- **A restart waits for its own message session, and so does a failover.**
- **A tier whose session fails keeps everything it holds while it reopens (#207).**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**Adding a lease phase or a transition.** Change `validTransitions` and its table tests together; a terminal phase has no successor, and `requiresPlacement` decides which phases need a bound placement.

**Adding a teardown path.** Only proof the compute is gone releases capacity; a timer never authorises a teardown, and `TestOnlyTheForceOperationDestroysRunningWork` holds who may destroy running work.

**Changing a purchase.** Every purchase goes through `Allocator.Escrow`, whose headroom check and insert are one transaction, and asks the admission queue (`mayBuy`); `TestConcurrentReservationsNeverOvercommit` is the guard.

## Where the tests are

- `internal/alloc`: `TestConcurrentReservationsNeverOvercommit`, the barrier tests in `barrier_test.go`, the transition table tests, `TestTheForceDestroyStatementIsWhatTheGoSliceSays`, `TestBothDisruptionStatementsPinTheirWholeStatement`.
- `internal/server`: `heartbeat_budget_test.go`, `sessionrecovery_test.go`, `drain_nondestructive_test.go`, `forcecallers_test.go`, `sessionconflict_test.go`, `stranded_test.go`.
- `internal/e2e`: `computebarrier_test.go`, `holdergone_test.go` (and the live variant), `restart_test.go`, `multiday_test.go`, `wire_test.go`.
- `internal/integration/sessionreplacement_test.go`: `TestLiveSessionReplacement` records the 409 rather than asserting it.

## Related skills

`billet-node-wire` (the transport the commands ride), `billet-lifecycle` (what `local down` does with the barrier), `billet-providers-local` and `billet-providers-aws` (what `List` and `Destroy` prove), `billet-state` (the fence inside every write).
