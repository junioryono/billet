# Control-plane migrations, PostgreSQL

The PostgreSQL twin of `../migrations`. Same file format, same parser (`internal/state/migrationfiles.go`), same freeze rule: the bytes between the markers are published, are hashed into every PostgreSQL deployment's `schema_migrations` table, and **may never be changed, for any reason, including reformatting.**

## Why there are two timelines and only one query set

DDL is the one part of the ledger that is not portable. `STRICT`, an `INTEGER` column with `CHECK (x IN (0,1))` standing in for a boolean, and the table rebuilds that exist only to work around SQLite's `ALTER TABLE` limits are all SQLite's own spelling.

The **queries** are not duplicated. There is one directory (`../queries`), compiled once, and its statements execute on both engines — see `docs/reference/decisions/adr-008-state-backends.md` for the measurement that made that possible. So this directory is the only place billet carries the same idea twice, and everything below exists to stop the two copies drifting.

## Every file here is its SQLite twin, translated

Same version, same name, same statements in the same order, with three substitutions and nothing else:

| SQLite | PostgreSQL | Why |
|---|---|---|
| `TEXT` | `text` | Spelling; the types are the same. |
| `INTEGER` | `bigint` | `INTEGER` is `int4` on PostgreSQL. `bigint` is what SQLite's INTEGER actually is. |
| `) STRICT` | `)` | SQLite-only; PostgreSQL is statically typed already. |
| *(none)* | trailing `;` | A fact about **sqlc**, not about PostgreSQL. billet's format has no semicolons — a statement is delimited by its markers — but sqlc's PostgreSQL parser reads a schema *file*, so without a terminator it runs consecutive statements together and reports `syntax error at or near "ALTER"` on the second. Measured: nine of the 43 files failed to parse until this was added. |

The type rewrite is **lexically aware**, and a regular expression was not enough. A global replacement rewrites the word wherever it occurs, so a future data migration reading `UPDATE t SET kind = 'INTEGER'` would silently store a *different value* on PostgreSQL — and the derivation test would bless it, because it would be comparing the corruption against itself. The scan therefore skips string literals, quoted identifiers and comments, and matches a type name case-insensitively: uppercase-only matching leaves a migration written `integer` untranslated, giving PostgreSQL an `int4` column where SQLite has a 64-bit one, and passing. None of today's 121 statements has a type name in any of those contexts, so this costs nothing now; it is there for the migration that has not been written yet.

**`TestEveryPostgresMigrationIsItsSQLiteTwinTranslated` re-derives that translation and compares**, so a file here cannot silently diverge from the one it mirrors, and a new SQLite migration with no twin fails by name. That is also why these files keep the SQLite timeline's odd tab indentation rather than being reformatted: the two are meant to be read side by side, and a prettier copy would defeat both the diff and the test.

**There is no escape hatch, deliberately.** A migration that cannot be translated mechanically is a design decision — two schemas that are no longer the same shape — and it should be made in the open, by whoever hits this test, rather than waved through by an exception list nobody reads. An unused exception mechanism is also untested machinery, which is its own way of being wrong.

## The versions are the same integers, deliberately

`state.LatestSchemaVersion()` is published in the release manifest as a single `int`, and two binaries compare it across an upgrade without either knowing what backend the other was configured for. One number therefore has to describe the binary, which it can only do while every timeline declares the same versions — `TestBothTimelinesDeclareTheSameVersionsAndNames` is what holds that.

Checksums are the half that legitimately differs, because they are over one engine's own statement bytes. Nothing compares them across timelines.

## Two catalogue differences are real, and both are PostgreSQL being stricter

Measured by applying both timelines and comparing `information_schema.columns` against `PRAGMA table_info`:

- **A text column's default renders as `''::text` rather than `''`.** The same default, written the way PostgreSQL writes one.
- **`admission.id`, `compute_barrier.id` and `force_destroy.generation` are `NOT NULL` here and nullable on SQLite.** SQLite's `INTEGER PRIMARY KEY` is the rowid alias, so `PRAGMA table_info` reports it nullable and an omitted value is auto-assigned. Every statement in `../queries` names those columns explicitly, so nothing relies on that; PostgreSQL refusing the omission is the safer of the two.

- **Five tables carry a primary-key constraint named after the temporary rebuild table** — `leases_new_pkey1`, `revoked_certs_new_pkey`, `node_enrollments_new_pkey`, `join_tokens_new_pkey`, `pending_completions_new_pkey`. A rebuild migration creates `<table>_new`, copies into it, drops the original and renames; PostgreSQL keeps the constraint name it generated, uniquifying it when one is already taken (which is where the trailing `1` comes from — migration 16's rebuild left a `leases_new_pkey` behind that migration 20's had to work around). SQLite does not name a table's implicit primary key at all, so there is nothing to compare against, the names are deterministic given the fixed migration order, and no query references one. **The trap this leaves is for a future migration:** `ALTER TABLE leases DROP CONSTRAINT leases_pkey` would fail, because that is not what the constraint is called. Look the name up rather than assuming it.

Everything else — 193 columns, their types, nullability and defaults — is identical.

## Adding one

Add the SQLite migration first, then run `TestEveryPostgresMigrationIsItsSQLiteTwinTranslated`. It fails naming the missing twin and prints the translated bytes to put in it. Then `TestNoShippedPostgresMigrationHasBeenEdited` prints the line to add to `pgMigrationsAreFrozen`.
