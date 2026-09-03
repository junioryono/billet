-- migration 33: node_inventory, for PostgreSQL
--
-- The twin of migrations/0033_node_inventory.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE node_inventory (
			node        text PRIMARY KEY,
			node_epoch  bigint NOT NULL CHECK (node_epoch >= 0),
			received_at text NOT NULL,
			running     bigint NOT NULL CHECK (running >= 0)
		);
-- +billet:end
