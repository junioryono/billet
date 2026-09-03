-- name: ListAppliedMigrations :many
-- Every migration this ledger has recorded, oldest first.
--
-- ORDERED so two readings of one ledger compare and serialise identically; the
-- migrator itself keys on version and does not depend on the order.
SELECT version, name, checksum FROM schema_migrations ORDER BY version;

-- name: RecordMigration :exec
-- Record one migration as applied, in the same transaction that applied it.
--
-- The checksum is over the statement BYTES, so this row is what a later open
-- compares against to refuse an edited migration.
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (@version, @name, @checksum, @applied_at);
