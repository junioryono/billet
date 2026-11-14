-- migration 83: job_destination_openings
--
-- A JOB'S DESTINATIONS SPLIT BY WHEN EACH CONNECTION OPENED, beside the totals
-- migration 59 keeps, so a report can say which step of the job talked to which
-- destination: per destination, per bucket of width_seconds, from the host's
-- connection tracker. A connection's bytes are all in the bucket it opened in,
-- since the tracker keeps a connection's totals and not their times.
--
-- ONE ROW A JOB, THE OPENINGS PACKED, because a job keeps up to 1,024 of them
-- (lease.MaxDestinationOpenings) and a statement a row would hold the ledger's
-- only writer for as many round trips; migration 59's queries say what that
-- cost was measured to be. The packing is internal/alloc packOpenings' and is
-- read back and validated there, as the report was validated before it was
-- written.
--
-- NO ROW IS NOT A JOB THAT TALKED TO NOTHING: every job from before this
-- migration, every report sent over a wire before VersionDestinationOpenings,
-- and every job whose flows were not totalled has none, and a reader says the
-- split is not there. Retired with the job's other detail, by the same stamp.
--
-- NO FOREIGN KEY ONTO job_history, for the reason migration 82 gives.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_destination_openings (
    lease_id TEXT NOT NULL PRIMARY KEY,
    width_seconds INTEGER NOT NULL CHECK (width_seconds >= 1),
    row_count INTEGER NOT NULL CHECK (row_count >= 0),
    packed TEXT NOT NULL
) STRICT
-- +billet:end
