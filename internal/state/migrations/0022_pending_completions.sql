-- migration 22: pending_completions
--
-- A completed scale-set message is acknowledged only after its authoritative
-- result can survive the control plane stopping before node teardown succeeds.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE pending_completions (
			tier       TEXT NOT NULL CHECK (length(trim(tier)) > 0),
			request_id INTEGER NOT NULL CHECK (request_id > 0),
			run_id     INTEGER NOT NULL DEFAULT 0 CHECK (run_id >= 0),
			result     TEXT NOT NULL CHECK (length(trim(result)) > 0),
			PRIMARY KEY (tier, request_id)
		) STRICT
-- +billet:end
