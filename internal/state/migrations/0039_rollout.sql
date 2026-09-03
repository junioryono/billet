-- migration 39: rollout
--
-- rolloutMigration is one durable fleet decision and where every component has
-- got to.
--
-- ONE DECISION, NOT A SETTING PER HOST. Updating billet has to be a thing an
-- operator does once: a channel is resolved to one immutable target, that target
-- is recorded, and every controller and node converges on it without another
-- version edit. A per-host desired version is the shape that makes an upgrade a
-- dozen decisions, each of which can be made differently.
--
-- THE TARGET IS A DIGEST, and that is what makes a restart safe. A channel moves;
-- a rollout must not. Recording the version alone would let a channel advancing
-- mid-rollout retarget work already underway, so the manifest's digest is the
-- identity and the version is what an operator reads.
--
-- AT MOST ONE ROLLOUT IS OPEN, enforced by a partial unique index rather than by
-- a check in Go — for the same reason the force-destroy request is: two rollouts
-- would each be draining hosts the other was installing on, and neither record
-- would describe what happened.
--
-- generation FENCES WHAT REACHES A NODE. An instruction carries it, and a node
-- that has already acted on a newer one refuses an older delivery rather than
-- installing a release the rollout has moved past. Repeated or delayed
-- instructions are idempotent on it.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE rollouts (
			id               TEXT PRIMARY KEY CHECK (length(trim(id)) > 0),
			generation       INTEGER NOT NULL CHECK (generation > 0),
			channel          TEXT NOT NULL,
			target_version   TEXT NOT NULL CHECK (length(trim(target_version)) > 0),
			target_digest    TEXT NOT NULL CHECK (length(target_digest) = 64),
			policy           TEXT NOT NULL,
			controller_phase TEXT NOT NULL,
			prior_version    TEXT NOT NULL,
			state            TEXT NOT NULL CHECK (state IN ('open','completed','aborted')),
			created_by       TEXT NOT NULL,
			created_at       TEXT NOT NULL,
			finished_at      TEXT NOT NULL,
			terminal_reason  TEXT NOT NULL
		) STRICT
-- +billet:end

-- +billet:statement
CREATE UNIQUE INDEX rollouts_open ON rollouts (state) WHERE state = 'open'
-- +billet:end

-- +billet:statement
CREATE TABLE rollout_nodes (
			rollout_id      TEXT NOT NULL,
			node            TEXT NOT NULL,
			phase           TEXT NOT NULL,
			attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
			next_attempt_at TEXT NOT NULL,
			blocker         TEXT NOT NULL,
			prior_release   TEXT NOT NULL,
			rollback_result TEXT NOT NULL,
			exempt_reason   TEXT NOT NULL,
			updated_at      TEXT NOT NULL,
			PRIMARY KEY (rollout_id, node)
		) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX rollout_nodes_phase ON rollout_nodes (rollout_id, phase)
-- +billet:end
