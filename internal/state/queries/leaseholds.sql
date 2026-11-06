-- Lease holds: a failed job's VM kept for someone to look at (migration 75). A hold delays a
-- destroy a completion already asked for; nothing here touches capacity.

-- name: OpenLeaseHold :exec
-- Open a lease's hold. DO NOTHING ON CONFLICT, because this is what keeps a hold to its bound:
-- until_at is written once, and a retry, a restart or a second completion cannot move it.
INSERT INTO lease_holds (lease_id, tier, held_at, until_at)
VALUES (@lease_id, @tier, @held_at, @until_at)
ON CONFLICT (lease_id) DO NOTHING;

-- name: ReadLeaseHold :one
-- A lease's hold. sql.ErrNoRows means none was opened.
SELECT lease_id, tier, held_at, until_at, ended_at, ended_by
  FROM lease_holds WHERE lease_id = @lease_id;

-- name: EndLeaseHold :execrows
-- End a lease's hold if it is holding. An ended hold keeps what ended it first.
UPDATE lease_holds SET ended_at = @ended_at, ended_by = @ended_by
 WHERE lease_id = @lease_id AND ended_at = '';

-- name: EndHoldingLeaseHolds :execrows
-- End every hold that is holding, for a drain that chose to cut them.
UPDATE lease_holds SET ended_at = @ended_at, ended_by = @ended_by WHERE ended_at = '';

-- name: ListHoldingLeaseHolds :many
-- Every hold that is holding, the soonest to end first.
SELECT lease_id, tier, held_at, until_at, ended_at, ended_by
  FROM lease_holds WHERE ended_at = ''
 ORDER BY until_at, lease_id;

-- name: DeleteEndedLeaseHolds :execrows
-- Forget holds that ended before a moment: what a hold did is in the job's history and the
-- log, and these rows are only what `billet status` and the listener read while one lasts. NOT
-- WHILE THE LEASE LIVES: a completion can go on being retried, and with no row it would open a
-- new hold on a VM whose hold already ended.
DELETE FROM lease_holds
 WHERE ended_at != '' AND ended_at < CAST(@before AS TEXT)
   AND lease_id NOT IN (SELECT id FROM leases WHERE phase NOT IN ('done', 'failed'));
