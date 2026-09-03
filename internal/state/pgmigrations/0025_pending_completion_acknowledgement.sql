-- migration 25: pending_completion_acknowledgement, for PostgreSQL
--
-- The twin of migrations/0025_pending_completion_acknowledgement.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN acknowledged bigint NOT NULL DEFAULT 0 CHECK (acknowledged IN (0,1));
-- +billet:end
