-- What the control plane's sweep of staged CodeBuild registrations did, one row
-- per region and Parameter Store path.
--
-- A RECORD, NOT A DECISION. Nothing reads this to decide whether a parameter may
-- be deleted -- that is answered per lease by ReadLeaseSettlement. It exists so
-- `billet status`, which runs in another process, can say what the sweep removed,
-- what it is still waiting on, and whether its last pass failed.

-- name: RecordCredentialSweep :exec
-- Record one pass over one path.
--
-- removed IS THE LAST PASS AND removed_total ACCUMULATES, so a status line can
-- show both "3 removed in total" and whether the most recent pass did anything.
-- Every other column describes the last pass only: kept and unaccounted are
-- counts of what is there now, and error is empty on a pass that completed.
INSERT INTO credential_sweeps
   (region, path, swept_at, removed, removed_total, kept, unaccounted, foreign_names, error)
VALUES (@region, @path, @swept_at, @removed, @removed, @kept, @unaccounted, @foreign_names,
        @error)
ON CONFLICT (region, path) DO UPDATE SET
   swept_at      = excluded.swept_at,
   removed       = excluded.removed,
   removed_total = credential_sweeps.removed_total + excluded.removed,
   kept          = excluded.kept,
   unaccounted   = excluded.unaccounted,
   foreign_names = excluded.foreign_names,
   error         = excluded.error;

-- name: ListCredentialSweeps :many
-- Every path the sweep has ever recorded a pass over.
SELECT region, path, swept_at, removed, removed_total, kept, unaccounted, foreign_names,
       error
  FROM credential_sweeps
 ORDER BY region, path;
