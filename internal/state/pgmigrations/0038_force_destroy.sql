-- migration 38: force_destroy, for PostgreSQL
--
-- The twin of migrations/0038_force_destroy.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE force_destroy (
			generation           bigint PRIMARY KEY CHECK (generation > 0),
			admission_generation bigint NOT NULL CHECK (admission_generation >= 0),
			state                text NOT NULL CHECK (state IN ('requested','completed')),
			reason               text NOT NULL,
			actor                text NOT NULL,
			requested_at         text NOT NULL,
			completed_at         text NOT NULL
		);
-- +billet:end

-- +billet:statement
CREATE UNIQUE INDEX force_destroy_open
		     ON force_destroy (state) WHERE state = 'requested';
-- +billet:end

-- +billet:statement
CREATE TABLE force_destroy_target (
			generation        bigint NOT NULL,
			lease_id          text NOT NULL CHECK (length(trim(lease_id)) > 0),
			tier              text NOT NULL,
			node              text NOT NULL,
			run_id            text NOT NULL,
			scheduler_request bigint NOT NULL,
			phase             text NOT NULL,
			state             text NOT NULL CHECK (state IN ('pending','destroyed','failed')),
			detail            text NOT NULL,
			PRIMARY KEY (generation, lease_id)
		);
-- +billet:end

-- +billet:statement
CREATE INDEX force_destroy_target_pending
		     ON force_destroy_target (state, tier);
-- +billet:end
