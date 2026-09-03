-- The migration bookkeeping table, bootstrapped idempotently OUTSIDE the
-- versioned set.
--
-- It is not a migration and must never become one: every versioned migration is
-- recorded HERE, so a table whose own existence had to be inferred from a failed
-- query is a table nothing can record. CREATE TABLE IF NOT EXISTS is what makes
-- running it on every open free.
--
-- THESE BYTES ARE NOT CHECKSUMMED. Unlike internal/state/migrations/*.sql, no
-- deployment records a sha256 of this statement, so editing it is safe in a way
-- editing a migration never is — what constrains it instead is
-- checkBookkeepingSchema, which names the four columns it must have and tells an
-- operator to delete a development database that predates them.
--
-- IT IS READ TWICE, WHICH IS THE REASON IT IS A FILE. state.go embeds it and
-- executes exactly these bytes; sqlc.yaml lists it beside the migration
-- directory so the generated query set knows the table exists. Written as a Go
-- constant it was invisible to sqlc, and the two bookkeeping queries would have
-- had to stay hand-written — a second place ledger SQL lives, for one table.
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	checksum   TEXT NOT NULL,
	applied_at TEXT NOT NULL
)
