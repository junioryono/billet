-- migration 80: pool_clearance, for PostgreSQL
--
-- The twin of migrations/0080_pool_clearance.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_epoch bigint NOT NULL DEFAULT 0 CHECK (clear_epoch >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_dispatch bigint NOT NULL DEFAULT 0 CHECK (clear_dispatch >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_since text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_observed_at text NOT NULL DEFAULT '';
-- +billet:end
