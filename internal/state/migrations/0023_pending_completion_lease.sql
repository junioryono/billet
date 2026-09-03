-- migration 23: pending_completion_lease
--
-- A result alone can replay node teardown, but it cannot return capacity when
-- the control plane stops after teardown succeeds and before the lease release
-- settles. Keep the fenced lease identity beside the result so restart recovery
-- can finish both halves of completion in their required order.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN lease_id TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN lease_epoch INTEGER NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN outcome TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','done','failed'))
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN release_only INTEGER NOT NULL DEFAULT 0 CHECK (release_only IN (0,1))
-- +billet:end
