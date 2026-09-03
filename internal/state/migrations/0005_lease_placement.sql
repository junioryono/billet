-- migration 5: lease_placement
--
-- placementMigration is appended to migrations in init. Kept as its own value
-- only so this long comment does not sit inside the main slice literal.
--
-- A reservation's PLACEMENT must be durable on the row, not derived from
-- whatever the tier catalog happens to say later.
--
-- target_node is the node a lease is constrained to by its tier's config
-- at reserve time; `node` remains the node that actually bound it. They
-- are different questions and were previously conflated: target_node has
-- no foreign key because it may name a host that has not registered yet,
-- while `node` keeps its FK because binding proves the node exists.
--
-- macos_slot records whether the lease consumes one of its host's macOS
-- guest licences. Counting that by walking the current tier map meant
-- renaming a tier, changing its guest_os, or restarting with a different
-- catalog silently reclassified leases already in flight.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN target_node TEXT
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN macos_slot INTEGER NOT NULL DEFAULT 0 CHECK (macos_slot IN (0, 1))
-- +billet:end

-- Reap scans by expiry; without this it is a full table scan holding the
-- only writer connection.
-- +billet:statement
CREATE INDEX leases_expiry_idx ON leases(expires_at) WHERE phase NOT IN ('done','failed')
-- +billet:end

-- job_history gains the queue timestamp so queue duration is measured
-- rather than fabricated from the terminalization time.
-- +billet:statement
ALTER TABLE job_history ADD COLUMN assigned_at TEXT
-- +billet:end
