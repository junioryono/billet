-- migration 80: pool_clearance
--
-- A POOL HOST DRAINING ALONE PROVES ITSELF IDLE (ADR-030, #553), without the deployment
-- seal `billet drain --wait` takes. Its release asked for, the host is out of placement;
-- the control plane then asks its node what it runs, as the compute barrier asks every
-- node, and the release is sealed on two empty answers spanning the absence grace, each
-- taken against the node's epoch and launch-dispatch generation read before the question
-- and while nothing in the ledger holds the host.
--
-- The run is the order's, one per order, because a release is the only thing that asks:
-- clear_epoch and clear_dispatch are the fence the run was taken under, clear_since when
-- the run's first empty answer was recorded (no earlier than its node listed nothing) and
-- clear_observed_at when the latest was asked (no later than its node did), empty when
-- there is no run. A run is proved when clear_observed_at is at least the grace after
-- clear_since, which is two answers and never a clock. clear_generation counts the
-- contradictions of the release; an answer is recorded only under the generation read
-- before it was asked, so one in flight across a contradiction never resumes a run.
--
-- EVERYTHING THAT CONTRADICTS A RELEASE ENDS THE RUN, in the same statement that records
-- the contradiction (ContradictPoolRelease), so a run that reaches proved began after
-- the last contradiction and stands for the order however its release_contradicted reads.

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_epoch INTEGER NOT NULL DEFAULT 0 CHECK (clear_epoch >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_dispatch INTEGER NOT NULL DEFAULT 0 CHECK (clear_dispatch >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_since TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_observed_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN clear_generation INTEGER NOT NULL DEFAULT 0 CHECK (clear_generation >= 0)
-- +billet:end
