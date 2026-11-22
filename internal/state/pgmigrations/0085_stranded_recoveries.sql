-- migration 85: stranded_recoveries
--
-- The twin of internal/state/migrations/0085_stranded_recoveries.sql; the reasoning is there.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE stranded_recoveries (
    repo text NOT NULL,
    run_id bigint NOT NULL CHECK (run_id > 0),
    run_attempt bigint NOT NULL CHECK (run_attempt > 0),
    tier text NOT NULL,
    lease_id text NOT NULL,
    job_name text NOT NULL,
    state text NOT NULL,
    tries bigint NOT NULL DEFAULT 0 CHECK (tries >= 0),
    created_at text NOT NULL,
    settled_at text NOT NULL DEFAULT '',
    detail text NOT NULL DEFAULT '',
    PRIMARY KEY (repo, run_id, run_attempt)
);
-- +billet:end

-- +billet:statement
CREATE INDEX stranded_recoveries_open_idx ON stranded_recoveries(created_at)
    WHERE state IN ('cancelling', 'rerunning');
-- +billet:end

-- +billet:statement
CREATE TABLE stranded_tiers (
    tier text PRIMARY KEY NOT NULL,
    asked_since text NOT NULL
);
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_stranded_idx ON job_history(stranded_at)
    WHERE stranded = 'stranded';
-- +billet:end
