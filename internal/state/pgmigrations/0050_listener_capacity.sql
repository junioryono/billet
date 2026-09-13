-- migration 50: listener_capacity, for PostgreSQL
--
-- The twin of migrations/0050_listener_capacity.sql.

-- +billet:statement
CREATE TABLE listener_capacity (
    tier text PRIMARY KEY NOT NULL,
    observed_at text NOT NULL,
    snapshot text NOT NULL
);
-- +billet:end
