-- migration 28: pool_runner_identity
--
-- A scale-set registration is a pool member rather than a job-bound runner.
-- Keep the compute lease separate from the job GitHub actually starts on it so
-- completion and scale-down never infer one identity from the other.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE pool_runners (
			lease_id          TEXT PRIMARY KEY CHECK (length(trim(lease_id)) > 0),
			tier              TEXT NOT NULL CHECK (length(trim(tier)) > 0),
			launch_request_id INTEGER NOT NULL CHECK (launch_request_id != 0),
			runner_id         INTEGER NOT NULL DEFAULT 0 CHECK (runner_id >= 0),
			runner_name       TEXT NOT NULL UNIQUE CHECK (length(trim(runner_name)) > 0),
			status            TEXT NOT NULL CHECK (status IN ('idle','busy','retiring','retired')),
			actual_request_id INTEGER NOT NULL DEFAULT 0,
			run_id            INTEGER NOT NULL DEFAULT 0 CHECK (run_id >= 0),
			job_id            TEXT NOT NULL DEFAULT '',
			source_acknowledged INTEGER NOT NULL DEFAULT 0 CHECK (source_acknowledged IN (0,1)),
			updated_at        TEXT NOT NULL,
			UNIQUE (tier, launch_request_id)
		) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX pool_runners_tier_status_idx ON pool_runners(tier, status)
-- +billet:end
