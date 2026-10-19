-- migration 62: job_github_run, for PostgreSQL
--
-- The twin of migrations/0062_job_github_run.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_run_id bigint NOT NULL DEFAULT 0 CHECK (github_run_id >= 0);
-- +billet:end
