-- migration 8: lease_request_id
--
-- requestIDMigration renames job_id to what it actually holds.
--
-- The column was written before anything consumed GitHub's scale-set API, and
-- the API disagrees with it twice over. What billet needs to record is
-- RunnerRequestID — that is the identity AcquireJobs claims work by, and the
-- only one that makes a redelivered message idempotent. GitHub's own JobID is a
-- separate field and is a STRING, so a column named job_id holding an int64 was
-- storing the wrong value under a name that would later look right to whoever
-- tried to correlate a lease with GitHub's API.
--
-- Renamed rather than left alone because the trap is silent: `job_id INTEGER`
-- accepts the request id without complaint, and SQLite's affinity rules would
-- even accept the GUID. The failure surfaces only when a human reads it.
--
-- RENAME COLUMN needs SQLite 3.25+; modernc.org/sqlite is far past that.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases RENAME COLUMN job_id TO request_id
-- +billet:end

-- +billet:statement
ALTER TABLE job_history RENAME COLUMN job_id TO request_id
-- +billet:end
