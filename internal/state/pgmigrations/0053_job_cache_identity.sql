-- migration 53: job_cache_identity, for PostgreSQL
--
-- The twin of migrations/0053_job_cache_identity.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN job_owner text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN job_repository text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN job_workflow_ref text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN job_event text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_id text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_owner text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_repository text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_workflow_ref text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_event text NOT NULL DEFAULT '';
-- +billet:end
