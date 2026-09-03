-- migration 8: lease_request_id, for PostgreSQL
--
-- The twin of migrations/0008_lease_request_id.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases RENAME COLUMN job_id TO request_id;
-- +billet:end

-- +billet:statement
ALTER TABLE job_history RENAME COLUMN job_id TO request_id;
-- +billet:end
