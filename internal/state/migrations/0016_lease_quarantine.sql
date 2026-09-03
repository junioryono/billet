-- migration 16: lease_quarantine
--
-- CAPACITY IS NOT FREED BY A LEASE EXPIRING, only by its compute being gone.
--
-- The reaper terminalizes anything whose holder stopped heartbeating, which is
-- right for escrow nobody launched and wrong for a lease with a container behind
-- it: terminalizing frees the capacity immediately, while the container keeps
-- running until the node next sweeps. Another tier can escrow that slot in
-- between, and two jobs end up on a machine sized for one.
--
-- So an expired RUNNING lease moves to a phase that still charges the host, and
-- leaves it only on proof the compute is gone. The phase list is a CHECK
-- constraint and SQLite cannot alter one, so the table is rebuilt.
--
-- Columns are named rather than SELECT *: the order has to survive every
-- earlier migration for a star to be correct, and nothing checks that.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE leases_new (
			id              TEXT PRIMARY KEY,
			tier            TEXT NOT NULL,
			node            TEXT REFERENCES nodes(name) ON DELETE SET NULL,
			phase           TEXT NOT NULL CHECK (phase IN
				('capacity','assigned','launching','online','busy','quarantine','done','failed')),
			vcpu            INTEGER NOT NULL CHECK (vcpu > 0),
			memory          INTEGER NOT NULL CHECK (memory > 0),
			run_id          INTEGER,
			request_id      INTEGER,
			epoch           INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
			created_at      TEXT NOT NULL,
			heartbeat_at    TEXT NOT NULL,
			expires_at      TEXT NOT NULL,
			target_node     TEXT,
			macos_slot      INTEGER NOT NULL DEFAULT 0 CHECK (macos_slot IN (0, 1)),
			guest_os        TEXT NOT NULL DEFAULT 'linux',
			provider        TEXT NOT NULL DEFAULT '',
			providers       TEXT NOT NULL DEFAULT '',
			chosen_provider TEXT NOT NULL DEFAULT ''
		) STRICT
-- +billet:end

-- +billet:statement
INSERT INTO leases_new
		   (id, tier, node, phase, vcpu, memory, run_id, request_id, epoch, created_at,
		    heartbeat_at, expires_at, target_node, macos_slot, guest_os, provider,
		    providers, chosen_provider)
		 SELECT id, tier, node, phase, vcpu, memory, run_id, request_id, epoch, created_at,
		        heartbeat_at, expires_at, target_node, macos_slot, guest_os, provider,
		        providers, chosen_provider
		   FROM leases
-- +billet:end

-- +billet:statement
DROP TABLE leases
-- +billet:end

-- +billet:statement
ALTER TABLE leases_new RENAME TO leases
-- +billet:end

-- EVERY index the old table carried, not the ones that came to mind. A
-- rebuild drops them all, and a missing one is invisible until the table is
-- large enough for the scan to matter — leases_expiry_idx is what keeps the
-- reaper from scanning the whole lease history on the single writer
-- connection every listener is waiting for.
-- +billet:statement
CREATE INDEX leases_open_idx ON leases(phase) WHERE phase NOT IN ('done','failed')
-- +billet:end

-- +billet:statement
CREATE INDEX leases_node_idx ON leases(node)
-- +billet:end

-- +billet:statement
CREATE INDEX leases_expiry_idx ON leases(expires_at) WHERE phase NOT IN ('done','failed')
-- +billet:end
