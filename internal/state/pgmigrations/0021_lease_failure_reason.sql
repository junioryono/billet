-- migration 21: lease_failure_reason, for PostgreSQL
--
-- The twin of migrations/0021_lease_failure_reason.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN failure_reason text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN failure_reason text NOT NULL DEFAULT '';
-- +billet:end
