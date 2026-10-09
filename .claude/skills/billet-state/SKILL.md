---
name: billet-state
description: "Load when adding a migration or a query, touching internal/state, internal/state/queries, internal/alloc's or internal/rollout's SQL, sqlc.yaml, anything that opens the ledger (Open, OpenAdmin, OpenMaintenance, OpenPostgresStandby, OpenInspect), DB.Tx or DB.View, the locks and fences, the release watermark, or the controller claim, its epoch fence and the standby code in internal/app's control plane."
---

# The ledger

## What this area is

`internal/state` owns the durable control-plane store: `DB`, the open modes, transactions, the migrator, the locks and fences, the controller claim, admission, pending completions, force-destroy records and the cache kill switch. SQL lives only in `internal/state/queries/*.sql` (including `internal/alloc`'s and `internal/rollout`'s), compiled by sqlc (pinned, `SQLC_VERSION`) into `internal/state/ledgerdb` and bound by `internal/state/queryset.go`. Migrations are files in `internal/state/migrations` (SQLite) and `internal/state/pgmigrations` (PostgreSQL), 59 of each, discovered by `go:embed`. `docs/reference/decisions/adr-008-state-backends.md` and `adr-009-controller-election.md` record the decisions.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Engines, transactions and open modes: [references/engines-and-opens.md](references/engines-and-opens.md)

- **The engine is a seam and the invariants are not.**
- **Every write transaction begins IMMEDIATE, and that is not tuning.**
- **The writer slot is observed from inside `DB.Tx`, and the observer must return at once.**
- **A ledger writer reaches no network.**
- **Three open modes, and what each may do.**
- **An open's own startup budget is a deadline, and a caller can tell it from its own.**
- **Unreachability is a property of the ERROR, never of the step that produced it, and its rule is MEASURED.**
- **`OpenPostgresCompletion` writes one row and is not a control plane.**

### The claim, locks, fences and the watermark: [references/claims-locks-and-fences.md](references/claims-locks-and-fences.md)

- **On a shared ledger the claim authorises the migration, not the directory flock.**
- **The claim's epoch is a fence read inside every write transaction.**
- **Refusing the write is not stopping the process.**
- **An election is a process that waits for the claim.**
- **A promotion is as fast as the database's keepalives, and that is the operator's to set.**
- **Locks and fences are four different things.**
- **The release watermark refuses a proved downgrade at every open, and only the control plane raises it.**
- **A controller's retirement is one row the two controllers contend for, and a `done` row is never deleted.**

### Migrations and the query set: [references/migrations-and-queries.md](references/migrations-and-queries.md)

- **Migrations are published bytes.**
- **The markers are billet's own and a near miss is refused.**
- **Versions are dense from 1 and that is load-bearing.**
- **The PostgreSQL timeline is derived.**
- **One query set serves both engines, generated once with the PostgreSQL engine.**
- **`ReadOps` and `WriteOps` are hand-written on purpose.**
- **The gate that proves the set still fits the schema is `TestEveryGeneratedQueryPreparesAgainstTheMigratedSchema`.**
- **Raw SQL is banned mechanically.**
- **Migrations 53 to 55 carry #226's cache evidence and outcomes.**
- **The ledger is not authoritative for cache generation pointers.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**Adding a migration.** Add the SQLite file and its PostgreSQL twin with the next dense version, never edit a published one (`migrationsAreFrozen` holds every sum; revert the file, never the table), run `make sqlc`, and take the new columns back in the rewind tests in `state_test.go`.

**Adding a query.** Put it in `internal/state/queries/*.sql` (ASCII only, no `SELECT *`, integer casts as `BIGINT`), run `make sqlc`, and bind it in exactly one of `ReadOps` or `WriteOps`; `TestEveryGeneratedQueryPreparesAgainstTheMigratedSchema` proves it fits the schema.

**A read.** Use `DB.View`, never `DB.Tx`, which takes the single writer slot.

## Where the tests are

- `internal/state/migrationfreeze_test.go` (`migrationsAreFrozen`, `TestNoShippedMigrationHasBeenEdited`), `migrationfiles_test.go`, `pgmigrations_test.go` (`TestEveryPostgresMigrationIsItsSQLiteTwinTranslated`, `TestMigrationVersionsAreDenseFromOne`).
- `internal/state/queryset_test.go` (prepares every query, claims each by exactly one half, `BIGINT`, ASCII, no wildcard), `rawsqlallowlist_test.go`, `faultdriver_test.go`.
- `internal/state/deploymentlock_process_test.go` (a real second process), `controller_test.go`, `postgresbackend_test.go` (gated by `BILLET_TEST_POSTGRES_DSN`).
- `cmd/billet/standbystop_test.go`, `adminlock_test.go`; `internal/app/claim_test.go`, `proofs_test.go`, `leadershipwiring_test.go`.

## Related skills

`billet-capacity` (what the transactions decide), `billet-backup-restore` (the fence and the barrier in use), `billet-checks-and-lint` (`rawsql`, depguard), `billet-releases-and-upgrades` (the schema version in the manifest).
