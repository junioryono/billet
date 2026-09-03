-- migration 14: join_tokens
--
-- ENROLLING HAS TO COST SOMETHING TO ATTEMPT, or the request endpoint is open
-- to anyone who can reach the port.
--
-- Approval still cannot be tricked — an operator matches a fingerprint against
-- what the node printed — but an unauthenticated endpoint lets a stranger fill
-- the pending list with plausible entries, and take a NAME before the real
-- machine asks for it. "First key claims the name" protects an operator from
-- approving a substitute; without a credential in front of it, it also lets
-- somebody deny a machine its own name.
--
-- HASHED, NEVER STORED. A join token is a credential, so the ledger keeps only
-- what is needed to recognise one — the same reason a password is not stored.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE join_tokens (
			token_sha256   TEXT PRIMARY KEY,
			note           TEXT NOT NULL DEFAULT '',
			uses_remaining INTEGER NOT NULL,
			created_at     TEXT NOT NULL,
			expires_at     TEXT NOT NULL
		)
-- +billet:end

-- A certificate handed out by `billet ca issue` is an admission too, and
-- it left no record: nothing could answer "what has been let into this
-- deployment, and when".
-- +billet:statement
ALTER TABLE node_enrollments ADD COLUMN source TEXT NOT NULL DEFAULT 'enrolled'
-- +billet:end
