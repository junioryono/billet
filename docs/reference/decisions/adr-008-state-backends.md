# ADR-008: SQLite and PostgreSQL control-plane state backends

**Status:** accepted, partially implemented (the single-controller PostgreSQL profile)

## Context

billet's control-plane state — the capacity ledger, node registrations, custody, job history, rollouts — lives in SQLite on the controller's local disk. ADR-001 chose that and still stands for the common case: the data is small and hot, SQLite gives it ACID semantics with no daemon to operate, and a dead controller delays CI rather than failing it because GitHub queues a job for 24 hours.

What it does not give is a **replaceable controller**. The authority is a directory on one machine, so recovery means restoring that directory onto local storage, and there is no path to managed database recovery or, later, to a second controller taking over. The state-backend plan asks for three coherent profiles: SQLite on local storage, PostgreSQL with one controller, and PostgreSQL with fenced active/passive controllers. This ADR covers the second and the seam that makes the third possible; leader election is out of scope and is phase 3.

## Decision

### One query set, not one per dialect

The obvious shape is a query set per engine. It would duplicate **167 statements** — the most invariant-dense code in the repository, `internal/alloc`'s escrow and placement among them — and require a second copy of every guard in `internal/state/queries/README.md`. A divergence between the two would be invisible until a scheduling write behaved differently on one engine.

It is not necessary, and that was measured rather than assumed.

**Measurement 1 — sqlc's two engines emit byte-identical Go.** Pointing sqlc v1.31.1's PostgreSQL engine at the existing `internal/state/queries/leases.sql` against an equivalent PostgreSQL schema produced generated code differing from the committed SQLite output in exactly two ways: `?N` became `$N`, and `CAST(… AS INTEGER)` typed as `int32` (PostgreSQL `int4`) rather than `int64`. Respelling those `BIGINT` — and `LIMIT @max_rows` as `LIMIT CAST(@max_rows AS BIGINT)` — removed the second difference entirely. Diffing the two engines' output for the same file then produced **nothing but the placeholders**: same struct names, same field names, same field types, same method signatures. SQLite types `BIGINT` as `int64` too, so the respelling changed no committed byte.

**Measurement 2 — modernc.org/sqlite executes `$N`.** Eleven probes against a real STRICT table, all passing: `$1…$N` bound positionally; a repeated `$1` bound once, which is PostgreSQL's semantics; `$2` appearing textually before `$1` still resolving correctly; `CAST(… AS BIGINT)`; lowercase `CAST($1 AS text)`; `LIMIT CAST($2 AS BIGINT)`; `ON CONFLICT … excluded` with `$N`; `EXISTS` and an `INTEGER 0/1` column both scanning into a Go `bool`; and `RowsAffected` on `:execresult`. SQLite treats `$1` as a named parameter and assigns indices by first appearance, which is exactly sqlc's numbering.

**So generation runs once, with the PostgreSQL engine**, because a `$N` constant runs on both engines and a `?N` constant does not. `internal/state/sqlitedb` became `internal/state/ledgerdb`; `internal/alloc` and `internal/rollout` needed an import rename rather than a rewrite.

**What it costs**, stated because it is a real reduction: `sqlc diff` checks the query set against one catalogue rather than two. The SQLite side is covered by `TestEveryGeneratedQueryPreparesAgainstTheMigratedSchema`, which prepares every generated statement against a real migrated SQLite ledger — a stronger question than a catalogue answers, because SQLite's prepare also resolves an `ON CONFLICT` arbiter and sqlc models no indexes at all.

**What it constrains**, also real: a statement has to be portable. `max(a, b)` is a two-argument scalar on SQLite and an aggregate on PostgreSQL, so `UpsertPendingCompletion`'s three sticky bits are written `CASE WHEN a > b THEN a ELSE b END`. `GREATEST` is the same trap facing the other way. When a statement will not generate, the answer is the portable spelling, never a second query set.

### Two migration timelines, same versions

