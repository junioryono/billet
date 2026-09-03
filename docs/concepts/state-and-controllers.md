# State and controllers

## The ledger

Everything authoritative about capacity lives in one database: leases and their phases, node registrations and their liveness, job history, admission (whether the deployment is accepting work), enrollment requests, revocations, the compute-barrier requests a drain uses, rollout decisions, and the controller claim. There are two profiles:

| | SQLite (default) | PostgreSQL |
|---|---|---|
| Configure | `server.state_dir: /var/lib/billet/server` | `server.identity_dir` plus `server.state: {backend: postgres, postgres: {dsn_env: BILLET_DSN}}` |
| Where it lives | `billet.db` inside the state directory, on **local** storage (never EFS or NFS; billet checks) | a database you operate |
| Recovering the controller | restore the same host or its disk; on AWS, EC2 auto-recovery of the same instance and EBS volume | rebuild the controller from scratch and point it at the database |
| Two controllers | refused | `server.controllers: active-passive`: a standby waits for the claim and promotes when the incumbent's database session ends |
| Backup | `billet local backup` captures the ledger with the rest of the deployment | your database's own backup; billet's archive carries the other three pieces and records the ledger as external |

Both profiles run the same 47 migrations and the same query set, and hold the same invariants: one authoritative controller, writes serialised through one connection, durability verified at open, contention retried rather than reported. Changing profile is a deliberate migration, not a config edit; [PostgreSQL and active-passive controllers](../deploying/postgres-and-active-passive.md) walks it.

## One controller

A control plane takes an exclusive lock on its state directory, so a second `billet server` on the same directory refuses to start. Operator commands (`billet status`, `nodes approve`, `leases release`, `check`, and the rest) open the ledger without that lock, because a one-shot command makes no scheduling decisions and its writes are ordinary transactions the database serialises against the server's own; that is why they work against a live deployment, which is when you need them. What they will not do is upgrade a schema a running control plane is mid-transaction against: a newer CLI against an older running server refuses and says which side to restart. A stopped deployment is the other case, and whoever opens the ledger first migrates it, so upgrade the server binary when you upgrade the CLI.

On PostgreSQL, the exclusion is a session-scoped advisory lock the database holds for as long as the controller's session lives, and every write transaction re-reads the controller's epoch before it runs. A controller that was replaced (its session dropped, a successor claimed) finds its next write refused, stops itself, and exits non-zero so its service manager either takes the deployment back or reports who holds it. It destroys nothing on the way out: the successor re-adopts the guests, expires the abandoned GitHub session, and reclaims the leases. There is no lease, no renewal timer and no failure detector in billet's own code; a controller is dead when its database session is, and the database decides that. [ADR-009](../reference/decisions/adr-009-controller-election.md) has the argument.

## What a restart does

A control plane that restarts waits for its own GitHub message session to expire (GitHub will not hand one over), clears the fleet's liveness (a plane that has just started has formed no judgement), reads the node-wire authority, opens its listeners, and re-adopts every lease whose node still holds compute. Jobs running when the controller died are left to finish. What a restart costs is scheduling latency, which is acceptable because GitHub queues a job for 24 hours when no runner is available ([ADR-001](../reference/decisions/adr-001-control-plane-hosting.md)): a dead controller delays CI rather than failing it, which is why the requirement is "recovers in minutes" and not high availability.

## Admission

The deployment is either open or sealed. `billet drain` seals it (an operator seal); `billet local down` seals it on the way to stopping (cleared by the next `billet local up`); `billet local recover` seals it after replacing the ledger. A seal stops new work being admitted and says nothing about what is already running; what decides whether it is safe to stop is the two-stage barrier `billet drain --wait` runs, which first asks the ledger and then asks every machine what it is actually running. [Draining and stopping](../operating/draining-and-stopping.md) covers it.

## Migrations are published bytes

A migration is identified by its version and a checksum over its statement bytes, and every deployment records both. A binary refuses a ledger whose recorded checksums disagree with what it carries and refuses a ledger written by a newer version. So the schema only ever moves forward, one dense version at a time, and the release manifest publishes the schema version so an upgrade can refuse a candidate below the installed ledger before anything stops ([Upgrades](../operating/upgrades.md)).
