# ADR-009: Controller election, standby controllers, and shared deployment identity

**Status:** accepted, partially implemented (the promotion phase)

## Context

ADR-008 put the ledger in PostgreSQL and made the controller replaceable. It deliberately stopped short of failover, and said so in the code and in the operator documentation in the same words: *"what still has no election behind it is the promotion. Nothing here decides that a leader is dead or that a follower should take over."*

What exists after phase 3a is the pair that makes a handover **safe** without making it **happen**. A session-scoped `pg_try_advisory_lock` stops a second controller starting. A monotonic epoch on `controller_claim`, re-read inside every write transaction, refuses a controller that lost its exclusion without noticing — and closes a signal that stops the process, because refusing a write is not stopping a process.

What is missing is a process that **waits** for the exclusion instead of exiting, the rules about what such a process may do before it wins, and the deployment identity material it needs in order to serve the fleet once it does.

## Decision

### The election is the session lock plus the epoch. There is no consensus protocol and no failure detector.

PostgreSQL releases a session advisory lock when the session ends. That is the whole of the leader-election primitive: **a controller is dead when its session is**, decided by the database rather than by billet, and a standby that is waiting on `pg_try_advisory_lock` becomes the controller at the moment the incumbent's session goes away.

This is deliberately not a lease. A lease needs a renewal, a renewal needs a timeout, and a timeout is a number that decides whether a live controller is declared dead — which is a decision billet would then own and get wrong under load. Nothing here has a clock in it.

**What it does not promise** is the ordinary fencing-token limit, already stated by `checkLeadership` and repeated here because it is the thing most likely to be misread: nothing can stop a predecessor writing *before* a successor claims. What is guaranteed is that once a successor has claimed, the predecessor writes nothing more.

**A promotion also waits out the old leader's message sessions.** The claim is the database's; a scale set's message session is GitHub's, and it is refused to a successor under a different owner exactly as to the same one (measured 2026-09-04, `TestLiveSessionReplacement`). So a promoted standby's listeners wait after the claim they already hold, and how long that wait lasts is not measured: the eight runs that measured one followed a successor under the ORIGINAL owner, still refused at 60 seconds and open between 91 and 92, which is the restart rather than the promotion. Nothing queued is lost in that window; what the measurement establishes for a promotion is the refusal, not how long a differently named successor then waits.

**Detection latency belongs to PostgreSQL.** Under a clean crash the session ends immediately and promotion is prompt. Under a partition it ends when the server's own TCP keepalive or `idle_session_timeout` policy says so. billet adds no watchdog, deliberately: the window survives detection, and a watchdog that stops a healthy control plane over a transient blip is its own failure. An operator who wants a tighter bound tunes the database. Measured on 2026-09-04 (`make promotion-rehearsal`, [Host rehearsals](../records/host-rehearsals.md)): with `tcp_keepalives_idle=10`, `tcp_keepalives_interval=5` and `tcp_keepalives_count=3` on the server, the standby was promoted 21 seconds after the leader was cut off, the node re-registered with it 62 seconds after the partition, and the healed old leader stopped, was restarted by systemd and stood by within 10 seconds.

### The claim authorises a migration; the directory lock does not

`openDir`'s rule was "migrating is the lock holder's job", and it is exactly right while the ledger is a file: holding the state directory excludes every controller that could open it, because SQLite must be on local storage and billet verifies that at startup.

On a ledger another machine can reach it authorises the wrong process. A second host takes its **own** directory's lock happily, so a `billet server` about to be refused as a second controller would upgrade the shared schema on its way to finding that out — and so would `billet check` or `billet nodes approve` run from that host, neither of which is a controller at all. The state-backend plan's invariant list already says only the authoritative migration owner changes schema; this makes the ledger's own exclusion that owner.

So a non-admin open takes the controller exclusion **before** it migrates. This ordering is only possible because the exclusion needs no schema: `pg_try_advisory_lock` is a server operation, and only the *record* half of a claim needs a table to write to.

**An operator command still migrates an unheld ledger, and gives the exclusion straight back.** Somebody has to create the schema on a deployment whose server has never started — `billet check` and `billet ca issue` both run before the first `billet server` — so refusing outright would leave a fresh PostgreSQL deployment with no way to initialise. Holding it afterwards was the alternative and is worse: a command is not the controller, `billet local backup` holds its handle across a whole ledger snapshot, and that would become a window in which no control plane can start, refused by an `ErrControllerHeld` quoting whichever holder the claim row last recorded rather than the command actually in the way. A diagnostic that names the wrong process is worse than none.

