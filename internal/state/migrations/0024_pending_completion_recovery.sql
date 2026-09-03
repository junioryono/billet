-- migration 24: pending_completion_recovery
--
-- Completion recovery needs both the bound host and a monotonic delivery phase.
-- The host keeps restart reconciliation from treating an unrelated live fleet as
-- proof of absence. The message id distinguishes a redelivery from a later job
-- that reuses GitHub's request id, and retired is the durable tombstone that makes
-- a failed row deletion harmless.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN lease_node TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
UPDATE pending_completions
		    SET lease_node = COALESCE((SELECT COALESCE(node, target_node, '') FROM leases
		                               WHERE leases.id = pending_completions.lease_id), '')
		  WHERE lease_id != ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN message_id INTEGER NOT NULL DEFAULT 0 CHECK (message_id >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN retired INTEGER NOT NULL DEFAULT 0 CHECK (retired IN (0,1))
-- +billet:end
