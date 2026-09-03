# Control-plane migrations

One file per migration, `<zero-padded version>_<lower_snake_name>.sql`, discovered by `go:embed` and parsed by `internal/state/migrationfiles.go`. The filename is the only source of the version and the name; nothing inside the file declares either.

## The bytes between the markers are published

```
-- prose, freely editable
-- +billet:statement
<published bytes>
-- +billet:end
```

A migration is identified by its version **and a sha256 of its statement bytes**, and that sum is recorded in every deployment's `schema_migrations` table. `Open` refuses a recorded checksum that disagrees with what the binary contains, which is correct and is also a control plane that will not start.

So: **the text between the markers may never be changed, for any reason, including reformatting.** Comments, indentation and whitespace inside a statement are part of the bytes. This is not a style preference — commit `ef84c7b` added two explanatory lines inside migration 1's `CREATE TABLE`, the SQL was unchanged in every sense that matters to SQLite, and every ledger written before that commit stopped opening. It was found on the reference host.

Migrations 1–42 therefore carry the tab indentation they inherited from having been Go raw string literals nested in a struct literal. It looks odd and it is correct. Leave it.

The prose above the first marker, and between statements, is *not* hashed and is free to edit. That is where a migration's reasoning belongs.

`migrationfreeze_test.go` holds the published checksum of every shipped migration and fails in CI if one changes, so a reformat is caught on the commit that makes it rather than during somebody's upgrade.

## Adding one

1. Take the next integer. Versions are **sequential and dense**, and that is load-bearing: `state.LatestSchemaVersion()` is published in the release manifest as a single number, and the upgrade fence refuses a candidate below the installed ledger's maximum. A single maximum is only a sound proxy for "this binary knows every version the ledger applied" while versions are dense and totally ordered — a version numbered into a gap breaks it, and the failure is a control plane that passes the fence, stops, and *then* cannot open the database it inherited.
2. `0043_what_it_does.sql`, LF line endings, one `-- +billet:statement` / `-- +billet:end` pair per statement, no semicolons.
3. Write the prose. Say what invariant the change carries and what failure it prevents.
4. Run the tests. `TestNoShippedMigrationHasBeenEdited` will fail and print the exact line to add to `migrationsAreFrozen`. Add it.
5. `TestADatabaseWrittenByAnEarlierBilletUpgrades` in `state_test.go` winds a database back to an older shape by hand, so a migration that adds a table or column also needs an entry in its wind-back list. That is deliberate friction: the test's whole point is running the new migration against rows, and it cannot do that if nothing undoes the new shape first.

If two branches both take 43, CI says so by name and one of them renumbers. That is safe precisely because neither has shipped.

## What does not belong here

- **Down migrations.** Restoring an older binary is a snapshot-and-restore operation. Reversing schema while newer code may already have written it is not a rollback.
- **Editing a shipped migration to fix it.** Append a new one. Migration 7 corrects migration 6's backfill exactly this way, and its comment says why.
- **`schema_migrations` itself.** The bookkeeping table is bootstrapped idempotently outside the versioned set; see `bootstrapSchemaMigrations`.
