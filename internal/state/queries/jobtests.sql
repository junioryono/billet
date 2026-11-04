-- Test results: a job's test suites and cases, the guest's own unverified view,
-- kept past its guest report (migration 71).

-- name: RecordJobTestResults :execrows
-- Keep the first test results for a lease.
--
-- DO NOTHING ON CONFLICT, for RecordJobGuestReport's reason, and written in that
-- report's transaction: a retry carries the same bytes and may not replace them.
INSERT INTO job_test_results (lease_id, recorded_at, codec, encoded_bytes, data)
VALUES (@lease_id, @recorded_at, @codec, @encoded_bytes, @data)
ON CONFLICT (lease_id) DO NOTHING;

-- name: ReadJobTestResults :one
-- One lease's test results. sql.ErrNoRows means none was kept, which a reader says
-- rather than printing no tests.
SELECT lease_id, recorded_at, codec, encoded_bytes, data
  FROM job_test_results WHERE lease_id = @lease_id;

-- name: ListJobTestResults :many
-- The test results recorded since a moment with the job each came from, newest
-- first, a page at a time from a cursor, (before_recorded, before_lease), the last
-- row of the page before; an empty before_recorded is the newest. An empty repo is
-- every repository.
--
-- THE RUN IS THE ONE THAT STARTED, as ListJobsAwaitingQueueTime reads it.
SELECT t.lease_id, t.recorded_at, t.codec, t.data, h.tier, COALESCE(h.repo, '') AS repo,
       h.workflow_ref, h.job_name, h.head_sha, h.head_branch, COALESCE(h.result, '') AS result,
       CAST(CASE WHEN h.github_run_id > 0 THEN h.github_run_id ELSE COALESCE(h.run_id, 0) END
            AS BIGINT) AS run_id
  FROM job_test_results t
  JOIN job_history h ON h.lease_id = t.lease_id
 WHERE t.recorded_at >= CAST(@since AS TEXT)
   AND (CAST(@repo AS TEXT) = '' OR h.repo = CAST(@repo AS TEXT))
   AND (CAST(@before_recorded AS TEXT) = ''
        OR t.recorded_at < CAST(@before_recorded AS TEXT)
        OR (t.recorded_at = CAST(@before_recorded AS TEXT) AND t.lease_id < CAST(@before_lease AS TEXT)))
 ORDER BY t.recorded_at DESC, t.lease_id DESC
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListJobTestResultsPastRetention :many
-- At most max_rows leases whose test results were recorded before a moment, oldest
-- first. Found on a read and deleted in a write, for the reason jobretention.sql
-- gives.
SELECT lease_id FROM job_test_results
 WHERE recorded_at < CAST(@before AS TEXT)
 ORDER BY recorded_at, lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListJobTestResultsPastBound :many
-- At most max_rows leases whose test results lie past a bound on the table's size,
-- oldest first: every row once the rows newer than it and itself total more than
-- max_bytes.
SELECT lease_id
  FROM (
      SELECT lease_id, recorded_at,
             CAST(SUM(encoded_bytes) OVER (ORDER BY recorded_at DESC, lease_id DESC) AS BIGINT) AS newer
        FROM job_test_results
  ) AS sized
 WHERE newer > CAST(@max_bytes AS BIGINT)
 ORDER BY recorded_at, lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: DeleteJobTestResults :execrows
-- The test results of the leases a read listed. ONE STATEMENT FOR THE BATCH, packed
-- as MarkJobDetailRetired reads its list, for the reason it gives.
WITH RECURSIVE ids (id, rest) AS (
    SELECT SUBSTR(CAST(@packed AS TEXT), 1, 32), SUBSTR(CAST(@packed AS TEXT), 33)
    UNION ALL
    SELECT SUBSTR(rest, 1, 32), SUBSTR(rest, 33) FROM ids WHERE rest != ''
)
DELETE FROM job_test_results WHERE lease_id IN (SELECT id FROM ids);
