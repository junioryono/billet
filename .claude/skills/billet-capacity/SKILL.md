---
name: billet-capacity
description: "billet's capacity model — leases, escrow before advertising, the fencing epoch, lease renewal handovers, and custody of compute billet cannot account for. Use when touching internal/alloc, internal/server/listener.go, internal/nodeplane, or internal/node; when changing what a tier advertises to GitHub; when adding a lease phase or a placement rule; and when a bug looks like double-booked capacity, a job that never launched, or a container nobody destroyed."
---

# Capacity, leases, and custody

These are the rules that keep billet from promising a machine it does not have. Break one and the symptom is a job assigned to a host with no room, or capacity that is never handed back.

### Capacity is escrowed BEFORE a listener advertises

Each tier is its own GitHub scale set with its own `maxCapacity`. If listeners advertise independently, GitHub can fill all of them at once and the host is overcommitted with nothing to stop it. Reserve against the global ledger first, advertise second. Capacity is a **vector** — CPU, memory, macOS licence slots, disk — never one integer.

`internal/alloc` owns this, and three details are load-bearing:

- **The headroom check and the insert are ONE transaction.** Checking outside it is a read followed by a hopeful write. Measured: moving the check out produced **28 grants against a ceiling of 4** under concurrency. `TestConcurrentReservationsNeverOvercommit` is the guard.
- **A lease's `node` stays NULL until a node binds it, but `target_node` is set at ESCROW.** The two answer different questions: the target is where billet decided the work goes, the node is where it actually went. Capacity is charged on `COALESCE(node, target_node)`, because a reservation aimed at a machine has already spent it — counting only bound leases let a tier escrow against the same host repeatedly in the window before its first launch, which is the overcommit the escrow exists to prevent, moved down from the deployment to the machine.
- **Every reservation names a machine.** It used to be set only for a tier that pinned itself, which left an ordinary lease charged to the deployment and to no host — so the fleet's remaining room never shrank and billet advertised the same slots repeatedly. `Bind` was always a verifier and needed no change; it simply started always having a target to verify against.
- **What a tier advertises is `min(deployment ceiling, Σ over eligible machines)`.** The fleet term stops billet promising more than the machines can hold; the ceiling keeps a one-box install behaving as it did, since a host detecting more threads than its config allows would otherwise win.
- **The epoch is a fence, and a reclaim bumps it.** Without that, a holder declared dead and replaced keeps writing to a lease someone else now owns — an orderly takeover becoming two concurrent owners of one slot. Every write presents its epoch; a stale one is refused.

The state machine is written down in `validTransitions` rather than implied by scattered UPDATEs, and terminal phases have no successors: a lease that released its capacity must never move backwards and re-acquire it, which is what a double-admit looks like from the inside.

### What is advertised is TOTAL escrowed capacity, and it is renewed on its own clock

The listener holds the escrow in three states — `held` (free), `acquiring` (promised to a request billet has claimed from GitHub but has not yet been given, keyed by request id), and `running` (assigned). All three are escrowed and **all three are advertised**, because `maxCapacity` is the scale set's *total* capacity, not its spare. The vendor's own listener sends a configured maximum that does not move as jobs are assigned. Sending only the free half shrinks the advertisement on every assignment, so a tier with room for two tells GitHub "1" the moment the first job lands and the second slot goes unused. The invariant above is untouched: every lease in all three came from the allocator, so the sum across listeners is still bounded by the budget.

**`acquiring` exists because a promise is not a count.** Capping acquisitions at "how many free leases are there right now" is an instantaneous measure, and an acquisition is an obligation that lasts until the assignment arrives. Leaving the lease in `held` while the claim is in flight let one lease back two consecutive offers, let a lease promised to one request be consumed by the assignment of another, and let the heartbeat spend it out from under a claim already on the wire. Reserving it under the mutex *before* the network call fixes all three at once, which is the tell that they were one bug rather than three.

**Heartbeats must not be bounded by the poll.** A long poll was assumed to be about 50 seconds against a 90 second TTL, which reads like ample margin. **Measured against a real organization on the first poll billet ever made, it ran ~88 seconds** — two seconds inside the TTL — and the vendor's HTTP client permits far longer once slow responses and retries are counted. Renewal that happens only *between* polls stops for as long as one poll lasts. The reaper then terminalises the leases, another tier escrows the capacity, and the poll returns an assignment backed by a lease this listener no longer holds. Tying renewal to the poll makes the safety of the whole escrow depend on a timeout billet does not control.

**Derive the cadence from the allocator's actual TTL, never from `DefaultLeaseTTL`.** A cadence computed from the default is correct only for a default-configured allocator; under a shorter TTL every lease expires between beats and advertised capacity climbs — measured at six times the budget before a test caught it. Nothing in the type system will catch this.

