-- migration 1: nodes, for PostgreSQL
--
-- The twin of migrations/0001_nodes.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE nodes (
				name          text PRIMARY KEY,
				provider      text NOT NULL,
				-- Fencing epoch, bumped every time a node re-registers. Responses
				-- carrying a stale epoch are from an instance the server has already
				-- written off and must be ignored.
				epoch         bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
				total_vcpu    bigint NOT NULL DEFAULT 0 CHECK (total_vcpu >= 0),
				total_memory  bigint NOT NULL DEFAULT 0 CHECK (total_memory >= 0),
				last_seen_at  text NOT NULL,
				drained       bigint NOT NULL DEFAULT 0 CHECK (drained IN (0, 1))
			);
-- +billet:end
