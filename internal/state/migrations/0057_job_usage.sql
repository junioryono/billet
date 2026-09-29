-- migration 57: job_usage
--
-- WHAT A JOB DID TO THE HOST, measured by the host. The node samples the
-- microVM's or container's cgroup, its tap and the VMM's threads, and the
-- package energy counter, for the whole life of the lease, and reports a
-- summary and a compressed series once, before the compute is destroyed. The
-- host is the source of truth: none of it comes from inside the guest, where a
-- job running as root could forge it.
--
-- job_usage is one row per lease, beside job_history and outliving the lease
-- the same way. Every quantity is an integer in the unit its name says (µs,
-- bytes, µJ). A quantity the host could not measure is ZERO AND NAMED in
-- `unmeasured` (a comma-separated closed vocabulary: cpu, memory, io, net,
-- threads, pressure, energy), because zero is also a real measurement and a
-- reader must be able to tell them apart. energy_source says how energy was
-- attributed ('' when it was not measured).
--
-- job_series holds the per-sample series as one opaque blob per lease (delta
-- encoded, compressed, base64 so it is TEXT on both engines), with the codec
-- that wrote it. A job sampled once a second for twenty minutes is about 24,000
-- points; as rows that is 120 million a day at 5,000 jobs, as blobs it is one
-- row per job.
--
-- THE FIRST REPORT IS KEPT. A node reports once per lease and a retry after a
-- lost answer carries the same bytes, so a second write is ignored rather than
-- allowed to replace the first.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_usage (
    lease_id TEXT PRIMARY KEY NOT NULL,
    node TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    source TEXT NOT NULL,
    unmeasured TEXT NOT NULL DEFAULT '',
    samples INTEGER NOT NULL,
    interval_ms INTEGER NOT NULL,
    window_ms INTEGER NOT NULL,
    cpu_user_us INTEGER NOT NULL,
    cpu_system_us INTEGER NOT NULL,
    guest_cpu_us INTEGER NOT NULL,
    vmm_cpu_us INTEGER NOT NULL,
    memory_peak_bytes INTEGER NOT NULL,
    oom_kills INTEGER NOT NULL,
    disk_read_bytes INTEGER NOT NULL,
    disk_write_bytes INTEGER NOT NULL,
    net_rx_bytes INTEGER NOT NULL,
    net_tx_bytes INTEGER NOT NULL,
    net_rx_packets INTEGER NOT NULL,
    net_tx_packets INTEGER NOT NULL,
    cpu_some_us INTEGER NOT NULL,
    cpu_full_us INTEGER NOT NULL,
    memory_some_us INTEGER NOT NULL,
    memory_full_us INTEGER NOT NULL,
    io_some_us INTEGER NOT NULL,
    io_full_us INTEGER NOT NULL,
    energy_active_uj INTEGER NOT NULL,
    energy_idle_uj INTEGER NOT NULL,
    energy_source TEXT NOT NULL DEFAULT ''
) STRICT
-- +billet:end

-- +billet:statement
CREATE TABLE job_series (
    lease_id TEXT PRIMARY KEY NOT NULL,
    codec INTEGER NOT NULL,
    series TEXT NOT NULL
) STRICT
-- +billet:end
