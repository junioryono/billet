-- migration 37: compute_barrier, for PostgreSQL
--
-- The twin of migrations/0037_compute_barrier.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN dispatch_generation bigint NOT NULL DEFAULT 0
			CHECK (dispatch_generation >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN decommissioned_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN decommission_proven bigint NOT NULL DEFAULT 0
			CHECK (decommission_proven IN (0, 1));
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN decommission_actor text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
CREATE TABLE compute_barrier (
			id                   bigint PRIMARY KEY CHECK (id = 1),
			barrier_id           text NOT NULL,
			admission_generation bigint NOT NULL CHECK (admission_generation >= 0),
			requested_at         text NOT NULL,
			requested_by         text NOT NULL
		);
-- +billet:end

-- +billet:statement
CREATE TABLE compute_barrier_nodes (
			node                text PRIMARY KEY,
			barrier_id          text NOT NULL,
			node_epoch          bigint NOT NULL CHECK (node_epoch >= 0),
			dispatch_generation bigint NOT NULL CHECK (dispatch_generation >= 0),
			empty_since         text NOT NULL,
			observed_at         text NOT NULL
		);
-- +billet:end
