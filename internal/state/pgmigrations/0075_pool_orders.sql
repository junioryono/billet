-- migration 75: pool_orders, for PostgreSQL
--
-- The twin of migrations/0075_pool_orders.sql.

-- +billet:statement
CREATE TABLE pool_orders (
    order_id text PRIMARY KEY NOT NULL,
    source text NOT NULL,
    scope text NOT NULL,
    shape text NOT NULL,
    location text NOT NULL DEFAULT '',
    arch text NOT NULL,
    state text NOT NULL CHECK (state IN ('ordering', 'placed', 'uncertain', 'refused', 'sealed', 'released', 'gone', 'abandoned')),
    host_id text NOT NULL DEFAULT '',
    node_name text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    first_attempt_at text NOT NULL,
    updated_at text NOT NULL
);
-- +billet:end

-- +billet:statement
CREATE INDEX pool_orders_state_idx ON pool_orders (state);
-- +billet:end
