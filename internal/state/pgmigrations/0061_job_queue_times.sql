-- migration 61: job_queue_times, for PostgreSQL
--
-- The twin of migrations/0061_job_queue_times.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_queued_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_scale_set_assigned_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_runner_assigned_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN bought_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_started_idx ON job_history(started_at);
-- +billet:end
