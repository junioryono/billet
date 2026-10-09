-- Job usage: what a job did to the host, measured by the host (migration 57).

-- name: RecordJobUsage :execrows
-- Keep the first usage report for a lease.
--
-- DO NOTHING ON CONFLICT, because a node reports once and a retry after a lost
-- answer carries the same summary; a second report is not allowed to replace
-- the first. The epoch fence is the caller's, in the same transaction.
--
-- THE ROW COUNT SAYS WHETHER THIS REPORT WON, and only the report that won may
-- write the series: otherwise a first report without a series and a second
-- with one would be stored as a pair neither request sent.
--
-- A hardware counter is NULL where it was not counted (migration 58), and
-- destinations_incomplete is NULL where the flows were not totalled, the tap's
-- totals NULL where the tap was not read (migration 59).
INSERT INTO job_usage
     (lease_id, node, recorded_at, source, unmeasured, samples, interval_ms,
      window_ms, cpu_user_us, cpu_system_us, guest_cpu_us, vmm_cpu_us,
      memory_peak_bytes, oom_kills, disk_read_bytes, disk_write_bytes,
      net_rx_bytes, net_tx_bytes, net_rx_packets, net_tx_packets,
      cpu_some_us, cpu_full_us, memory_some_us, memory_full_us, io_some_us,
      io_full_us, energy_active_uj, energy_idle_uj, energy_source,
      cycles, instructions, cache_references, cache_misses, branch_misses,
      frontend_stall_cycles, destinations_incomplete, tap_sent_bytes,
      tap_received_bytes)
VALUES (@lease_id, @node, @recorded_at, @source, @unmeasured, @samples,
        @interval_ms, @window_ms, @cpu_user_us, @cpu_system_us, @guest_cpu_us,
        @vmm_cpu_us, @memory_peak_bytes, @oom_kills, @disk_read_bytes,
        @disk_write_bytes, @net_rx_bytes, @net_tx_bytes, @net_rx_packets,
        @net_tx_packets, @cpu_some_us, @cpu_full_us, @memory_some_us,
        @memory_full_us, @io_some_us, @io_full_us, @energy_active_uj,
        @energy_idle_uj, @energy_source, @cycles, @instructions,
        @cache_references, @cache_misses, @branch_misses,
        @frontend_stall_cycles, @destinations_incomplete, @tap_sent_bytes,
        @tap_received_bytes)
ON CONFLICT (lease_id) DO NOTHING;

-- name: RecordJobSeries :exec
-- Keep the first series for a lease, for the reason RecordJobUsage keeps the
-- first summary.
INSERT INTO job_series (lease_id, codec, series)
VALUES (@lease_id, @codec, @series)
ON CONFLICT (lease_id) DO NOTHING;

-- name: ReadJobUsage :one
-- One lease's usage summary. sql.ErrNoRows means nothing was measured, which a
-- reader says rather than printing zeros.
SELECT lease_id, node, recorded_at, source, unmeasured, samples, interval_ms,
       window_ms, cpu_user_us, cpu_system_us, guest_cpu_us, vmm_cpu_us,
       memory_peak_bytes, oom_kills, disk_read_bytes, disk_write_bytes,
       net_rx_bytes, net_tx_bytes, net_rx_packets, net_tx_packets,
       cpu_some_us, cpu_full_us, memory_some_us, memory_full_us, io_some_us,
       io_full_us, energy_active_uj, energy_idle_uj, energy_source, cycles,
       instructions, cache_references, cache_misses, branch_misses,
       frontend_stall_cycles, destinations_incomplete, tap_sent_bytes,
       tap_received_bytes
  FROM job_usage WHERE lease_id = @lease_id;

-- name: ReadJobSeries :one
-- One lease's series blob and the codec that wrote it.
SELECT codec, series FROM job_series WHERE lease_id = @lease_id;

-- name: RecordJobDestination :exec
-- One of a job's destinations, or with an empty address and ordinal 256 the
-- total beyond them (migration 59). Written only by the usage report that won
-- RecordJobUsage, in its transaction, so a lease's rows are one report's.
INSERT INTO job_destinations
     (lease_id, ordinal, addr, sent_bytes, received_bytes, connections)
VALUES (@lease_id, @ordinal, @addr, @sent_bytes, @received_bytes,
        @connections);

-- name: ReadJobDestinations :many
-- One lease's destinations in the order the node gave them, the total beyond
-- them last.
SELECT ordinal, addr, sent_bytes, received_bytes, connections
  FROM job_destinations WHERE lease_id = @lease_id
 ORDER BY ordinal;
