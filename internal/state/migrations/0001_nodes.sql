-- migration 1: nodes
--
-- A live registration writes epoch 1 or more; zero is only ever a row no
-- registration has touched.
--
-- SAID HERE RATHER THAN IN THE SQL, and that is not a style choice: a
-- migration's checksum covers the STATEMENT BYTES, comments included, so
-- adding this sentence inside the CREATE TABLE changed migration 1 and
-- every ledger written before that stopped opening. See migrationsAreFrozen.
--
-- A node is a compute host. Nodes dial the server, so the server learns
-- of them here rather than from configuration.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE nodes (
				name          TEXT PRIMARY KEY,
				provider      TEXT NOT NULL,
				-- Fencing epoch, bumped every time a node re-registers. Responses
				-- carrying a stale epoch are from an instance the server has already
				-- written off and must be ignored.
				epoch         INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
				total_vcpu    INTEGER NOT NULL DEFAULT 0 CHECK (total_vcpu >= 0),
				total_memory  INTEGER NOT NULL DEFAULT 0 CHECK (total_memory >= 0),
				last_seen_at  TEXT NOT NULL,
				drained       INTEGER NOT NULL DEFAULT 0 CHECK (drained IN (0, 1))
			) STRICT
-- +billet:end
