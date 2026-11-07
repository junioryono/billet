-- migration 77: pool_release, for PostgreSQL
--
-- The twin of migrations/0077_pool_release.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN release_requested_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN release_reason text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN release_contradicted bigint NOT NULL DEFAULT 0 CHECK (release_contradicted IN (0, 1));
-- +billet:end

-- +billet:statement
UPDATE pool_orders SET release_contradicted = 1 WHERE node_name != '' AND state IN ('placed', 'sealed');
-- +billet:end

-- +billet:statement
DELETE FROM compute_barrier_nodes
 WHERE node IN (SELECT node_name FROM pool_orders WHERE node_name != '' AND state IN ('placed', 'sealed'));
-- +billet:end
