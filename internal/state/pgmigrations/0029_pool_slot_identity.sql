-- migration 29: pool_slot_identity, for PostgreSQL
--
-- The twin of migrations/0029_pool_slot_identity.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE pool_slot_identities (
			lease_id    text PRIMARY KEY CHECK (length(trim(lease_id)) > 0),
			internal_id bigint NOT NULL UNIQUE CHECK (internal_id < 0)
		);
-- +billet:end
