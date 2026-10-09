-- migration 58: job_counters, for PostgreSQL
--
-- The twin of migrations/0058_job_counters.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN cycles bigint;
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN instructions bigint;
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN cache_references bigint;
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN cache_misses bigint;
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN branch_misses bigint;
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN frontend_stall_cycles bigint;
-- +billet:end
