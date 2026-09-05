-- migration 49: job_placement_and_cache, for PostgreSQL
--
-- The twin of migrations/0049_job_placement_and_cache.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN site text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN price_micros_per_hour bigint NOT NULL DEFAULT 0 CHECK (price_micros_per_hour >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN image_cache text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN cache_generation text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN actions_cache text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN chosen_provider text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN instance_type text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN vcpu bigint NOT NULL DEFAULT 0 CHECK (vcpu >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN memory bigint NOT NULL DEFAULT 0 CHECK (memory >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN site text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN price_micros_per_hour bigint NOT NULL DEFAULT 0 CHECK (price_micros_per_hour >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN image_cache text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN cache_generation text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN actions_cache text NOT NULL DEFAULT '';
-- +billet:end
