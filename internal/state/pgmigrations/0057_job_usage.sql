-- migration 57: job_usage, for PostgreSQL
--
-- The twin of migrations/0057_job_usage.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE job_usage (
    lease_id text PRIMARY KEY NOT NULL,
    node text NOT NULL,
    recorded_at text NOT NULL,
    source text NOT NULL,
    unmeasured text NOT NULL DEFAULT '',
    samples bigint NOT NULL,
    interval_ms bigint NOT NULL,
    window_ms bigint NOT NULL,
    cpu_user_us bigint NOT NULL,
    cpu_system_us bigint NOT NULL,
    guest_cpu_us bigint NOT NULL,
    vmm_cpu_us bigint NOT NULL,
    memory_peak_bytes bigint NOT NULL,
    oom_kills bigint NOT NULL,
    disk_read_bytes bigint NOT NULL,
    disk_write_bytes bigint NOT NULL,
    net_rx_bytes bigint NOT NULL,
    net_tx_bytes bigint NOT NULL,
    net_rx_packets bigint NOT NULL,
    net_tx_packets bigint NOT NULL,
    cpu_some_us bigint NOT NULL,
    cpu_full_us bigint NOT NULL,
    memory_some_us bigint NOT NULL,
    memory_full_us bigint NOT NULL,
    io_some_us bigint NOT NULL,
    io_full_us bigint NOT NULL,
    energy_active_uj bigint NOT NULL,
    energy_idle_uj bigint NOT NULL,
    energy_source text NOT NULL DEFAULT ''
);
-- +billet:end

-- +billet:statement
CREATE TABLE job_series (
    lease_id text PRIMARY KEY NOT NULL,
    codec bigint NOT NULL,
    series text NOT NULL
);
-- +billet:end
