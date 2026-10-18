-- migration 62: job_github_run
--
-- THE RUN THE JOB THAT STARTED BELONGS TO. job_history.run_id is the run of the
-- job a lease was assigned, and a pooled runner is launched for one job and may
-- be given another, of another run, by GitHub; the binding that names the run
-- that started (pool_runners.run_id) is deleted once the runner's compute is
-- gone. A reader of GitHub's REST record of the job, which is where GitHub's
-- queue time comes from now that no scale-set message carries it (#525), needs
-- the run that started for as long as the row is read.
--
-- github_run_id is the JobStarted message's own workflow run, written once with
-- the rest of the job's identity. Zero means not recorded, which every row from
-- before this reads as.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_run_id INTEGER NOT NULL DEFAULT 0 CHECK (github_run_id >= 0)
-- +billet:end
