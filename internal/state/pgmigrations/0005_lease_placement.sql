-- migration 5: lease_placement, for PostgreSQL
--
-- The twin of migrations/0005_lease_placement.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN target_node text;
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN macos_slot bigint NOT NULL DEFAULT 0 CHECK (macos_slot IN (0, 1));
-- +billet:end

-- +billet:statement
CREATE INDEX leases_expiry_idx ON leases(expires_at) WHERE phase NOT IN ('done','failed');
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN assigned_at text;
-- +billet:end
