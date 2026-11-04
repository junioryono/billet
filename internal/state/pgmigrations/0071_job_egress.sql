-- migration 71: job_egress, for PostgreSQL
--
-- The twin of migrations/0071_job_egress.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE job_egress (
    lease_id text PRIMARY KEY NOT NULL,
    node text NOT NULL,
    recorded_at text NOT NULL,
    listed bigint NOT NULL CHECK (listed IN (0, 1)),
    report text NOT NULL
);
-- +billet:end

-- +billet:statement
CREATE INDEX job_egress_recorded_at ON job_egress (recorded_at);
-- +billet:end
