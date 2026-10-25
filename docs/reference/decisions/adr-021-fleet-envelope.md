# ADR-021: The fleet one control plane schedules

Accepted, 2026-10-25. Issue #528.

## Context

Every write a control plane makes takes the ledger's one writer slot: SQLite's write lock, or PostgreSQL's advisory lock under a one-connection writer pool ([ADR-008](adr-008-state-backends.md)). A purchase's headroom check and its insert are one transaction, and must stay one (moving the check outside once produced 28 grants against a ceiling of 4). So what one control plane can schedule is what fits in that slot.

The [write budget record](../records/ledger-write-budget.md) measured the slot on 2026-10-08 and found a purchase over 128 hosts holding it for about 30 ms. Nothing said what fleet billet is meant to schedule, so nothing said whether 128 hosts was far past any deployment or a modest one. This records the fleet billet commits to, the arithmetic that turns it into a budget for the slot, and what was measured against it.

## Decision

### The target

| | Target | Arithmetic |
|---|---|---|
| Hosts | 256 | |
| Open leases | 4,096 | sixteen a host |
| Purchases, sustained | 14 a second | 4,096 leases turned over every 300 s, a five-minute median job |
| Purchases, burst | 26 a second for 10 s | a 256-job matrix bought within ten seconds of arriving |
| A job's writes after its purchase | 6 transactions | assigned, bound, launching, online, busy, released (`BenchmarkLeaseLife`) |
| Renewals | 137 leases a second | every open lease once a third of its 90 s TTL, by its listener |

The writes a job makes besides these (its usage report, its cache observation, its queue time) are not counted, and each adds one transaction.

### What each may take of the slot

- **A purchase at the target holds the slot for at most 10 ms**, so the sustained purchases take at most 14% of it and a burst at most 26%.
- **Renewals take at most a quarter of the slot**, the threshold the write budget record already reopened its decision at.
- **The steady state, renewals, purchases and every job's writes together, takes at most half**, which leaves the slot free half the time for a burst, an operator's command and the reaper. A burst on top of it may take the rest for its ten seconds.

These are measured on the hardware the record names, with the commit as the storage under the ledger makes it. A slot hold is billet's own work plus one commit, and on network block storage the commit is most of it: an `fdatasync` on the test host's EBS gp3 volume took 3.6 ms at the median, and on an instance-store NVMe 0.14 ms.

### What the target decided

- **Placement grows linearly with the fleet, and is counted rather than placed.** Headroom is counted one preference rank at a time: a rank whose hosts all charge the same is counted in one pass, and only a rank whose hosts charge differently is placed job by job, over its own hosts. A tier falling back from its hosts to a remote shape took 68 ms of the slot per purchase over 256 hosts before this; the count is held to the greedy one it replaces by a randomized test.
- **One transaction reads the fleet once.** The placeable hosts are read once per question and every tier's candidates filtered from them, and the floors' counts come from the same pass as the usage when a floor needs them.
- **A listener renews its leases in one commit a pass, up to 256 to a transaction.** One transaction a lease was half the slot at the target on network block storage before anything was scheduled. The statement answers each lease exactly as its own renewal would, so nothing that reads a renewal's answer changed, and no wire change was needed: the listener renews in process.
- **A node's renewals stay one transaction a lease.** A node renews only what it is launching and what it holds in custody, a small fraction of the fleet, and batching them would need a route and a protocol version.
- **The commit is the operator's, as the database's keepalives are ([ADR-009](adr-009-controller-election.md)).** billet keeps SQLite's `synchronous=FULL` and refuses PostgreSQL's `synchronous_commit=off`, because a commit acknowledged before it is durable is a scheduling decision a crash can take back. Storage that commits faster is how a deployment buys headroom past what the record shows.

## What was measured

[The write budget record](../records/ledger-write-budget.md) has every number, the hardware and the method (2026-10-25). In short:
- **Before:** on network block storage the target took more than the whole slot on either engine (104% SQLite, 105% PostgreSQL), and renewals alone took half of it.
- **After:** 44% on SQLite, 50% on PostgreSQL, and renewals under 1%.
- **On NVMe:** 11% on SQLite and 23% on PostgreSQL.

Against the budget:
- **A purchase within 10 ms:** met everywhere; the slowest is 9.3 ms, with two floors outstanding on SQLite over gp3.
- **The steady state within half the slot:** met everywhere except PostgreSQL over gp3, at 50.4%, four fifths of it a job's own six commits. That is where this decision is next reopened: fewer commits in a job's life, or faster storage under the ledger.

## Reopen when

- `billet_ledger_write_held_seconds` shows the writer slot busy more than half the time in steady state;
- a deployment schedules more than 256 hosts or 4,096 open leases;
- a job gains a write, or the lease TTL is shortened.

## Alternatives rejected

**A fleet model the controller keeps in memory and the transaction only verifies.** The issue proposed it if placement could not be made linear. It was made linear without it, and a model kept beside the ledger is a second copy of the fleet's truth that every write path has to keep in step.

**An index for the usage aggregation.** A covering partial index on the lease's host removed the sort from the per-host usage query (1.25 ms to 0.48 ms on the target's ledger, SQLite's command line) and barely moved the floors' count; that is about a millisecond of a purchase, against a migration, its PostgreSQL twin and the index's upkeep on every lease write.

**Batching every renewal over the wire.** A node's renewals are few at the target, and a batch route costs a protocol version, a fencing answer per lease that a node must act on separately, and a batch window that delays every renewal behind it.

**A weaker commit.** SQLite's `synchronous=NORMAL` and PostgreSQL's `synchronous_commit=off` would take the commit out of every write, and with it the guarantee that a lease billet acted on survives the host it was written on.
