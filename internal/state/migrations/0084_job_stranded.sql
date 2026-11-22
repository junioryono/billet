-- migration 84: job_stranded
--
-- WHETHER GITHUB KEPT OFFERING A JOB BILLET FAILED TO LAUNCH, decided once on the
-- row of the launch that started nothing (#911). On 2026-11-20 a consumer's job was
-- assigned, its launch went untaken for the command's ten minutes, GitHub cancelled
-- the assignment twice and never offered the job again: its REST API showed it
-- queued for ten hours. Two cancellations of unrun assignments can end GitHub's
-- offers of a job (docs/reference/records/assignment-protocol.md), and nothing in
-- GitHub's messages says so, so billet asks GitHub's REST record of the run.
--
-- stranded is '' until decided, and then one of:
--   offered    GitHub offered the job again, or the run is over: nothing is stranded.
--   stranded   the job is still queued with no runner, long after the failure, while
--              its tier was advertising and waiting for nothing.
--   expired    a day passed without a decision; nobody is asked about it after that.
-- stranded_at is when it was decided. ON THE ROW, as the re-run decision is (migration
-- 74), so it is decided once whatever restarts or fails over, and retired with the
-- row.
--
-- THE INDEX IS OVER THE UNDECIDED LAUNCH FAILURES ALONE, the only rows the detector
-- reads, by when their lease ended.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN stranded TEXT NOT NULL DEFAULT ''
    CHECK (stranded IN ('', 'offered', 'stranded', 'expired'))
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN stranded_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_launch_miss_idx ON job_history(finished_at)
    WHERE failure_reason = 'billet:launch-failed' AND stranded = ''
-- +billet:end
