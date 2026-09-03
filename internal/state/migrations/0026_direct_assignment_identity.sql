-- migration 26: direct_assignment_identity
--
-- GitHub assigns runnerRequestId 0 when it sends JobAssigned directly instead
-- of first offering JobAvailable. A durable negative identity, keyed by jobId,
-- keeps those jobs distinct without colliding with GitHub's positive request ids.
-- Pending completions accept either namespace but continue to refuse zero, which
-- remains the unusable wire value rather than a scheduler identity.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_identities (
			job_id      TEXT PRIMARY KEY CHECK (length(trim(job_id)) > 0),
			internal_id INTEGER NOT NULL UNIQUE CHECK (internal_id < 0)
		) STRICT
-- +billet:end

-- +billet:statement
CREATE TABLE pending_completions_new (
			tier         TEXT NOT NULL CHECK (length(trim(tier)) > 0),
			request_id   INTEGER NOT NULL CHECK (request_id != 0),
			run_id       INTEGER NOT NULL DEFAULT 0 CHECK (run_id >= 0),
			result       TEXT NOT NULL CHECK (length(trim(result)) > 0),
			lease_id     TEXT NOT NULL DEFAULT '',
			lease_epoch  INTEGER NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
			outcome      TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','done','failed')),
			release_only INTEGER NOT NULL DEFAULT 0 CHECK (release_only IN (0,1)),
			lease_node   TEXT NOT NULL DEFAULT '',
			message_id   INTEGER NOT NULL DEFAULT 0 CHECK (message_id >= 0),
			retired      INTEGER NOT NULL DEFAULT 0 CHECK (retired IN (0,1)),
			acknowledged INTEGER NOT NULL DEFAULT 0 CHECK (acknowledged IN (0,1)),
			PRIMARY KEY (tier, request_id)
		) STRICT
-- +billet:end

-- +billet:statement
INSERT INTO pending_completions_new
		   (tier, request_id, run_id, result, lease_id, lease_epoch, outcome,
		    release_only, lease_node, message_id, retired, acknowledged)
		 SELECT tier, request_id, run_id, result, lease_id, lease_epoch, outcome,
		        release_only, lease_node, message_id, retired, acknowledged
		   FROM pending_completions
-- +billet:end

-- +billet:statement
DROP TABLE pending_completions
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions_new RENAME TO pending_completions
-- +billet:end
