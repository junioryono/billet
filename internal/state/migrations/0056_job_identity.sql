-- migration 56: job_identity
--
-- WHO THE JOB WAS, ON THE ROW THAT OUTLIVES ITS LEASE. job_history said which
-- tier and host a lease used and what GitHub concluded, but not which of
-- GitHub's jobs it ran: request_id is the runner request, run_id the workflow
-- run, and the `repo` column migration 4 declared was never written. A per-job
-- measurement nobody can join back to a repository, a workflow and a job name
-- answers no question an operator asks.
--
-- github_job_id is GitHub's workflow-job id (JobMessageBase.JobID), the key a
-- guest-side step reader and the Actions API are joined on later. workflow_ref
-- is the job's workflow ref verbatim; the scale-set message carries no workflow
-- name, so a reader derives the file from the ref. job_name is the job's
-- display name and event the event that queued it.
--
-- EVERY VALUE IS WHAT GITHUB'S OWN MESSAGE SAID, and each is written once and
-- kept: a pooled runner learns its job on JobStarted, a direct assignment at
-- assignment, and a completion only fills what neither recorded. The empty
-- string means not recorded, which is what every row from before this reads as.
-- None of it decides anything; it is for people to read.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_job_id TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN workflow_ref TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN job_name TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN event TEXT NOT NULL DEFAULT ''
-- +billet:end
