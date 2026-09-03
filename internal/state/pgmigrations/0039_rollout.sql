-- migration 39: rollout, for PostgreSQL
--
-- The twin of migrations/0039_rollout.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE rollouts (
			id               text PRIMARY KEY CHECK (length(trim(id)) > 0),
			generation       bigint NOT NULL CHECK (generation > 0),
			channel          text NOT NULL,
			target_version   text NOT NULL CHECK (length(trim(target_version)) > 0),
			target_digest    text NOT NULL CHECK (length(target_digest) = 64),
			policy           text NOT NULL,
			controller_phase text NOT NULL,
			prior_version    text NOT NULL,
			state            text NOT NULL CHECK (state IN ('open','completed','aborted')),
			created_by       text NOT NULL,
			created_at       text NOT NULL,
			finished_at      text NOT NULL,
			terminal_reason  text NOT NULL
		);
-- +billet:end

-- +billet:statement
CREATE UNIQUE INDEX rollouts_open ON rollouts (state) WHERE state = 'open';
-- +billet:end

-- +billet:statement
CREATE TABLE rollout_nodes (
			rollout_id      text NOT NULL,
			node            text NOT NULL,
			phase           text NOT NULL,
			attempts        bigint NOT NULL DEFAULT 0 CHECK (attempts >= 0),
			next_attempt_at text NOT NULL,
			blocker         text NOT NULL,
			prior_release   text NOT NULL,
			rollback_result text NOT NULL,
			exempt_reason   text NOT NULL,
			updated_at      text NOT NULL,
			PRIMARY KEY (rollout_id, node)
		);
-- +billet:end

-- +billet:statement
CREATE INDEX rollout_nodes_phase ON rollout_nodes (rollout_id, phase);
-- +billet:end
