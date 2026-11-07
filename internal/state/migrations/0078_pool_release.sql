-- migration 78: pool_release
--
-- A POOL HOST IS RELEASED ONLY ON PROOF (ADR-030, #553), and the proof waits for the
-- host to drain: a person asks for the release, the host is taken out of placement at
-- once, and the order is sealed, its node's certificates revoked and the source asked
-- to take the host back only once the ledger shows it holds nothing.
--
-- release_requested_at is when the release was asked for, empty until it is; it is
-- the moment placement stopped, so an inventory the node reports after it is one
-- nothing could have been placed behind. It never moves back: a release once asked
-- for is not withdrawn, and a node's registration keeps the host out of placement
-- while it stands.
--
-- release_reason is what the person who asked gave, for `billet pool` and the log.
--
-- release_contradicted is 1 once the order's node has said, since its last decommission
-- on the compute barrier's proof, that it is back or runs something: it arrived to
-- register (refused or not), or an inventory of its current registration reported
-- compute. Only a fresh decommission on proof sets it back to 0, in that decommission's
-- own transaction, so the order of the two is the ledger's and never a clock's. It is
-- the release's alone: the decommission itself, which the drain's clearance and a
-- CodeBuild fleet's claim read, is left as the person recorded it.
--
-- AN ORDER LINKED BEFORE THIS MIGRATION HAS NO SUCH HISTORY, and none can be read back:
-- a billet before it recorded nothing durable when a decommissioned node came back or
-- reported compute. So every linked order a release could still seal starts contradicted,
-- and its node's idle run is discarded with it, so its release waits for a decommission
-- proved by answers taken after this.
--
-- The states this uses (sealed, released, gone) were listed by migration 75.

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN release_requested_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN release_reason TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN release_contradicted INTEGER NOT NULL DEFAULT 0 CHECK (release_contradicted IN (0, 1))
-- +billet:end

-- +billet:statement
UPDATE pool_orders SET release_contradicted = 1 WHERE node_name != '' AND state IN ('placed', 'sealed')
-- +billet:end

-- +billet:statement
DELETE FROM compute_barrier_nodes
 WHERE node IN (SELECT node_name FROM pool_orders WHERE node_name != '' AND state IN ('placed', 'sealed'))
-- +billet:end
