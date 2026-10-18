-- migration 61: job_queue_times
--
-- HOW LONG A JOB WAITED, AND WHERE. job_history.queued_at is billet's own
-- clock at assignment, equal to assigned_at, so the time a job sat at GitHub
-- before billet heard of it was recorded nowhere, and a fleet with no room
-- could not be told from a slow launch (#523, #525).
--
-- github_queued_at, github_scale_set_assigned_at and github_runner_assigned_at
-- are what GitHub's own scale-set message said (JobMessageBase.QueueTime,
-- ScaleSetAssignTime and RunnerAssignTime), on GitHub's clock, written once
-- from the message that names the job the lease ran: JobStarted, or a
-- completion filling what it did not. bought_at is when billet bought the
-- lease that ran it (leases.created_at), copied when the row opens, because
-- the lease row is reaped and the history row is what an operator reads.
--
-- The empty string means not recorded, which every row from before this
-- reads as: a job assigned before these columns existed has an unknown wait,
-- never a zero one. queued_at keeps its published meaning.
--
-- The index serves the report, which is windowed on when jobs started.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_queued_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_scale_set_assigned_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_runner_assigned_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN bought_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_started_idx ON job_history(started_at)
-- +billet:end
