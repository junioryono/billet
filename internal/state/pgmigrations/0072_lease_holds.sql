-- migration 72: lease_holds, for PostgreSQL
--
-- The twin of migrations/0072_lease_holds.sql, which says why. Everything between the
-- markers below is PUBLISHED BYTES; the prose is not. Reformat one tab and every ledger
-- that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE lease_holds (
    lease_id text PRIMARY KEY NOT NULL,
    tier text NOT NULL,
    held_at text NOT NULL,
    until_at text NOT NULL,
    ended_at text NOT NULL DEFAULT '',
    ended_by text NOT NULL DEFAULT ''
);
-- +billet:end

-- +billet:statement
CREATE INDEX lease_holds_holding ON lease_holds (until_at) WHERE ended_at = '';
-- +billet:end
