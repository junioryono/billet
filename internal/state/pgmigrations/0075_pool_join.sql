-- migration 75: pool_join, for PostgreSQL
--
-- The twin of migrations/0075_pool_join.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE join_tokens ADD COLUMN pool_order text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
CREATE INDEX join_tokens_pool_order_idx ON join_tokens (pool_order) WHERE pool_order != '';
-- +billet:end

-- +billet:statement
ALTER TABLE node_enrollments ADD COLUMN pool_order text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN node_fingerprint text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN node_cert_serial text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN join_hold text NOT NULL DEFAULT '';
-- +billet:end
