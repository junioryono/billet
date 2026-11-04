-- migration 71: job_test_results, for PostgreSQL
--
-- The twin of migrations/0071_job_test_results.sql, which says why. Everything
-- between the markers below is PUBLISHED BYTES; the prose is not. Reformat one tab
-- and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN head_sha text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN head_branch text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN run_attempt bigint NOT NULL DEFAULT 0 CHECK (run_attempt >= 0);
-- +billet:end

-- +billet:statement
CREATE TABLE job_test_results (
    lease_id text PRIMARY KEY NOT NULL,
    recorded_at text NOT NULL,
    codec bigint NOT NULL CHECK (codec >= 1),
    encoded_bytes bigint NOT NULL CHECK (encoded_bytes >= 0),
    data text NOT NULL
);
-- +billet:end

-- +billet:statement
CREATE INDEX job_test_results_recorded_at ON job_test_results (recorded_at, lease_id);
-- +billet:end
