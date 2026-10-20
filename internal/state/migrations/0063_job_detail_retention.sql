-- migration 63: job_detail_retention
--
-- A JOB'S DETAIL IS KEPT FOR A BOUNDED TIME, AND ITS RETIREMENT IS RECORDED.
-- job_series, job_guest_reports and job_destinations grew with every job a
-- monitored node ran, and nothing deleted a row: on 2026-10-17 they were 588 MiB
-- of the reference controller's 740 MB ledger, which grew about 170 MB a day,
-- and the startup integrity scan reads the whole file cold after every host
-- upgrade (#572). The control plane now deletes them past a retention; a job's
-- usage summary (job_usage) stays.
--
-- detail_retired_at is when that happened to this job's row, empty for a job
-- whose detail was never retired, so a report can say "retired" rather than
-- "never measured" for a job whose series, guest report or destinations are
-- gone. The indexes serve the retention: it finds a job's series and
-- destinations by when its usage was recorded, in the same transaction as them,
-- and deletes a batch's detail by the stamp it marked the batch with.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN detail_retired_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE INDEX job_usage_recorded_at ON job_usage (recorded_at)
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_detail_retired_at ON job_history (detail_retired_at)
-- +billet:end
