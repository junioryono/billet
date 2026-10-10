-- migration 60: job_guest_reports, for PostgreSQL
--
-- The twin of migrations/0060_job_guest_reports.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE job_guest_reports (
    lease_id text PRIMARY KEY NOT NULL,
    node text NOT NULL,
    recorded_at text NOT NULL,
    agent_version text NOT NULL,
    agent_schema bigint NOT NULL CHECK (agent_schema >= 0),
    codec bigint NOT NULL CHECK (codec >= 1),
    data text NOT NULL,
    accepted bigint NOT NULL CHECK (accepted >= 0),
    refused bigint NOT NULL CHECK (refused >= 0),
    dropped_bytes bigint NOT NULL CHECK (dropped_bytes >= 0),
    hello bigint NOT NULL CHECK (hello IN (0, 1)),
    final_seen bigint NOT NULL CHECK (final_seen IN (0, 1)),
    node_restarted bigint NOT NULL CHECK (node_restarted IN (0, 1)),
    first_received_at text,
    last_received_at text,
    CHECK ((first_received_at IS NULL) = (last_received_at IS NULL))
);
-- +billet:end

-- +billet:statement
CREATE INDEX job_guest_reports_recorded_at ON job_guest_reports (recorded_at);
-- +billet:end
