-- migration 59: job_destinations
--
-- WHERE A JOB'S TRAFFIC WENT, beside what migration 57 keeps of how much there
-- was. With node.monitoring.flows a Firecracker node totals each job's traffic
-- by destination from the host's connection tracker and reports it once with the
-- rest of the usage summary: up to 256 destinations by name, largest first, and
-- one total for every destination beyond them.
--
-- job_destinations is one row per named destination, its `ordinal` the place
-- the node gave it (0 is the largest), plus ONE ROW FOR THE REST, which is the
-- row whose address is empty and whose ordinal is 256. The primary key on
-- (lease_id, ordinal) and the bound on ordinal hold a job to 257 rows whatever a
-- node sends; the unique key on the address keeps one destination from being
-- counted on two rows.
--
-- THE VERDICT IS ON job_usage, not on a table of its own, because the
-- destinations ride the usage report and the first report is the one kept:
-- written in the same INSERT as the row it describes, the verdict cannot exist
-- without that row or be written by a report that lost. destinations_incomplete
-- is NULL WHEN THE FLOWS WERE NOT TOTALLED, which is not a job that sent
-- nothing: every row from before this migration, every node without flows, and
-- every job whose flows could not be followed. 1 says flows may have been
-- missed and every total is a lower bound. tap_sent_bytes and tap_received_bytes
-- are the guest's tap's totals less each packet's Ethernet header, NULL when the
-- tap was not read; they are read a moment before the flows, so their
-- difference from the destinations' sum is a comparison, not a measurement.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_destinations (
    lease_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0 AND ordinal <= 256),
    addr TEXT NOT NULL,
    sent_bytes INTEGER NOT NULL CHECK (sent_bytes >= 0),
    received_bytes INTEGER NOT NULL CHECK (received_bytes >= 0),
    connections INTEGER NOT NULL CHECK (connections >= 0),
    PRIMARY KEY (lease_id, ordinal),
    UNIQUE (lease_id, addr),
    CHECK ((ordinal = 256 AND addr = '') OR (ordinal < 256 AND addr <> ''))
) STRICT
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN destinations_incomplete INTEGER CHECK (destinations_incomplete IN (0, 1))
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN tap_sent_bytes INTEGER CHECK (tap_sent_bytes >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN tap_received_bytes INTEGER CHECK (tap_received_bytes >= 0)
-- +billet:end