DDL is not portable. `STRICT`, an `INTEGER` column with `CHECK (x IN (0,1))` standing in for a boolean, and the four table rebuilds that exist only to work around SQLite's `ALTER TABLE` limits are all SQLite's own spelling. So `internal/state/pgmigrations` carries a translated twin of every migration.

**The versions and names are identical, deliberately.** `state.LatestSchemaVersion()` is published in the release manifest as a single `int`, and two binaries compare it across an upgrade without either knowing what backend the other was configured for. One number has to describe the binary, which it can only do while every timeline declares the same versions. Aligning them is what leaves `internal/releasesource` and the upgrade fence untouched by this whole phase. Checksums are the half that legitimately differs, because they are over one engine's own statement bytes; nothing compares one across timelines.

**What makes a duplicated timeline survivable is that it is derived.** Every PostgreSQL migration is its SQLite twin under four declared substitutions — `TEXT`→`text`, `INTEGER`→`bigint`, `) STRICT`→`)`, and a trailing `;` — and `TestEveryPostgresMigrationIsItsSQLiteTwinTranslated` re-derives that translation and compares statement bytes in both directions. It compares **bytes rather than schemas** because that is the stronger question: two files can produce identical catalogues and still disagree about what they *do*, and a backfilling `UPDATE` applied on one engine and not the other leaves every column identical and the data different.

The trailing semicolon is a fact about sqlc rather than about PostgreSQL: its parser reads a schema *file*, so without a terminator it runs consecutive statements together. Nine of the 43 files failed to parse until it was added.

The translation is **lexically aware**, not a regular expression. A global replacement rewrites a type name wherever it occurs, so a future data migration reading `UPDATE t SET kind = 'INTEGER'` would silently store a different value on PostgreSQL — and the derivation test would bless it, because it would be comparing the corruption against itself. Matching only the uppercase spelling is the same defect facing the other way.

**There is no exception mechanism.** A migration that cannot be translated mechanically means the two schemas are no longer the same shape, and that decision should be made in the open by whoever hits the test.

### The engine is a seam; the invariants are not

`internal/state/backend.go` holds what differs: opening and proving a connection, creating and inspecting the bookkeeping table, reading a catalogue, classifying contention and cancellation, taking a snapshot, and what a write transaction must do before anything else.

Everything else stays above it and is written once — the exclusive process lock, the single writer connection, the busy retry and its asymmetric patience, the maintenance fence, the schema re-verification inside every transaction on an unlocked handle, and the migration runner. Each cost a debugging session to arrive at, and an engine that could reimplement one is an engine that could get it wrong.

### `Tx` keeps its meaning on PostgreSQL via an advisory lock

Every allocation decision is read-current, decide, record. SQLite guarantees that by taking a lock on the *file* at `BEGIN IMMEDIATE`, which also excludes the operator command running in another process.

The obvious PostgreSQL mapping is `SERIALIZABLE` plus a retry on `40001`, and **it is wrong here**. Retrying a serialization failure means re-executing the caller's closure, and `DB.Tx`'s closures are not pure. An API whose contract becomes "your function may run twice" is a different API from the one every caller in this repository was written against, and the failure of getting it wrong is a double-charged lease.

