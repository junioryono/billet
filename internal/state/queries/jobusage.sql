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

-- name: RecordJobDestinations :exec
-- Every one of a job's destination rows, the named ones and the total beyond
-- them (migration 59), written only by the usage report that won
-- RecordJobUsage, in its transaction, so a lease's rows are one report's.
--
-- ONE STATEMENT FOR UP TO 257 ROWS, because a write transaction holds the
-- ledger's only writer slot (SQLite) or its write lock (PostgreSQL) for every
-- round trip it makes, and one INSERT a row made 257 of them: measured
-- 2026-10-09 against a PostgreSQL on loopback, 18 ms a report one row at a time
-- and 14 ms in one statement, a gap that grows by 256 round trips on a real
-- network. A portable statement cannot take a list, so the caller packs the
-- rows into one string of fixed-width records (internal/alloc
-- packDestinations: a 3-digit ordinal, the address space-padded to 39
-- characters, then the sent, received and connection counts as 19 zero-padded
-- digits each, 99 characters a row) and the recursive slot list below cuts one
-- record off the front at each step.
--
-- EACH STEP CARRIES THE REST rather than indexing into the whole string,
-- because both engines find a character offset in TEXT by scanning from its
-- start: indexed, the same report took 57 ms on PostgreSQL.
WITH RECURSIVE slot (n, rec, rest) AS (
    SELECT 0, substr(CAST(@packed AS TEXT), 1, 99),
           substr(CAST(@packed AS TEXT), 100)
     WHERE 0 < CAST(@row_count AS BIGINT)
    UNION ALL
    SELECT n + 1, substr(rest, 1, 99), substr(rest, 100)
      FROM slot WHERE n + 1 < CAST(@row_count AS BIGINT)
)
INSERT INTO job_destinations
     (lease_id, ordinal, addr, sent_bytes, received_bytes, connections)
SELECT CAST(@lease_id AS TEXT),
       CAST(substr(rec, 1, 3) AS BIGINT),
       rtrim(substr(rec, 4, 39)),
       CAST(substr(rec, 43, 19) AS BIGINT),
       CAST(substr(rec, 62, 19) AS BIGINT),
       CAST(substr(rec, 81, 19) AS BIGINT)
  FROM slot;

-- name: ReadJobDestinations :many
-- One lease's destinations in the order the node gave them, the total beyond
-- them last.
SELECT ordinal, addr, sent_bytes, received_bytes, connections
  FROM job_destinations WHERE lease_id = @lease_id
 ORDER BY ordinal;
