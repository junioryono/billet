-- migration 65: job_phase_times, for PostgreSQL
--
-- The twin of migrations/0065_job_phase_times.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN launched_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN registered_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN runner_id bigint NOT NULL DEFAULT 0 CHECK (runner_id >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN custody_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN teardown_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN quarantined_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN launch_incarnation text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN holder_incarnation text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_github_run_idx ON job_history (github_run_id);
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_run_idx ON job_history (run_id);
-- +billet:end
