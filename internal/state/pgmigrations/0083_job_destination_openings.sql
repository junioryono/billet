-- migration 83: job_destination_openings, for PostgreSQL
--
-- The twin of migrations/0083_job_destination_openings.sql, which says why the
-- table is shaped as it is: a job's destinations split by when each connection
-- opened, one row a job with the openings packed, and no row for a job whose
-- split was never sent.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration refuses to
-- open.

-- +billet:statement
CREATE TABLE job_destination_openings (
    lease_id text NOT NULL PRIMARY KEY,
    width_seconds bigint NOT NULL CHECK (width_seconds >= 1),
    row_count bigint NOT NULL CHECK (row_count >= 0),
    packed text NOT NULL
);
-- +billet:end
