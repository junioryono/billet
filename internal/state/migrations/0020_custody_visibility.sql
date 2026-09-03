-- migration 20: custody_visibility
--
-- Custody is durable operator-visible state even though the node's detailed
-- tending record remains local. force_release is a request TO that holder: the
-- node observes it through heartbeat, drops its local proof obligation, and
-- terminalizes the lease itself rather than having the control plane release
-- capacity underneath a process that still believes it owns it.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN held_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN force_release INTEGER NOT NULL DEFAULT 0 CHECK (force_release IN (0,1))
-- +billet:end

-- +billet:statement
CREATE TABLE leases_new (
			id               TEXT PRIMARY KEY,
			tier             TEXT NOT NULL,
			node             TEXT REFERENCES nodes(name) ON DELETE SET NULL,
			phase            TEXT NOT NULL CHECK (phase IN
				('capacity','assigned','launching','online','busy','custody','teardown','quarantine','done','failed')),
			vcpu             INTEGER NOT NULL CHECK (vcpu > 0),
			memory           INTEGER NOT NULL CHECK (memory > 0),
			run_id           INTEGER,
			request_id       INTEGER,
			epoch            INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
			created_at       TEXT NOT NULL,
			heartbeat_at     TEXT NOT NULL,
			expires_at       TEXT NOT NULL,
			target_node      TEXT,
			macos_slot       INTEGER NOT NULL DEFAULT 0 CHECK (macos_slot IN (0, 1)),
			guest_os         TEXT NOT NULL DEFAULT 'linux',
			provider         TEXT NOT NULL DEFAULT '',
			providers        TEXT NOT NULL DEFAULT '',
			chosen_provider  TEXT NOT NULL DEFAULT '',
			requested_vcpu   INTEGER NOT NULL DEFAULT 0 CHECK (requested_vcpu >= 0),
			requested_memory INTEGER NOT NULL DEFAULT 0 CHECK (requested_memory >= 0),
			instance_type    TEXT NOT NULL DEFAULT '',
			held_at          TEXT NOT NULL DEFAULT '',
			force_release    INTEGER NOT NULL DEFAULT 0 CHECK (force_release IN (0,1))
		) STRICT
-- +billet:end

-- +billet:statement
INSERT INTO leases_new
		   (id, tier, node, phase, vcpu, memory, run_id, request_id, epoch, created_at,
		    heartbeat_at, expires_at, target_node, macos_slot, guest_os, provider,
		    providers, chosen_provider, requested_vcpu, requested_memory, instance_type,
		    held_at, force_release)
		 SELECT id, tier, node, phase, vcpu, memory, run_id, request_id, epoch, created_at,
		        heartbeat_at, expires_at, target_node, macos_slot, guest_os, provider,
		        providers, chosen_provider, requested_vcpu, requested_memory, instance_type,
		        held_at, force_release
		   FROM leases
-- +billet:end

-- +billet:statement
DROP TABLE leases
-- +billet:end

-- +billet:statement
ALTER TABLE leases_new RENAME TO leases
-- +billet:end

-- +billet:statement
UPDATE leases SET held_at = heartbeat_at WHERE phase = 'quarantine'
-- +billet:end

-- +billet:statement
CREATE INDEX leases_open_idx ON leases(phase) WHERE phase NOT IN ('done','failed')
-- +billet:end

-- +billet:statement
CREATE INDEX leases_node_idx ON leases(node)
-- +billet:end

-- +billet:statement
CREATE INDEX leases_expiry_idx ON leases(expires_at) WHERE phase NOT IN ('done','failed')
-- +billet:end
