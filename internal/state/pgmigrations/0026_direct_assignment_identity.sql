-- migration 26: direct_assignment_identity, for PostgreSQL
--
-- The twin of migrations/0026_direct_assignment_identity.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE job_identities (
			job_id      text PRIMARY KEY CHECK (length(trim(job_id)) > 0),
			internal_id bigint NOT NULL UNIQUE CHECK (internal_id < 0)
		);
-- +billet:end

-- +billet:statement
CREATE TABLE pending_completions_new (
			tier         text NOT NULL CHECK (length(trim(tier)) > 0),
			request_id   bigint NOT NULL CHECK (request_id != 0),
			run_id       bigint NOT NULL DEFAULT 0 CHECK (run_id >= 0),
			result       text NOT NULL CHECK (length(trim(result)) > 0),
			lease_id     text NOT NULL DEFAULT '',
			lease_epoch  bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
			outcome      text NOT NULL DEFAULT '' CHECK (outcome IN ('','done','failed')),
			release_only bigint NOT NULL DEFAULT 0 CHECK (release_only IN (0,1)),
			lease_node   text NOT NULL DEFAULT '',
			message_id   bigint NOT NULL DEFAULT 0 CHECK (message_id >= 0),
			retired      bigint NOT NULL DEFAULT 0 CHECK (retired IN (0,1)),
			acknowledged bigint NOT NULL DEFAULT 0 CHECK (acknowledged IN (0,1)),
			PRIMARY KEY (tier, request_id)
		);
-- +billet:end

-- +billet:statement
INSERT INTO pending_completions_new
		   (tier, request_id, run_id, result, lease_id, lease_epoch, outcome,
		    release_only, lease_node, message_id, retired, acknowledged)
		 SELECT tier, request_id, run_id, result, lease_id, lease_epoch, outcome,
		        release_only, lease_node, message_id, retired, acknowledged
		   FROM pending_completions;
-- +billet:end

-- +billet:statement
DROP TABLE pending_completions;
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions_new RENAME TO pending_completions;
-- +billet:end
