-- name: CountForceDestroyInState :one
-- How many force-destroy requests sit in one state.
--
-- Asked inside the write transaction that would create the next one: two
-- concurrent forces would each enumerate a target set the other was midway
-- through destroying.
SELECT COUNT(*) FROM force_destroy WHERE state = @state;

-- name: HighestForceDestroyGeneration :one
-- The highest generation on file, or 0 when there has never been a force.
--
-- CAST because sqlc types a bare MAX (and a bare COALESCE) as interface{};
-- measured. COALESCE first, because CAST(NULL AS BIGINT) is still NULL. The 0
-- reproduces exactly what a NULL scanned into sql.NullInt64 used to yield, so
-- the caller's +1 still makes the first request generation 1.
SELECT CAST(COALESCE(MAX(generation), 0) AS BIGINT) AS generation FROM force_destroy;

-- name: InsertForceDestroy :exec
-- Record one operator decision to destroy running compute.
--
-- completed_at is empty rather than null: this row is read back into a struct
-- whose fields are strings, and a nullable column would make "not finished" and
-- "billet could not read it" the same value.
INSERT INTO force_destroy
    (generation, admission_generation, state, reason, actor, requested_at, completed_at)
VALUES (@generation, @admission_generation, @state, @reason, @actor, @requested_at, '');

-- name: InsertForceDestroyTarget :exec
-- Record one lease the operator authorised destroying.
--
-- EVERY FIELD IS STORED so the diagnostic survives the command: a listener
-- acting on this a poll later, or a second control plane after a restart, has no
-- other record of what the person actually approved.
INSERT INTO force_destroy_target
    (generation, lease_id, tier, node, run_id, scheduler_request, phase, state, detail)
VALUES (@generation, @lease_id, @tier, @node, @run_id, @scheduler_request, @phase,
        @state, '');

-- name: ForceDestroyInState :one
-- The one request sitting in a given state, if any.
SELECT generation, admission_generation, state, reason, actor, requested_at,
       completed_at
  FROM force_destroy WHERE state = @state;

-- name: LatestForceDestroy :one
-- The most recent request, open or finished, for the report an operator reads
-- after the fact.
SELECT generation, admission_generation, state, reason, actor, requested_at,
       completed_at
  FROM force_destroy ORDER BY generation DESC LIMIT 1;

-- name: PendingForceTargets :many
-- The leases one tier still owes a destroy for.
--
-- SCOPED TO A TIER because a listener may only act on its own escrow: destroying
-- another tier's compute would be tearing down a lease it never held and cannot
-- release.
SELECT lease_id, tier, node, run_id, scheduler_request, phase, state, detail
  FROM force_destroy_target
 WHERE generation = @generation AND tier = @tier AND state = @state
 ORDER BY lease_id;

-- name: ForceTargets :many
-- Every lease one request covers, whatever became of it.
SELECT lease_id, tier, node, run_id, scheduler_request, phase, state, detail
  FROM force_destroy_target WHERE generation = @generation
 ORDER BY tier, lease_id;

-- name: SettleForceTarget :execresult
-- Record what became of one lease, and only while it is still pending.
--
-- :execresult because ALREADY SETTLED IS NOT AN ERROR. A listener that restarts
-- re-observes the request and may act on a target another incarnation already
-- finished, so the caller reads RowsAffected to tell "I settled it" from
-- "somebody already had".
UPDATE force_destroy_target SET state = @state, detail = @detail
 WHERE generation = @generation AND lease_id = @lease_id AND state = @was_state;

-- name: CountForceTargetsInState :one
-- How many of one request's targets are still in a given state.
--
-- Read in the SAME transaction as the settlement above, because completion is
-- decided from it: two listeners settling their last target concurrently would
-- otherwise both see work outstanding and leave a request nothing will finish.
SELECT COUNT(*) FROM force_destroy_target
 WHERE generation = @generation AND state = @state;

-- name: CompleteForceDestroy :exec
-- Close out a request once nothing is pending.
--
-- Guarded on the state it is leaving, so a second listener arriving at the same
-- conclusion cannot rewrite completed_at.
UPDATE force_destroy SET state = @state, completed_at = @completed_at
 WHERE generation = @generation AND state = @was_state;
