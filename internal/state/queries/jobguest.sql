-- Guest reports: what the agent inside a job's guest told the node, the
-- guest's own unverified view (migration 60).

-- name: RecordJobGuestReport :execrows
-- Keep the first guest report for a lease.
--
-- DO NOTHING ON CONFLICT, for RecordJobUsage's reason: a retry after a lost
-- answer carries the same report, and a second is not allowed to replace the
-- first. The epoch fence is the caller's, in the same transaction. The arrival
-- times are NULL where no batch arrived.
INSERT INTO job_guest_reports
     (lease_id, node, recorded_at, agent_version, agent_schema, codec, data,
      accepted, refused, dropped_bytes, hello, final_seen, node_restarted,
      first_received_at, last_received_at)
VALUES (@lease_id, @node, @recorded_at, @agent_version, @agent_schema, @codec,
        @data, @accepted, @refused, @dropped_bytes, @hello, @final_seen,
        @node_restarted, @first_received_at, @last_received_at)
ON CONFLICT (lease_id) DO NOTHING;

-- name: ReadJobGuestReport :one
-- One lease's guest report. sql.ErrNoRows means none was kept, which a reader
-- says rather than printing an empty report.
SELECT lease_id, node, recorded_at, agent_version, agent_schema, codec, data,
       accepted, refused, dropped_bytes, hello, final_seen, node_restarted,
       first_received_at, last_received_at
  FROM job_guest_reports WHERE lease_id = @lease_id;
