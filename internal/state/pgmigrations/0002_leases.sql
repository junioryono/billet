-- migration 2: leases, for PostgreSQL
--
-- The twin of migrations/0002_leases.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE leases (
				id           text PRIMARY KEY,
				tier         text NOT NULL,
				node         text REFERENCES nodes(name) ON DELETE SET NULL,
				phase        text NOT NULL CHECK (phase IN
					('capacity','assigned','launching','online','busy','done','failed')),
				vcpu         bigint NOT NULL CHECK (vcpu > 0),
				memory       bigint NOT NULL CHECK (memory > 0),
				run_id       bigint,
				job_id       bigint,
				epoch        bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
				created_at   text NOT NULL,
				heartbeat_at text NOT NULL,
				expires_at   text NOT NULL
			);
-- +billet:end

-- +billet:statement
CREATE INDEX leases_open_idx ON leases(phase) WHERE phase NOT IN ('done','failed');
-- +billet:end

-- +billet:statement
CREATE INDEX leases_node_idx ON leases(node);
-- +billet:end
