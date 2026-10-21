-- Job detail retention: a job's series, destinations and guest report are kept
-- for a bounded time and then deleted, its usage summary kept (migration 63).
--
-- FOUND ON A READ, RETIRED IN A WRITE. The candidates are listed outside any
-- write transaction, since listing them reads every unretired job's detail;
-- a write batch then stamps the jobs it was handed by their key, each still
-- unretired and finished, and the deletes take exactly the stamped jobs'
-- rows, so a batch never deletes detail it did not mark retired and never
-- marks a job whose detail it did not delete, and the writer slot is held for
-- key lookups and indexed deletes only.

-- name: ListRetirableJobDetail :many
-- At most max_rows finished, unretired jobs all of whose detail was recorded
-- before a moment, oldest first: a series or destinations, by when the job's
-- usage was recorded (they are written in its transaction), and a guest
-- report, by its own time.
--
-- DISCOVERED FROM WHAT IS LEFT TO RETIRE, never from the history: job_series
-- and job_guest_reports hold only unretired detail, and the usage rows that
-- still claim destinations are the partial index migration 63 adds, which a
-- retirement empties. So the read's work is the unretired detail, which the
-- retention bounds, and not every job the deployment ever ran.
--
-- ONE CANDIDATE A JOB, grouped by lease, so a job whose usage and guest report
-- arrived at different times does not take two places in the limit. ONLY A
-- FINISHED JOB, whose lease takes no more reports, so no detail arrives after
-- its retirement. A job with no history row is never a candidate, since it
-- could never be stamped.
SELECT c.lease_id
  FROM (
      SELECT s.lease_id AS lease_id, u.recorded_at AS at
        FROM job_series s JOIN job_usage u ON u.lease_id = s.lease_id
      UNION ALL
      SELECT u.lease_id AS lease_id, u.recorded_at AS at
        FROM job_usage u WHERE u.destinations_incomplete IS NOT NULL
      UNION ALL
      SELECT g.lease_id AS lease_id, g.recorded_at AS at
        FROM job_guest_reports g
  ) AS c
  JOIN job_history h ON h.lease_id = c.lease_id
 WHERE h.detail_retired_at = '' AND COALESCE(h.finished_at, '') != ''
 GROUP BY c.lease_id
HAVING MAX(c.at) < CAST(@before AS TEXT)
 ORDER BY MAX(c.at), c.lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: MarkJobDetailRetired :execrows
-- Stamp the jobs a read listed that are still unretired and finished: their
-- detail cannot have changed since, because a finished job's lease takes no
-- report, and a stamp, once written, is kept.
--
-- ONE STATEMENT FOR THE BATCH, for RecordJobDestinations' reason: a write
-- transaction holds the writer slot (SQLite) or the deployment's write lock
-- (PostgreSQL) for every round trip it makes. The caller packs the lease ids
-- end to end (internal/alloc packLeaseIDs: each a lease id's 32 characters),
-- and the recursive list below cuts one off the front at each step.
WITH RECURSIVE ids (id, rest) AS (
    SELECT SUBSTR(CAST(@packed AS TEXT), 1, 32), SUBSTR(CAST(@packed AS TEXT), 33)
    UNION ALL
    SELECT SUBSTR(rest, 1, 32), SUBSTR(rest, 33) FROM ids WHERE rest != ''
)
UPDATE job_history
   SET detail_retired_at = CAST(@stamp AS TEXT)
 WHERE lease_id IN (SELECT id FROM ids)
   AND detail_retired_at = '' AND COALESCE(finished_at, '') != '';

-- name: DeleteRetiredJobSeries :execrows
-- The series of the jobs a batch stamped.
DELETE FROM job_series
 WHERE lease_id IN (SELECT lease_id FROM job_history WHERE detail_retired_at = CAST(@stamp AS TEXT));

-- name: DeleteRetiredJobDestinations :execrows
-- The destinations of the jobs a batch stamped.
DELETE FROM job_destinations
 WHERE lease_id IN (SELECT lease_id FROM job_history WHERE detail_retired_at = CAST(@stamp AS TEXT));

-- name: ForgetRetiredJobDestinationTotals :exec
-- The usage summary's verdict on the destinations of the jobs a batch stamped,
-- and the tap's totals read beside them. A verdict is the summary's claim that
-- destination rows exist, and a reader refuses one without its rows (alloc
-- readDestinations), so it goes with them.
UPDATE job_usage
   SET destinations_incomplete = NULL, tap_sent_bytes = NULL, tap_received_bytes = NULL
 WHERE destinations_incomplete IS NOT NULL
   AND lease_id IN (SELECT lease_id FROM job_history WHERE detail_retired_at = CAST(@stamp AS TEXT));

-- name: DeleteRetiredJobGuestReports :execrows
-- The guest reports of the jobs a batch stamped.
DELETE FROM job_guest_reports
 WHERE lease_id IN (SELECT lease_id FROM job_history WHERE detail_retired_at = CAST(@stamp AS TEXT));