Releasing it is what makes the handle re-verify the schema inside every later transaction: once the exclusion is gone a control plane can start and migrate, so nothing verified at open is still guaranteed at the moment the command writes. That is the same rule `OpenAdmin` has always followed without the lock, applied to the new way of not holding one.

### Two questions the directory lock used to answer alone

`DB` now carries `unlocked` and `revalidate` separately, and they are only equal while the ledger is a file:

- **`unlocked`** — this handle did not take the state directory's lock. It is still what decides whether a SQLite handle may claim the controller at all.
- **`revalidate`** — this handle does not hold the controller exclusion for its lifetime, so something else may migrate underneath it and every transaction must re-check.

They come apart in both directions on a shared ledger: a command on a second host takes its own directory lock and holds no exclusion at all, and one that took the exclusion only long enough to migrate has given it back by the time it starts writing.

### A standby is the same control plane, stopped at one line

There is no second implementation. `runServer` opens a handle that cannot write, builds everything that validates configuration, installs its signal handler, and then calls `becomeController` — and every authoritative act in the function is below that call. `TestEverythingAuthoritativeHappensAfterTheControllerClaim` asserts that structurally, because the hazard is an ordering and an ordering has nothing to observe at run time without standing up two controllers and a shared ledger.

**The one authority read is the reason the ordering is not merely tidy.** `BuildNodeWire` reads the node-wire CA once and freezes the TLS config, the trust bundle and the issuing authority from that snapshot. Taken at process start, a `billet ca rotate` between a standby starting and being promoted would leave the promoted controller presenting, trusting and issuing from a stale read — which is exactly the not-fail-closed state `LoadServing`'s single-read design exists to prevent, reached again through a new door.

**A standby's handle refuses writes, and that is not belt-and-braces.** The epoch fence exempts a handle that never claimed, deliberately, because `migrate` runs before any claim and `OpenAdmin` holds none. So the fence protects a controller that has been *replaced* and does nothing whatever for one that has *never* claimed. `Tx` refuses a standby outright — at the one choke point every write crosses, rather than by auditing which goroutines the standby runtime happens to start, which is a fence with a hole per goroutine.

**A standby migrates at promotion and not before**, which is what lets a newer standby wait beside an older leader — the shape a follower-first upgrade needs. Its open therefore asks a weaker question than an operator command does: a schema *ahead* of this binary is refused at startup, because a process that does not know every applied version could not serve the deployment if promoted and there is nothing it could do about that at the failover; a schema *behind* it is left alone.

### `READY=1` before the wait, and a `STATUS=` line beside it

The packaged unit is `Type=notify` with `TimeoutStartSec=120`, so a standby that withheld readiness until promotion would be killed at two minutes and restarted forever — the same restart-loop argument `runServer` already settles for a tier waiting on GitHub to expire a message session. A waiting standby is doing its job. What it needs is a way to say which job, so `notifyStatus` sends `STATUS=standby: waiting for the controller claim (<holder> holds it at epoch <n>)`, refreshed on every poll because `systemctl status` shows only the latest. The journal line beside it is rate-limited to one every five minutes, because a healthy pair may wait for days.

### The layout is a config key, not a flag, and not automatic

`server.controllers: single | active-passive`, defaulting to `single`.

**Automatic waiting is refused because it deletes a diagnostic.** Today a second PostgreSQL controller exits non-zero naming the holder and `Restart=on-failure` repeats that refusal every `RestartSec` — a loud, visible account of a real misconfiguration. If every refused controller waited instead, "two units are misconfigured as active" would become a deployment that looks healthy and has quietly halved itself.

**A `--standby` flag is refused because the packaged unit's `ExecStart` is fixed**, so it would need a drop-in, and because the two hosts stop being symmetric: after one failover the "standby" is the controller and its unit file names a role its host no longer has. Both hosts writing the same config key stays true through any number of failovers.

**It is refused beside a SQLite ledger** rather than ignored. There is nothing to elect over — the ledger is a file on local storage, and billet refuses to put one anywhere a second machine could reach — so a standby there would wait forever for a lock only its own host's other process could hold.

### Shared identity is REPLICATION over the file layout, not a shared authority

A promoted controller has to present a certificate the fleet already trusts, and `wirecert.LoadServing` reaches `LoadOrCreateCA`, which **creates** an authority when the directory is empty. So a standby that has never held this deployment's CA mints a rival one at promotion and drops the entire fleet, while looking perfectly healthy. That is the failure the identity store exists to remove.

