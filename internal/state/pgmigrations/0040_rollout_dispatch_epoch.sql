-- migration 40: rollout_dispatch_epoch, for PostgreSQL
--
-- The twin of migrations/0040_rollout_dispatch_epoch.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE rollout_nodes ADD COLUMN dispatch_epoch bigint NOT NULL DEFAULT 0
			CHECK (dispatch_epoch >= 0);
-- +billet:end
