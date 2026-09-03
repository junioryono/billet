-- migration 40: rollout_dispatch_epoch
--
-- rolloutDispatchEpochMigration records the registration a rollout instruction
-- was sent against.
--
-- THE ONLY CAUSAL EVIDENCE A ROLLOUT HAS ABOUT A HOST COMING BACK. After telling
-- a node to upgrade, the coordinator sees two states that are identical in every
-- field it can read: a host that has not started yet and is still running its old
-- binary, and a host that upgraded, failed, rolled itself back and re-registered
-- on that same old binary. Both are live; both report the previous release.
--
-- A registration bumps the node's epoch and nothing else does, so an epoch HIGHER
-- than the one the instruction was sent against provably postdates it. That is
-- the rule the capacity code already lives by — a freshness check on your own
-- record is not a causal fence on somebody else's state — applied to the one
-- question a rollout cannot otherwise answer.
--
-- ZERO MEANS NOT RECORDED, which is what a row written before this migration
-- carries and what a host nothing was ever dispatched to has. Nothing is
-- authorised from a zero: a coordinator that cannot tell leaves the host where it
-- is, which holds the rollout open rather than concluding anything about it.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE rollout_nodes ADD COLUMN dispatch_epoch INTEGER NOT NULL DEFAULT 0
			CHECK (dispatch_epoch >= 0)
-- +billet:end
