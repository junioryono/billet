-- migration 71: node_cache_stores
--
-- WHAT EACH NODE'S BREAKER LAST SAID ABOUT ITS SITE'S STORE (#531), which `billet
-- status` says back. Telemetry like node_inventory (migration 33), and fenced the same
-- way: node_epoch is the node's durable epoch when the report arrived, so a report from
-- before the host reconnected is not read as its current word. A control plane that
-- restarts has the row, and a node re-sends at every registration.
--
-- A BREAKER DECIDES NOTHING HERE. Nothing but `billet status` reads this: placement,
-- capacity, custody and teardown do not, because what a store is doing says nothing
-- about compute.
--
-- since is the node's own clock, received_at this control plane's. failures and reason
-- say what opened it; a closed one carries neither. No store's own words are kept.
--
-- NO FOREIGN KEY ONTO nodes, because a restore from an archive older than this drops
-- this table by name, and no CHECK on the words, for the reason migration 35 gives. The
-- sets are closed in Go by lease.CacheStoreReport.Validate.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE node_cache_stores (
			node        TEXT NOT NULL,
			store       TEXT NOT NULL,
			state       TEXT NOT NULL,
			since       TEXT NOT NULL,
			failures    INTEGER NOT NULL DEFAULT 0 CHECK (failures >= 0),
			reason      TEXT NOT NULL DEFAULT '',
			node_epoch  INTEGER NOT NULL CHECK (node_epoch >= 0),
			received_at TEXT NOT NULL,
			PRIMARY KEY (node, store)
		) STRICT
-- +billet:end
