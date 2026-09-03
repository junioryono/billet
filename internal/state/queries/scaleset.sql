-- name: DeleteMovedScaleSet :exec
-- Forget a record whose scale set id now answers to a different name.
--
-- A scale set deleted and recreated outside billet keeps its name and takes a
-- new id; one RENAMED keeps its id under a new name. This removes the stale
-- (group, label) so the orphan report does not name an object that is gone.
DELETE FROM scale_set
 WHERE org = @org AND scale_set_id = @scale_set_id
   AND NOT (runner_group = @runner_group AND label = @label);

-- name: UpsertScaleSet :exec
-- Remember that billet created this scale set.
--
-- The id is REFRESHED rather than kept: the server reconciles every tier on
-- every start, so this runs constantly against rows that already exist, and a
-- stale id would send an operator looking for an object that no longer exists.
INSERT INTO scale_set (org, runner_group, label, scale_set_id, created_at)
VALUES (@org, @runner_group, @label, @scale_set_id, @created_at)
ON CONFLICT (org, runner_group, label) DO UPDATE SET scale_set_id = excluded.scale_set_id;

-- name: DeleteScaleSet :exec
-- Drop the record for a scale set billet has deleted.
DELETE FROM scale_set WHERE org = @org AND runner_group = @runner_group AND label = @label;

-- name: ListScaleSets :many
-- Every scale set billet recorded creating for one organization.
SELECT org, runner_group, label, scale_set_id FROM scale_set
 WHERE org = @org ORDER BY runner_group, label;
