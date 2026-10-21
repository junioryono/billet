-- migration 65: job_phase_times
--
-- WHEN EACH PART OF A JOB'S LIFE BEGAN, KEPT WHERE AN OPERATOR CAN READ IT. A
-- lease records only its current phase, and the row is reaped once it ends, so
-- how long a job spent launching, registering its runner or being torn down,
-- and which node process held it, was answerable from no table once the job was
-- over (#28). job_history outlives the lease, and is what `billet jobs show`
-- reads.
--
-- launched_at, custody_at, teardown_at and quarantined_at are when the lease
-- first entered that phase, on billet's clock; registered_at is when the plane
-- journaled the runner it minted, and runner_id is GitHub's id for it. Each is
-- written once, in the transaction that makes the move. launch_incarnation is
-- the node process that bound the lease and began the launch, and
-- holder_incarnation the process that held it when it ended, copied from the
-- lease as it terminalizes. Empty or zero means not recorded: the phase was
-- never entered, or the row was written before this migration.
--
-- The indexes serve the lookup by the run GitHub shows, which matches either the
-- run that started or, for a row with none, the run the lease was assigned.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN launched_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN registered_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN runner_id INTEGER NOT NULL DEFAULT 0 CHECK (runner_id >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN custody_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN teardown_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN quarantined_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN launch_incarnation TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN holder_incarnation TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_github_run_idx ON job_history (github_run_id)
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_run_idx ON job_history (run_id)
-- +billet:end
