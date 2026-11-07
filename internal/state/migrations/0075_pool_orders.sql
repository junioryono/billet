-- migration 75: pool_orders
--
-- WHAT A POOL OF RENTED BARE METAL ORDERED, AND WHAT BECAME OF IT (ADR-030, #553).
--
-- One row per order, written BEFORE the source is asked, so an order whose answer is
-- lost is read back under its identifier rather than placed a second time. The row is
-- the host once the source names one (host_id), and the node once one joins from it
-- (node_name).
--
-- state is the order's whole life, and every move is one way:
--   ordering   the row is committed and the source has been, or is being, asked
--   placed     the source named the host
--   uncertain  the answer was lost and the source can no longer deduplicate a resend;
--              a person decides
--   refused    the source refused (no stock, quota, or a refusal an operator acts on)
--   sealed     a release is sealed: nothing is placed on the host again
--   released   the source was asked to take the host back
--   gone       the source said, in its own terminal answer, that the host is gone
--   abandoned  a person closed an uncertain order
-- The states a later change uses are listed now because a CHECK constraint cannot
-- change without rebuilding the table, and a published migration is frozen.
--
-- scope is where the source orders, as it was when the order was placed (an AWS
-- region, subnet and zone): a lost order is read back and sent again only where it
-- was placed, never wherever a changed config points now.
--
-- first_attempt_at bounds a resend: a source deduplicates an order for a window from
-- its first attempt (hostsource.Terms.IdempotentFor), and past it a lost answer is
-- uncertain.

-- +billet:statement
CREATE TABLE pool_orders (
    order_id TEXT PRIMARY KEY NOT NULL,
    source TEXT NOT NULL,
    scope TEXT NOT NULL,
    shape TEXT NOT NULL,
    location TEXT NOT NULL DEFAULT '',
    arch TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('ordering', 'placed', 'uncertain', 'refused', 'sealed', 'released', 'gone', 'abandoned')),
    host_id TEXT NOT NULL DEFAULT '',
    node_name TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    first_attempt_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX pool_orders_state_idx ON pool_orders (state)
-- +billet:end
