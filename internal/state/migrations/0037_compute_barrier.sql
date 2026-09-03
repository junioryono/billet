-- migration 37: compute_barrier
--
-- THE COMPUTE BARRIER: what a host is running, asked for on demand and fenced so
-- the answer is causal rather than merely recent.
--
-- `node_inventory` one migration back is TELEMETRY — the host's last word,
-- already stale when it arrived. This is the machinery that turns an inventory
-- into evidence, and every column here exists to stop it becoming the earlier
-- thing.
--
-- `dispatch_generation` counts LAUNCHES DISPATCHED to a host, never commands: a
-- destroy, sweep, tend or inventory cannot create compute, so counting them
-- would invalidate proofs for no reason. It is advanced under the plane's mutex
-- BEFORE the launch enters that host's queue, which is what makes it impossible
-- for an acknowledgement to be accepted with a launch sitting behind it.
--
-- A DECOMMISSION IS MEMBERSHIP, and `drained` is the column that already means
-- "may not serve" to both placement queries and the floor arithmetic — so it is
-- reconciled rather than joined by a second concept that can disagree with it.
-- `decommission_proven` is the one that must never be lost: a host excluded
-- WITHOUT proof is an admission that billet does not know what is on it, and a
-- drain that excludes it has to print a different conclusion from a proven one.
--
-- `compute_barrier` is a SINGLETON REQUEST rather than a queue. `billet drain`
-- is a separate process with no handle to the running plane, so the request has
-- to be durable and observed; concurrent waiters join one barrier id instead of
-- superseding each other and resetting each other's runs. It is scoped to an
-- admission generation, which is what makes it self-cleaning: a resume moves the
-- generation and the loop deletes a request that can no longer mean anything.
--
-- `compute_barrier_nodes` holds a CONTINUOUS RUN, not an observation.
-- `empty_since` is the start of the run and `observed_at` its most recent
-- sample; an error, a non-empty answer or either fence moving DELETES the row,
-- because the whole claim is that the host has been empty without interruption.
-- A row is therefore never a single snapshot, which is what a stamped
-- observation would have been.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN dispatch_generation INTEGER NOT NULL DEFAULT 0
			CHECK (dispatch_generation >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN decommissioned_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN decommission_proven INTEGER NOT NULL DEFAULT 0
			CHECK (decommission_proven IN (0, 1))
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN decommission_actor TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE TABLE compute_barrier (
			id                   INTEGER PRIMARY KEY CHECK (id = 1),
			barrier_id           TEXT NOT NULL,
			admission_generation INTEGER NOT NULL CHECK (admission_generation >= 0),
			requested_at         TEXT NOT NULL,
			requested_by         TEXT NOT NULL
		) STRICT
-- +billet:end

-- +billet:statement
CREATE TABLE compute_barrier_nodes (
			node                TEXT PRIMARY KEY,
			barrier_id          TEXT NOT NULL,
			node_epoch          INTEGER NOT NULL CHECK (node_epoch >= 0),
			dispatch_generation INTEGER NOT NULL CHECK (dispatch_generation >= 0),
			empty_since         TEXT NOT NULL,
			observed_at         TEXT NOT NULL
		) STRICT
-- +billet:end
