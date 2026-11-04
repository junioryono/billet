-- What the host saw of a job's network: the names its guest asked for, what they
-- answered, and what a tier's allowlist refused (migration 71).

-- name: RecordJobEgress :execrows
-- Keep the first egress report for a lease.
--
-- DO NOTHING ON CONFLICT, for RecordJobUsage's reason: a retry after a lost answer
-- carries the same report, and a second is not allowed to replace the first. The
-- epoch fence is the caller's, in the same transaction.
INSERT INTO job_egress (lease_id, node, recorded_at, listed, report)
VALUES (@lease_id, @node, @recorded_at, @listed, @report)
ON CONFLICT (lease_id) DO NOTHING;

-- name: ReadJobEgress :one
-- One lease's egress report. sql.ErrNoRows means none was kept, which a reader
-- says rather than printing an empty report.
SELECT lease_id, node, recorded_at, listed, report
  FROM job_egress WHERE lease_id = @lease_id;
