-- Stranded jobs (#911): which launches that started nothing still wait for a decision
-- on whether GitHub kept offering their job, and the decision once made. Migration 84
-- holds why the decision lives on the row.

-- name: ListLaunchMisses :many
-- The launches that started nothing and are not yet decided, whose leases ended since
-- a moment, oldest first and bounded, with what GitHub's REST record of the run is read
-- by. The WHERE is the partial index's, word for word, so the read is the index.
SELECT lease_id, tier, COALESCE(repo, '') AS repo, CAST(COALESCE(run_id, 0) AS BIGINT) AS run_id,
       github_job_id, job_name, COALESCE(finished_at, '') AS finished_at
  FROM job_history
 WHERE failure_reason = 'billet:launch-failed' AND stranded = ''
   AND finished_at >= @since
 ORDER BY finished_at, lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: CountLaterLeasesOfRun :one
-- How many leases a tier bought for a run after a moment: GitHub offering the run's
-- work to the tier again, which a job it stopped offering never is.
SELECT CAST(COUNT(*) AS BIGINT) AS later
  FROM job_history
 WHERE tier = @tier AND run_id = @run_id AND queued_at > @after;

-- name: DecideStranded :execrows
-- Record whether GitHub kept offering a launch's job, once: a row already decided
-- keeps its decision, and the rows changed say whether this one decided it.
UPDATE job_history SET stranded = @stranded, stranded_at = @stranded_at
 WHERE lease_id = @lease_id AND stranded = '';
