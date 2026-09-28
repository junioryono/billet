-- Job usage: what a job did to the host, measured by the host (migration 57).

-- name: RecordJobUsage :exec
-- Keep the first usage report for a lease.
--
-- DO NOTHING ON CONFLICT, because a node reports once and a retry after a lost
-- answer carries the same summary; a second report is not allowed to replace
-- the first. The epoch fence is the caller's, in the same transaction.
INSERT INTO job_usage
     (lease_id, node, recorded_at, source, unmeasured, samples, interval_ms,
      window_ms, cpu_user_us, cpu_system_us, guest_cpu_us, vmm_cpu_us,
      memory_peak_bytes, oom_kills, disk_read_bytes, disk_write_bytes,
      net_rx_bytes, net_tx_bytes, net_rx_packets, net_tx_packets,
      cpu_some_us, cpu_full_us, memory_some_us, memory_full_us, io_some_us,
      io_full_us, energy_active_uj, energy_idle_uj, energy_source)
VALUES (@lease_id, @node, @recorded_at, @source, @unmeasured, @samples,
        @interval_ms, @window_ms, @cpu_user_us, @cpu_system_us, @guest_cpu_us,
        @vmm_cpu_us, @memory_peak_bytes, @oom_kills, @disk_read_bytes,
        @disk_write_bytes, @net_rx_bytes, @net_tx_bytes, @net_rx_packets,
        @net_tx_packets, @cpu_some_us, @cpu_full_us, @memory_some_us,
        @memory_full_us, @io_some_us, @io_full_us, @energy_active_uj,
        @energy_idle_uj, @energy_source)
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
       io_full_us, energy_active_uj, energy_idle_uj, energy_source
  FROM job_usage WHERE lease_id = @lease_id;

-- name: ReadJobSeries :one
-- One lease's series blob and the codec that wrote it.
SELECT codec, series FROM job_series WHERE lease_id = @lease_id;