That makes heartbeat the only writer concurrent with the poll loop, so the escrow takes a mutex, and `assign` holds it **across** the allocator write. Releasing it around the write to keep heartbeats snappy looks obviously right and is not: it opens a window where the write succeeds but a concurrent heartbeat has already dropped the lease, leaving the assignment durable in the ledger and tracked nowhere in memory. A transient heartbeat error **keeps** the lease — only `ErrFenced` and `ErrLeaseNotFound` mean it is genuinely someone else's. Dropping on a busy database removes the lease from the release path too, so the ledger keeps counting it until the reaper expires it.

The message lifecycle closes: `Available` → acquire, `Assigned` → consume escrow (idempotent by request id, because an unacknowledged message is redelivered), `Completed` → release. Acknowledging `Completed` without releasing leaves the lease open until the reaper expires it, withholding capacity and recording the wrong conclusion against it.

### A commitment made to a remote service cannot be revoked by a local timer

`acquiring` is the one escrow state with no way back on its own: every exit needs GitHub to say something. That reads exactly like a leak — a lease renewed forever because a message went missing — and the obvious fix is to age it out. **That fix was written, reviewed, and reverted, and the reason generalises well beyond this listener.**

`AcquireJobs` is one-way. There is no decline or release endpoint on the session client, and `DeleteMessage` acknowledges a *notification* rather than refusing a job. So releasing the escrow on a timer hands nothing back to GitHub. It only means billet has forgotten it owes a runner while GitHub still expects one — the freed slot goes to another tier, and the assignment, when it arrives, has nothing behind it. A fix for a capacity leak had created a way to drop live work.

The reasoning that produced it was sound and the premise was not. The question asked was *can this state end on its own?* The question that mattered was **are we entitled to end it unilaterally?** When a local record mirrors a commitment held by someone else, only they can release it; the local timer can report, and that is all. So a stale promise is reported and kept — it is capacity billet genuinely still owes — and the real remedy is to invalidate the session so GitHub itself redelivers.

**The second attempt is the instructive one, because it looked like it had escaped this.** Instead of a timer it released a promise when GitHub's own statistics reported the scale set had no job outstanding of any kind — apparently authorised by the party holding the commitment, with a staleness check only to avoid racing an in-flight acquisition. It was the same bug. The staleness check bounds how old *billet's promise* is; it says nothing about how old the *statistics* are, and the response carries no observation time, generation, or request identity to pin them to. Elapsed local time was still doing the authorising, now paired with a snapshot of unknown freshness — plus a quiet assumption that those counters are exhaustive, atomically maintained, and scoped as expected.

So: **a freshness check on your own record is not a causal fence on someone else's snapshot.** To act on remote state you need something that provably postdates your own action — a version, an observation time, a per-request status. Absent that, more samples and longer thresholds are the same guess with more decimal places. When the API does not offer one, the honest answer is to leave the gap open, report it, and go and measure the real system.

The same distinction decides how loudly to fail. An assignment with no escrow behind it **declines and carries on**, because that is reachable by ordinary races and stopping the control plane strands every tier's capacity over one job. A scale-set response that is not a subset of its request **stops the listener**, because no race can produce it: it means the API broke, billet can no longer tell which remote commitments are real, and stopping is itself the remedy since a fresh session makes GitHub redeliver. Match the blast radius to whether the condition is a race or a broken contract.

### Compute is unaccounted for until proven otherwise, and that is a state with a name

`internal/node/custody.go`. A **custody** entry is a lease whose compute billet cannot account for and which nothing else in the process is managing. Two things produce one and they are the same situation from different sides:

- **Adopted** — a container survived a restart. The runner inside is talking to GitHub on its own and may well finish the job.
- **Discarded** — a launch failed ambiguously and its cleanup was not confirmed.

Both heartbeat the lease so the reaper leaves it alone; both release only when the compute is confirmed gone. The rules that were each learned by getting them wrong:

- **A negative observation is not a causal result.** A `docker ps` issued right after a lost `docker run` can overtake the daemon and see nothing. A successful `Destroy` proves the compute is gone; an *absence* does not, and has to persist through `strayGrace` before it is believed.
- **Adoption renews the lease at the moment it adopts.** Billet may have been down longer than a lease TTL, and the control plane reaps BEFORE its first tend — so leaving the renewal to the tick let the reaper terminalize the lease that had just been adopted.
- **"Could not verify" is not "safe to destroy."** Only `ErrLeaseNotFound` proves a lease is gone. Any other read error aborts recovery, having destroyed nothing.
- **Serializing a mutation is not serializing a transition.** Holding the lock for the flag write and releasing it before the backend calls is the same race, one line down.
- **A cleanup obligation is owed to the COMPUTE, not to the lease.** Only a successful destroy discharges a pending completion retry. Losing the lease — fenced, reaped — changes who owns the capacity; it does not make the container stop running, and GitHub will not redeliver the completion that would ask again. If no lease remains, skip the release and keep destroying.

  Written down because the opposite was committed for one round and the argument for it was seductive: the capacity really is someone else's, so the record looks like litter. Two separate rules were being conflated. No record is CREATED for a request this listener never held — a restart loses the map while leases live on, and those retries could accomplish nothing. That says nothing about a record created when the listener DID hold the lease.
