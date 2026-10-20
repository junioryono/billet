-- migration 63: job_detail_retention, for PostgreSQL
--
-- The twin of migrations/0063_job_detail_retention.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN detail_retired_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
CREATE INDEX job_usage_recorded_at ON job_usage (recorded_at);
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_detail_retired_at ON job_history (detail_retired_at);
-- +billet:end
