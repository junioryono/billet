-- migration 28: pool_runner_identity, for PostgreSQL
--
-- The twin of migrations/0028_pool_runner_identity.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE pool_runners (
			lease_id          text PRIMARY KEY CHECK (length(trim(lease_id)) > 0),
			tier              text NOT NULL CHECK (length(trim(tier)) > 0),
			launch_request_id bigint NOT NULL CHECK (launch_request_id != 0),
			runner_id         bigint NOT NULL DEFAULT 0 CHECK (runner_id >= 0),
			runner_name       text NOT NULL UNIQUE CHECK (length(trim(runner_name)) > 0),
			status            text NOT NULL CHECK (status IN ('idle','busy','retiring','retired')),
			actual_request_id bigint NOT NULL DEFAULT 0,
			run_id            bigint NOT NULL DEFAULT 0 CHECK (run_id >= 0),
			job_id            text NOT NULL DEFAULT '',
			source_acknowledged bigint NOT NULL DEFAULT 0 CHECK (source_acknowledged IN (0,1)),
			updated_at        text NOT NULL,
			UNIQUE (tier, launch_request_id)
		);
-- +billet:end

-- +billet:statement
CREATE INDEX pool_runners_tier_status_idx ON pool_runners(tier, status);
-- +billet:end
