-- migration 33: node_inventory
--
-- nodeInventoryMigration records what each host last SAID it was running.
--
-- EVIDENCE, NOT PROOF, and the schema is shaped so it cannot quietly become the
-- latter. It is a separate table from `nodes` because a node row is
-- authoritative — what the deployment believes about a host — while this is
-- telemetry: one host's own last word, already stale when it arrives.
--
-- `received_at` is named for what the control plane actually knows. The node
-- lists its provider and THEN posts the result, so the server learns when the
-- report arrived and never when the snapshot was taken. A record that called
-- itself observed_at would be claiming an ordering the protocol never
-- establishes, and exactly that mistake sank an earlier design.
--
-- node_epoch is the fence. It is the durable epoch from `nodes`, bumped on
-- every accepted registration, rather than the plane's in-memory incarnation:
-- a control plane that restarts while the node process keeps running would
-- otherwise carry a record forward as if it were current.
--
-- running is a COUNT of billet-recognised instances in that snapshot, and it is
-- only ever written on the path where the host VOUCHED for its list — the
-- quarantine resolver refuses to act on an inventory a node could not read, and
-- this is written inside that same transaction. So there is no sentinel here
-- for "could not tell": a host that cannot see its provider sends nothing, its
-- previous row ages visibly, and a reconnect makes the row stale by epoch.
--
-- An earlier draft of this schema documented -1 as that sentinel. Nothing ever
-- wrote it, which is a comment describing behaviour the code does not have.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE node_inventory (
			node        TEXT PRIMARY KEY,
			node_epoch  INTEGER NOT NULL CHECK (node_epoch >= 0),
			received_at TEXT NOT NULL,
			running     INTEGER NOT NULL CHECK (running >= 0)
		) STRICT
-- +billet:end
