-- The migration bookkeeping table, for PostgreSQL.
--
-- The twin of schema_migrations.sql, carrying the same statement with SQLite's
-- spellings translated — the same three substitutions the migration timelines
-- use, and for the same reasons. See pgmigrations/README.md.
--
-- IT IS THE ONE sqlc IS TOLD ABOUT, because the generated query set is built
-- from the PostgreSQL catalogue. That is not a claim that PostgreSQL is
-- privileged: the two schemas are proved equivalent, so a catalogue derived from
-- either types the same Go, and the generated statements are then checked
-- against a real migrated ledger of each engine rather than against the
-- catalogue alone.
--
-- LIKE ITS TWIN, THESE BYTES ARE NOT CHECKSUMMED. No deployment records a
-- sha256 of this statement, so editing it is safe in a way editing a migration
-- never is; checkBookkeepingSchema is what constrains it, by naming the four
-- columns it must have.
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    bigint PRIMARY KEY,
	name       text NOT NULL,
	checksum   text NOT NULL,
	applied_at text NOT NULL
);