**The obvious shape — put the authority IN the store and read it from there — was rejected**, and the reasoning is the whole of this decision. `internal/wirecert`'s safety argument is a state machine over five file names: `ca-previous.crt` says a rotation was STARTED and `ca-previous.key` says it is COMMITTED, the current pair installs key-then-cert and is read cert-then-key so exactly one tear is possible and `repairPair` resolves it, and `LoadServing` confirm-reads because the write ordering bounds a reader only inside one rotation. Every clause of that rests on properties a remote key/value store does not have: cross-key write ordering, per-file durable visibility, `O_EXCL` creation, a crash-releasing flock, and read-after-write consistency for the confirming read. A backend interface underneath those functions would port the code onto a substrate where its reasoning is simply false.

**So the store is a channel and the files stay the source of truth.** A controller publishes what it holds; a controller holding **nothing** adopts. Nothing ever writes over a local authority — a host holding a different one is refused, naming both fingerprints, because billet cannot tell a host left behind by a rotation from a host pointed at the wrong deployment and the file it would replace is what every node verifies against. `billet ca sync --force` is an operator making that judgement, and even then the superseded directory is moved aside rather than deleted, because it holds a private key.

**The ordering in `runServer` is the load-bearing part.** Adoption is after the claim and **before** `serveNodeWire`, or the wire's own read mints the rival authority first; publication is **after** it, because on a first controller the wire is what creates the authority and publishing earlier publishes nothing. Both are asserted structurally, since neither has anything to observe at run time without two hosts and a real fleet.

**What it costs, and the documentation says it in these words:** two controllers do not converge on their own. `billet ca rotate` and `ca retire` publish what they produced, and the other host takes it when somebody runs `billet ca sync`.

### The App key is published after the file, never instead of it

GitHub issues an App private key once and cannot re-issue it. A publication straight from memory has exactly one failure mode with no way back — the App registered, the key gone — so `github-app create` writes the key to disk exactly as it always did and publishes afterwards, and every failure leaves a file that `billet github-app store-key` can publish. The local copy is the staging area, which is `reserveKeyFile`'s model extended rather than replaced. Writes are no-overwrite, which Parameter Store provides natively.

## Consequences

- A second `billet server` on a PostgreSQL ledger is refused **at the open** rather than at a later `ClaimController`, and the schema it found is the schema it leaves.
- `claimController` is idempotent, because the open takes the exclusion and `ClaimController` takes it again before recording the epoch. A second acquisition would come from a second connection — a second session asking for a lock this process already holds — and the control plane would report itself as its own second controller.
- SQLite deployments are untouched. `sharedLedger()` is false there, the directory lock is the exclusion, and every path behaves exactly as it did.
- **A PostgreSQL ledger records which deployment it belongs to** (migration 45), written by the first claim and compared by every later one, and every operator command asks the read-only half. Two hosts sharing one ledger under two identity directories is refused naming both, because that state is two control planes admitting nodes against two authorities while charging capacity into one record.
- **The node-wire authority and the App key can be shared through AWS Parameter Store** (`server.identity.backend: aws-ssm`), as SecureStrings under one prefix, so an active/passive pair does not depend on somebody keeping two copies in step. Both remain files on each host; the store is how a host with none gets the deployment's.
- **What remains the operator's is the address.** Every node dials one `server_addr`, so a failover is promotion plus that address arriving at the promoted host — a floating IP, a DNS change or a load balancer. billet does not own any of those.

## Alternatives rejected (shared identity)

**The authority as a document IN the store, read from there.** Ports a five-file state machine onto a substrate with no cross-key ordering, no `O_EXCL`, no crash-releasing lock and no read-after-write consistency — keeping the code and discarding every reason it is correct.

**Automatic convergence: adopt whatever the store publishes, always.** Makes the store able to replace the key every node in the fleet verifies against, on a schedule nobody chose, from a value billet cannot prove is newer rather than merely different.

**AWS Secrets Manager.** A second service for the same job, and `DeleteSecret` imposes a seven-day recovery window unless forced where `DeleteParameter` is immediate. billet already speaks Parameter Store, with signing vectors in the tree.

**CA private keys as ledger rows.** Every `pg_dump` would then carry them, and the DSN would become the credential that guards the deployment's authority.

## Alternatives rejected

**A lease with renewal on database time.** Needs a timeout, and a timeout is a number that decides whether a live controller is dead. The session lock needs neither.

**Detecting a lost session and stopping.** Rejected in phase 3a and unchanged: the window between the partition and the observation survives any detector, and a watchdog that stops a healthy control plane over a blip costs more than it saves. The epoch refuses the write instead, which is exact.

**Letting the admin handle keep the exclusion after migrating.** Turns every long operator command into a window where no control plane can start, and the refusal names a row rather than the process in the way.

**Refusing to migrate from an admin handle at all.** Leaves a fresh PostgreSQL deployment with no way to create its schema, because `billet check` runs before the first `billet server`.