- **"Could not X" usually collapses two different facts, and only one is evidence.** A heartbeat that returns ErrFenced is the allocator SAYING the lease is not ours; a heartbeat that times out is the database saying nothing at all. Returning one boolean for both made a listener that briefly lost its ledger forget the containers it had launched. The same shape recurs everywhere in this codebase — a failed destroy is not proof the container survived, an absent `docker ps` row is not proof it is gone. When a call can fail for reasons that mean opposite things, the return type has to be able to say so.
- **A claim and an obligation expire differently.** Losing a lease ends this listener's claim on the CAPACITY. It does not end its obligation to destroy the container it started, and the two have been conflated three separate times: once for pending cleanup records, once for running leases dropped by the heartbeat, once at shutdown. Whenever a record is removed because "it is not ours any more", ask what was launched under it.
- **A bound shorter than the work it bounds causes the failure it prevents.** The shutdown grace was 90 seconds against a node command timeout of TEN MINUTES, so an ordinary slow destroy tripped the watchdog, stopped renewal, and let the reaper reclaim capacity whose container was still being destroyed — precisely what the grace existed to avoid. Before choosing a timeout, find the longest legitimate operation underneath it and make the bound larger, or make the work smaller.
- **Concurrency against a serial queue can be worse than sequence.** A node runs commands one at a time and each command's timeout starts when it is QUEUED, so firing twenty destroys at once starts twenty ten-minute clocks against a queue that serves them in turn: the later ones expire while the node is working happily through the earlier ones, and healthy jobs are recorded as failures. Fan-out needs a bound chosen against the SERVER's concurrency, not the client's patience.
- **An OPTIONAL capability cannot carry a safety invariant.** The reversed change above was defended with "both shipped runners implement `Sweeper`, which destroys compute no lease is holding." True, and irrelevant: `Sweeper` is a type assertion on `Runner`, so that reasoning makes correctness depend on which implementation is plugged in, and billet is meant to be extended by strangers. If safety rests on a capability, the interface must require it — otherwise assume the implementation without it, and let the capability be a backstop rather than the mechanism.
- **Time warns; it does not authorise a teardown.** Held compute has NO bound by default (`DefaultMaxCustody = 0`) and warns hourly. Elapsed time is not evidence that a job stopped making progress — billet imposes no job limit and self-hosted runners run past GitHub's six-hour default — so killing live work must be authorised by a completion, an observed exit, or an operator. An operator who knows their longest job can set one with `node.WithMaxCustody`; a job killed by it is archived as FAILED, not done.

### GitHub does not requeue a job whose runner vanished mid-execution

Checked against the vendored `scaleset` README, after I asserted the opposite in a commit message and had to retract it. Reassignment is documented for a job *"assigned to your scale set but not acquired by a runner in time"* — GitHub cancels and requeues that, up to 3 times. It says **nothing** about a job a runner has already started.

So force-killing a container that is running a job is a **deliberate job failure**, not a recovery, and "a graceful shutdown does it too" is not a defence: a graceful shutdown is a choice the operator made. This is why restart recovery adopts.

### An instance is named for its lease, and the name is the only durable link

`provider.InstanceName` / `provider.LeaseOf`. Nothing writes "this container belongs to that lease" anywhere, so after a crash the name is all reconciliation has. Two consequences:

- The instance carries **billet's** name, never GitHub's runner name. They are different identities and conflating them made every orphan unattributable.
- Docker's `--filter name=X` is a **SUBSTRING** match — measured, not assumed: `billet-abc` really does return `billet-abcdef`. `Find` compares exactly afterwards.

### A lease must be renewed by exactly one party, and there are three handovers

Custody exists because a remote launch has three outcomes and one of them is UNKNOWN. Every defect in this area has been a moment where the count of parties renewing a lease was zero — never two, which is harmless, because `Heartbeat` is idempotent and a released lease answers `ErrLeaseNotFound`.

The three moments, each of which cost a review round to find:

- **While the provider is still working.** The plane gives up after the command timeout and tells the listener the node has custody, so the listener stops. The node is inside `provider.Launch` and adopts nothing until it returns. The node now renews from the moment it commits to launching (`r.launching`), which is the first instant either side can be sure something may exist.
- **When the report arrives too late.** The plane records a TOMBSTONE for every launch it abandons — on timeout and on re-registration — so a late success is answered with "this lease is yours" instead of 204. Without the tombstone the node files the instance in its ordinary running set, which nothing renews.
- **When the result races the timeout.** Both select branches are live at once. `settle` drains the result channel while holding the plane mutex, which is what makes the answer exact: `Result` sends under the same lock, so the send has either completed or not started.
