---
name: billet-add-migration
description: "Load when adding a ledger migration or a query: the next dense version, the SQLite file and its PostgreSQL twin, the frozen checksums, the wind-back list, `sqlc generate`, binding a query into ReadOps or WriteOps, and the tests that prove the set still fits the schema."
---

# Adding a migration or a query

A checklist. The rules behind each step, and the incidents that wrote them, are in `billet-state` (its `references/migrations-and-queries.md`), and `internal/state/migrations/README.md` is the procedure's own statement.

## A migration

1. Take the next integer: one above the highest file in `internal/state/migrations/`. Versions are dense from 1, and `TestMigrationVersionsAreDenseFromOne` holds it.
2. Write `internal/state/migrations/<zero-padded version>_<lower_snake_name>.sql` with LF line endings, one `-- +billet:statement` / `-- +billet:end` pair per statement and no semicolons. Put the reasoning (the invariant the change carries and the failure it prevents) in the prose above the first marker, which is not hashed.
3. Write its twin in `internal/state/pgmigrations/` with the same version and name, translated by the declared substitutions only; `TestEveryPostgresMigrationIsItsSQLiteTwinTranslated` re-derives it.
4. Run the state tests. `TestNoShippedMigrationHasBeenEdited` and `TestNoShippedPostgresMigrationHasBeenEdited` fail and print the exact lines to add to `migrationsAreFrozen` and `pgMigrationsAreFrozen`; add them.
5. Update every test that rewinds a ledger to an earlier schema and touches what the migration changes (a table, a column, an index or a constraint): the wind-back list of `TestADatabaseWrittenByAnEarlierBilletUpgrades` in `state_test.go`, and any fixture that rebuilds one table by hand, such as `TestAPendingCompletionWrittenAtVersion25SurvivesVersion26`, which recreates `pending_completions` and removes only migrations 26 and 53 from the bookkeeping. A rewind that leaves the new migration recorded while undoing its effect tests a ledger no deployment has; undo the dependent effects and remove their migration records before reopening.
6. Run `sqlc generate`: sqlc reads the migration directory as its schema, so the generated `ledgerdb` changes with it. Commit what it generates; `sqlc diff` proves it is current.

A migration creates tables, their columns and their indexes, and references only tables an earlier migration created: never a foreign key, view, sequence default or inheritance from an older table onto a table it creates. A PostgreSQL restore from an archive taken before the migration drops the tables its dump does not name with one `DROP TABLE … RESTRICT` (`state.DropTablesOutside`, `billet-backup-restore`'s `references/archive-and-restore.md`), and a dependency from a kept table onto a dropped one makes every recovery from such an archive refuse.

Never edit a migration once it is on main, not even its whitespace: append a new one. The `.claude` edit hook refuses an edit to a published migration file.

## A query

1. Write it in `internal/state/queries/*.sql`: ASCII only, no `SELECT *` (`-- wildcard-ok: <reason>` is the escape), integer casts as `BIGINT`, and a statement that is portable across SQLite and PostgreSQL rather than a second query set.
2. Run `sqlc generate`.
3. Bind it in exactly one of `ReadOps` or `WriteOps` (`internal/state/queryset.go`). An operation that only reads uses `DB.View`; a read that decides or validates a write stays inside that write's `DB.Tx`, through `ReadQueries(tx)`, as escrow's headroom check does.
4. `TestEveryGeneratedQueryPreparesAgainstTheMigratedSchema` prepares every query against a migrated ledger, and the query-set tests in `queryset_test.go` check the rest.

SQL executed from hand-written, non-test Go anywhere else is refused by the `rawsql` analyzer; an exception needs `//billet:ignore rawsql // <reason>` and a row in `internal/state/queries/README.md`. Tests and generated code are exempt, so a test's SQL needs neither, and a row naming one is stale and refused by `TestTheAllowlistTableNamesEveryRawStatement`.
