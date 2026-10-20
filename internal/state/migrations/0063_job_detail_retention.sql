-- migration 63: job_detail_retention
--
-- A JOB'S DETAIL IS KEPT FOR A BOUNDED TIME, AND ITS RETIREMENT IS RECORDED.
-- job_series, job_guest_reports and job_destinations grew with every job a
-- monitored node ran, and nothing deleted a row: on 2026-10-17 they were 588 MiB
-- of the reference controller's 740 MB ledger, which grew over 100 MB a day,
-- and the startup integrity scan reads the whole file cold after every host
-- upgrade (#572). The control plane now deletes them past a retention; a job's
-- usage summary (job_usage) stays.
--
-- detail_retired_at is when that happened to this job's row, empty for a job
-- whose detail was never retired, so a report can say "retired" rather than
-- "never measured" for a job whose series, guest report or destinations are
-- gone. The indexes serve the retention: it finds the jobs whose usage still
-- claims destinations (a verdict it clears when it deletes them, so the index
-- holds only what is left to retire, not the deployment's history), and it
-- deletes a batch's detail by the stamp it marked the batch with.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN detail_retired_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE INDEX job_usage_destinations_kept ON job_usage (lease_id) WHERE destinations_incomplete IS NOT NULL
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_detail_retired_at ON job_history (detail_retired_at)
-- +billet:end
