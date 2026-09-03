-- migration 23: pending_completion_lease, for PostgreSQL
--
-- The twin of migrations/0023_pending_completion_lease.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN lease_id text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN outcome text NOT NULL DEFAULT '' CHECK (outcome IN ('','done','failed'));
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN release_only bigint NOT NULL DEFAULT 0 CHECK (release_only IN (0,1));
-- +billet:end
