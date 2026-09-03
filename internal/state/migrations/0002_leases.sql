-- migration 2: leases
--
-- The capacity ledger. A row exists from the moment capacity is escrowed
-- — before a scale-set listener advertises it to GitHub — not from the
-- moment a VM boots. Reserving any later lets concurrent tier listeners
-- each advertise their own maximum and collectively overcommit the host.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE leases (
				id           TEXT PRIMARY KEY,
				tier         TEXT NOT NULL,
				node         TEXT REFERENCES nodes(name) ON DELETE SET NULL,
				phase        TEXT NOT NULL CHECK (phase IN
					('capacity','assigned','launching','online','busy','done','failed')),
				vcpu         INTEGER NOT NULL CHECK (vcpu > 0),
				memory       INTEGER NOT NULL CHECK (memory > 0),
				run_id       INTEGER,
				job_id       INTEGER,
				epoch        INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
				created_at   TEXT NOT NULL,
				heartbeat_at TEXT NOT NULL,
				expires_at   TEXT NOT NULL
			) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX leases_open_idx ON leases(phase) WHERE phase NOT IN ('done','failed')
-- +billet:end

-- +billet:statement
CREATE INDEX leases_node_idx ON leases(node)
-- +billet:end