So the writer takes `pg_advisory_xact_lock` before anything else. Throughput is serialized, which is exactly what SQLite already does, so nothing regresses; a caller that waits does so under `lock_timeout`, is classified as contention, and is retried by the same loop with the same patience as a busy `BEGIN`. The migration transaction is the one exception: its statements are DDL, which take locks the advisory lock says nothing about, and it runs once before anybody is waiting on the process, so it sets `lock_timeout` to zero for itself (`SET LOCAL`) and waits, bounded by the thirty-second startup budget wherever it runs. Under the writer's fifty milliseconds a `CREATE TABLE` behind any other session's lock was cancelled and the cancellation was the result of opening the ledger (#75).

It is **keyed on the deployment** rather than taken globally, because sharing a PostgreSQL is one of the reasons to run one. It is **transaction-scoped** rather than session-scoped, so a crashed process cannot wedge every writer. Its namespace differs from the controller claim's, because the same key would make a controller block its own writes forever.

### The driver is pgx, and the cost is accepted

Measured against billet's own binary rather than an isolated probe:

| | binary | Δ | modules added |
|---|---|---|---|
| billet today | 40.25 MB | — | — |
| `+ lib/pq` | 40.79 MB | +0.54 MB (1.3%) | 1 |
| `+ pgx/v5/stdlib` | 45.49 MB | +5.24 MB (13.0%) | 4 |

**pgx, at +5.24 MB on every binary including the nodes.** ADR-002 rejected the AWS SDK on a similar measurement, and that precedent does *not* transfer: there, the dependency could be replaced by about six hundred lines of billet's own signer pinned to AWS's own output, which is why the trade was available at all. The PostgreSQL wire protocol is not something to hand-roll. Between the two candidates, `lib/pq` is smaller and in maintenance mode; this is the credential path for the control plane's authority, and the difference is 4.7 MB on a 40 MB binary.

Build variants behind a build tag were rejected: the issue asks for this "without fragmenting ordinary releases by accident", `billet server` and `billet node` are one binary, and a release matrix that doubles is a release matrix where somebody ships the wrong half.

### PostgreSQL refuses what it cannot honestly do

`SnapshotInto` is `VACUUM INTO`, which is what lets `billet local backup` pair the ledger with the deployment identity, the CA and the App key in one archive. There is no equivalent billet should own for PostgreSQL: a consistent copy is `pg_dump` or the provider's snapshot, both the operator's to run. A half-measure that copied rows through billet's own connection would produce an archive that **looks** like a backup and is not, so the call refuses and names what to run instead.

`integrityCheck` is likewise a documented no-op. `quick_check` exists because a SQLite database is a file this process is solely responsible for; PostgreSQL owns its own crash recovery, and amcheck is an extension — a control plane that will not start unless an operator installed one is a control plane that refuses correct deployments.

What *is* checked is the question that survives the change of engine: `synchronous_commit=off` means PostgreSQL acknowledges a commit before the WAL record is on disk, which is exactly what SQLite's `synchronous=FULL` exists to prevent.

## Consequences

- A deployment can put its ledger in a database billet does not operate, and recover the controller from a managed backup.
- Adding a migration means adding two files, and the derivation test prints the second one.
- The PostgreSQL backend is tested against a real server or not at all. CI runs a `postgres:18-alpine` service and the tests **refuse to skip** when `CI` is set, because a run that lost the service would otherwise report success for a backend nothing exercised.
- One controller is still the rule, and PostgreSQL mode now takes a database-scoped claim before it polls GitHub or dispatches: a session-scoped `pg_try_advisory_lock`, released by the server when the session ends, so a crashed controller frees the deployment with no lease, no timeout and no stale row.

  **The claim guarantees that a second controller cannot start, which is narrower than "exactly one controller".** The lock lives in a session billet does not control the lifetime of — a server restart, an `idle_session_timeout`, a pooling proxy or a network partition ends it and releases the lock, and the process finds out about none of it. Detection would narrow the window and cannot close it, and a watchdog that stops a healthy control plane over a transient blip is its own failure.
- **So the epoch is the fence, and `DB.Tx` reads it inside every write transaction.** A controller that lost its exclusion without noticing is *refused* rather than *detected*: its next write comes back `state.ErrLeadershipLost`, naming the replacement and both generations.

  **It is exact rather than best effort, and the reason is serialization.** Every writer already takes the same lock — SQLite's at `BEGIN IMMEDIATE`, PostgreSQL's `pg_advisory_xact_lock` in `beginWrite` — and a successor's claim advances the epoch through `DB.Tx`, so it takes that lock too. If the row still names our epoch inside our transaction, no successor committed a claim before us and none can commit before our `COMMIT`. The check sits *before* the caller's closure, so a fenced controller's callback never runs at all; a check after it would let a scheduling decision be made and only the commit refused.

  **What it does not promise is the standard fencing-token limit, and it is worth stating rather than implying:** it cannot stop a predecessor writing *before* a successor claims, because nothing can. What it guarantees is that once a successor has claimed, the predecessor writes nothing more — which is what makes a handover safe. Reads are not fenced, and neither are operator commands: `OpenAdmin` handles hold no claim, and fencing them would refuse `billet leases release --force` against exactly the live deployment it exists for.

  **Refusing the write is not stopping the process, and that half needs its own mechanism.** Every background writer in the control plane is deliberately patient with an error it cannot classify — a heartbeat keeps its lease rather than dropping it, the reaper logs and retries, a cleanup retry backs off — because the alternative is a database blip failing builds. All of that is right for a blip and wrong for a lost claim: it would leave a replaced controller polling GitHub, holding its message session and running the cleanup loop that calls `Runner.Destroy`, which never touches the ledger and is therefore fenced by nothing. So the refusal also closes `DB.LeadershipLostSignal`, `cmd/billet` selects on it and cancels the plane, and `retryCleanup` refuses outright once fenced so a tick inside that window starts nothing.

  **A fenced control plane then stops without acting on anything** — it destroys no compute, closes no message session and hands back no capacity, because each of those is an authoritative act it no longer has the right to perform and the successor performs all three correctly. That makes a fenced stop deliberately identical to a hard kill, which is a recovery billet already implements: guests keep running and are re-adopted, GitHub expires the abandoned session (the path `openSession` already waits out after any ungraceful restart), and the successor's startup reap reclaims the leases once the heartbeats stop. It exits non-zero, so `Restart=on-failure` either takes the deployment back — right when the replacement was itself transient — or meets the startup refusal naming the holder; exiting 0 would leave a healed partition with no controller at all.

  **An unreadable claim row is deliberately NOT treated as a lost one**, and the reasoning runs the opposite way to the usual fail-closed rule. What makes abandonment safe is that a successor *demonstrably exists* and owns every obligation being dropped; an unreadable row is no evidence of one, and the destroys a fenced teardown skips are for jobs GitHub has already concluded — so refusing to finish them because a query timed out strands containers on somebody's host for a build that ended. The *write* is still refused, and the signal does not fire, so a blip cannot stop a healthy control plane either.

  **What it costs was measured rather than asserted:** one single-row read by primary key per write transaction. On an empty transaction, SQLite goes 8.3µs → 15.8µs and PostgreSQL over loopback 419µs → 585µs — one round trip, which on PostgreSQL is the dominant cost of anything. Against a real `Reserve`+`Release` cycle (two write transactions) SQLite measured 318–327µs unfenced against 328–350µs fenced, and PostgreSQL 2.9–3.1ms against 3.6–4.1ms, where the run-to-run variance is larger than the effect. Writes are serialized behind one lock by design, so this is throughput on a path that was already serial.

  **What still has no election behind it is the promotion.** Nothing decides that a leader is dead or that a follower should take over; that is the election's remaining phase, and this is the half it has to be built on.
- Backup and restore for an external ledger are not yet paired with the deployment identity. `billet local backup` and `restore` remain SQLite-only.
- The transactional host upgrade runs on an external ledger since [ADR-010](adr-010-automatic-updates.md), without copying it: it fences, snapshots and migrates nothing there, and the rollback boundary is the candidate's start.

## Alternatives rejected

**A query set per dialect.** The two-pins problem across 167 invariant-dense statements. Measurement 1 removed the need.

**Hand-written dialect-neutral param and row types.** The shape this phase was planned around before Measurement 1: about 135 types and two sets of conversions, roughly fifteen thousand lines of mechanical code, all of it a place for a field to be mapped wrongly in silence.

**One squashed PostgreSQL migration creating the current schema.** Fewer files, and it breaks the single published `LedgerSchema` integer that the upgrade fence compares.

**`SERIALIZABLE` plus retry.** Changes `DB.Tx`'s contract to "your closure may run twice" for every existing caller.

**A `schema.sql` flattened dump as sqlc's input.** A second source of truth to regenerate on every migration, whose failure mode when forgotten is a catalogue that silently disagrees with the database.
