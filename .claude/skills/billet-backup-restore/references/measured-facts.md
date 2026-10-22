# Measured facts

Part of the `billet-backup-restore` skill: what was measured, dated where it was recorded.

- The first rehearsal failed at `billet server --upgrade-probe` under the service account with permission denied on the App key.
- `VACUUM INTO`: refused in a transaction, refused on the query-only pool, refuses an existing destination, creates 0644.
- `O_RDWR` on a `0444` kernel: permission denied; chmod and fsync through a read-only descriptor succeed; a no-op chmod on a read-only mount: EROFS.
- A test hook failing every directory sync never reached the reuse branch's own flush, which is how deleting it survived a mutation run.
- Kernel lock mutation: removing the reaper's skip alone leaves the test green; removing it and widening the pattern turns it red.
- PostgreSQL client (2026-10-20, server 18.6, clients 18.1 and 17.6):
  - libpq does not expand a URI in `PGDATABASE`: it dialled the default socket.
  - libpq refuses `search_path` as a URI parameter (`invalid URI query parameter`), while `PGOPTIONS='-c search_path=x'` works.
  - `pg_dump` 17 against server 18: `aborting because of server version mismatch`, exit 1. `pg_restore` 17 lists an 18 archive.
  - `pg_dump --schema=<s>` writes the schema as its own TOC entry (and `COMMENT - SCHEMA` for `public`). `--clean --if-exists` then emits `DROP SCHEMA IF EXISTS <s>; CREATE SCHEMA <s>`, and with another object in the schema the single-transaction restore failed at the drop and created none of its tables.
  - `pg_export_snapshot()` in a REPEATABLE READ READ ONLY transaction under `default_transaction_read_only=on` is accepted by `pg_dump --snapshot`. The dump omits a row committed after the export. Another database refuses it: `cannot import a snapshot from a different database`.
