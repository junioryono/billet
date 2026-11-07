-- migration 74: job_reruns
--
-- The twin of internal/state/migrations/0074_job_reruns.sql; the reasoning is there.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_reruns (
    repo text NOT NULL,
    run_id bigint NOT NULL CHECK (run_id > 0),
    run_attempt bigint NOT NULL CHECK (run_attempt > 0),
    kind text NOT NULL,
    github_job_id bigint NOT NULL DEFAULT 0 CHECK (github_job_id >= 0),
    state text NOT NULL,
    tries bigint NOT NULL DEFAULT 0 CHECK (tries >= 0),
    created_at text NOT NULL,
    settled_at text NOT NULL DEFAULT '',
    decision text NOT NULL DEFAULT '',
    detail text NOT NULL DEFAULT '',
    PRIMARY KEY (repo, run_id, run_attempt)
);
-- +billet:end

-- +billet:statement
CREATE TABLE rerun_tiers (
    tier text PRIMARY KEY NOT NULL,
    asked_since text NOT NULL
);
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN rerun_decision text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN rerun_decided_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN rerun_attempt bigint NOT NULL DEFAULT 0 CHECK (rerun_attempt >= 0);
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_rerun_decided_idx ON job_history(rerun_decided_at)
			WHERE rerun_decision != '';
-- +billet:end
