-- migration 21: lease_failure_reason
--
-- An external reclaim is known before the guest disappears. Record that fact on
-- the live lease so a node restart between the warning and teardown cannot turn
-- a known failed build into an unattributed completion.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN failure_reason TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN failure_reason TEXT NOT NULL DEFAULT ''
-- +billet:end
