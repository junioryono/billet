-- migration 20: custody_visibility, for PostgreSQL
--
-- The twin of migrations/0020_custody_visibility.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN held_at text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN force_release bigint NOT NULL DEFAULT 0 CHECK (force_release IN (0,1));
-- +billet:end

-- +billet:statement
CREATE TABLE leases_new (
			id               text PRIMARY KEY,
			tier             text NOT NULL,
			node             text REFERENCES nodes(name) ON DELETE SET NULL,
			phase            text NOT NULL CHECK (phase IN
				('capacity','assigned','launching','online','busy','custody','teardown','quarantine','done','failed')),
			vcpu             bigint NOT NULL CHECK (vcpu > 0),
			memory           bigint NOT NULL CHECK (memory > 0),
			run_id           bigint,
			request_id       bigint,
			epoch            bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
			created_at       text NOT NULL,
			heartbeat_at     text NOT NULL,
			expires_at       text NOT NULL,
			target_node      text,
			macos_slot       bigint NOT NULL DEFAULT 0 CHECK (macos_slot IN (0, 1)),
			guest_os         text NOT NULL DEFAULT 'linux',
			provider         text NOT NULL DEFAULT '',
			providers        text NOT NULL DEFAULT '',
			chosen_provider  text NOT NULL DEFAULT '',
			requested_vcpu   bigint NOT NULL DEFAULT 0 CHECK (requested_vcpu >= 0),
			requested_memory bigint NOT NULL DEFAULT 0 CHECK (requested_memory >= 0),
			instance_type    text NOT NULL DEFAULT '',
			held_at          text NOT NULL DEFAULT '',
			force_release    bigint NOT NULL DEFAULT 0 CHECK (force_release IN (0,1))
		);
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
		   FROM leases;
-- +billet:end

-- +billet:statement
DROP TABLE leases;
-- +billet:end

-- +billet:statement
ALTER TABLE leases_new RENAME TO leases;
-- +billet:end

-- +billet:statement
UPDATE leases SET held_at = heartbeat_at WHERE phase = 'quarantine';
-- +billet:end

-- +billet:statement
CREATE INDEX leases_open_idx ON leases(phase) WHERE phase NOT IN ('done','failed');
-- +billet:end

-- +billet:statement
CREATE INDEX leases_node_idx ON leases(node);
-- +billet:end

-- +billet:statement
CREATE INDEX leases_expiry_idx ON leases(expires_at) WHERE phase NOT IN ('done','failed');
-- +billet:end
