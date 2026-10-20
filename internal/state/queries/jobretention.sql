-- Job detail retention: a job's series, destinations and guest report are kept
-- for a bounded time and then deleted, its usage summary kept (migration 63).
--
-- ONE BATCH IS ONE TRANSACTION OF FOUR STATEMENTS: the jobs are stamped first,
-- with a stamp unique to the batch, and the three deletes then take exactly the
-- stamped jobs' rows, so a batch never deletes detail it did not mark retired
-- and never marks a job whose detail it did not delete.

-- name: MarkJobDetailRetired :execrows
-- Stamp at most max_rows jobs that still hold detail recorded before a moment:
-- a series or destinations, by when the job's usage was recorded (they are
-- written in its transaction), or a guest report, by its own time. Oldest
-- first. A retired job holds none, so batches progress; a job with no history
-- row cannot be stamped, so the join keeps one from taking a place in every
-- batch. A stamp, once written, is the job's first retirement and is kept.
UPDATE job_history
   SET detail_retired_at = CAST(@stamp AS TEXT)
 WHERE detail_retired_at = ''
   AND lease_id IN (
       SELECT lease_id FROM (
           SELECT u.lease_id AS lease_id, u.recorded_at AS at
             FROM job_usage u
             JOIN job_history h ON h.lease_id = u.lease_id
            WHERE u.recorded_at < @before
              AND (EXISTS (SELECT 1 FROM job_series s WHERE s.lease_id = u.lease_id)
                   OR EXISTS (SELECT 1 FROM job_destinations d WHERE d.lease_id = u.lease_id))
           UNION
           SELECT g.lease_id AS lease_id, g.recorded_at AS at
             FROM job_guest_reports g
             JOIN job_history h ON h.lease_id = g.lease_id
            WHERE g.recorded_at < @before
       ) old
        ORDER BY at, lease_id
        LIMIT CAST(@max_rows AS BIGINT));

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
 WHERE lease_id IN (SELECT lease_id FROM job_history WHERE detail_retired_at = CAST(@stamp AS TEXT));

-- name: DeleteRetiredJobGuestReports :execrows
-- The guest reports of the jobs a batch stamped.
DELETE FROM job_guest_reports
 WHERE lease_id IN (SELECT lease_id FROM job_history WHERE detail_retired_at = CAST(@stamp AS TEXT));
