-- migration 50: listener_capacity
--
-- LISTENER OWNERSHIP IS NOT A LEASE PHASE. An acquiring lease can still be in
-- capacity, so status needs the listener's observation beside the ledger rows.
-- This record authorizes nothing and carries its observation time explicitly.

-- +billet:statement
CREATE TABLE listener_capacity (
    tier TEXT PRIMARY KEY NOT NULL,
    observed_at TEXT NOT NULL,
    snapshot TEXT NOT NULL
) STRICT
-- +billet:end
