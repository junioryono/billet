-- migration 19: ec2_shape_accounting, for PostgreSQL
--
-- The twin of migrations/0019_ec2_shape_accounting.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN ec2_shapes text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN requested_vcpu bigint NOT NULL DEFAULT 0 CHECK (requested_vcpu >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN requested_memory bigint NOT NULL DEFAULT 0 CHECK (requested_memory >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN instance_type text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
UPDATE leases SET requested_vcpu = vcpu, requested_memory = memory
		  WHERE requested_vcpu = 0 OR requested_memory = 0;
-- +billet:end
