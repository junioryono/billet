-- migration 84: job_stranded, for PostgreSQL
--
-- The twin of migrations/0084_job_stranded.sql, which says why: whether GitHub kept
-- offering a job billet failed to launch, decided once on that launch's row, and an
-- index over the undecided launch failures alone.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration refuses to
-- open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN stranded text NOT NULL DEFAULT ''
    CHECK (stranded IN ('', 'offered', 'stranded', 'expired'));
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN stranded_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_launch_miss_idx ON job_history(finished_at)
    WHERE failure_reason = 'billet:launch-failed' AND stranded = '';
-- +billet:end
