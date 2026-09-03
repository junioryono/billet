-- migration 35: job_disruption, for PostgreSQL
--
-- The twin of migrations/0035_job_disruption.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN disruption text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN disrupted_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN disruption text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN disrupted_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN result text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN result_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_disrupted_idx ON job_history(result_at)
			WHERE disruption != '';
-- +billet:end
