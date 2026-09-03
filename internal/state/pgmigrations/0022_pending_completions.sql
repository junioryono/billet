-- migration 22: pending_completions, for PostgreSQL
--
-- The twin of migrations/0022_pending_completions.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE pending_completions (
			tier       text NOT NULL CHECK (length(trim(tier)) > 0),
			request_id bigint NOT NULL CHECK (request_id > 0),
			run_id     bigint NOT NULL DEFAULT 0 CHECK (run_id >= 0),
			result     text NOT NULL CHECK (length(trim(result)) > 0),
			PRIMARY KEY (tier, request_id)
		);
-- +billet:end
