-- migration 59: job_destinations, for PostgreSQL
--
-- The twin of migrations/0059_job_destinations.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE job_destinations (
    lease_id text NOT NULL,
    ordinal bigint NOT NULL CHECK (ordinal >= 0 AND ordinal <= 256),
    addr text NOT NULL,
    sent_bytes bigint NOT NULL CHECK (sent_bytes >= 0),
    received_bytes bigint NOT NULL CHECK (received_bytes >= 0),
    connections bigint NOT NULL CHECK (connections >= 0),
    PRIMARY KEY (lease_id, ordinal),
    UNIQUE (lease_id, addr),
    CHECK ((ordinal = 256 AND addr = '') OR (ordinal < 256 AND addr <> ''))
);
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN destinations_incomplete bigint CHECK (destinations_incomplete IN (0, 1));
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN tap_sent_bytes bigint CHECK (tap_sent_bytes >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN tap_received_bytes bigint CHECK (tap_received_bytes >= 0);
-- +billet:end
