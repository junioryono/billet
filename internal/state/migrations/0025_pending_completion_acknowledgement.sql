-- migration 25: pending_completion_acknowledgement
--
-- A retired completion must outlive its source message, but not every later
-- restart. Persisting acknowledgement lets either ordering of settlement and
-- source deletion remove the tombstone only after both facts are durable.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN acknowledged INTEGER NOT NULL DEFAULT 0 CHECK (acknowledged IN (0,1))
-- +billet:end
