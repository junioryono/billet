-- migration 71: job_run_attempt (PostgreSQL)
--
-- See the SQLite migration of the same version for why. Zero is "not recorded",
-- never a first attempt.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_run_attempt bigint NOT NULL DEFAULT 0 CHECK (github_run_attempt >= 0);
-- +billet:end
