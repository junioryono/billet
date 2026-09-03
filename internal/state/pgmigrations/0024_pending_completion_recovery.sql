-- migration 24: pending_completion_recovery, for PostgreSQL
--
-- The twin of migrations/0024_pending_completion_recovery.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN lease_node text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
UPDATE pending_completions
		    SET lease_node = COALESCE((SELECT COALESCE(node, target_node, '') FROM leases
		                               WHERE leases.id = pending_completions.lease_id), '')
		  WHERE lease_id != '';
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN message_id bigint NOT NULL DEFAULT 0 CHECK (message_id >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN retired bigint NOT NULL DEFAULT 0 CHECK (retired IN (0,1));
-- +billet:end
