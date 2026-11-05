-- migration 73: job_egress
--
-- WHAT THE HOST SAW OF A JOB'S NETWORK (#540, ADR-026): the names its guest asked
-- the node's resolver for, what those names answered, what a tier's allowlist
-- refused and how many packets it dropped. Kept beside what the host measured
-- (migration 57) and apart from it, because a list is enforced on a node that
-- samples nothing, and a job with no usage report still has this to say.
--
-- One row per lease, outliving the lease the way job_usage does. `report` is
-- lease.JobEgress as JSON, at most a few hundred KiB because the report's lists
-- are bounded, and nothing on the control plane decides from it: it is written once
-- and read back whole for a reader, so it has no columns of its own to search. The
-- guest chose every name in it, and lease.JobEgress's validation is what holds
-- those to bytes a terminal prints as themselves.
--
-- `listed` is the one fact outside it, so a reader can say which jobs ran under a
-- list without decoding every report.
--
-- THE FIRST REPORT IS KEPT, for job_usage's reason: a retry after a lost answer
-- carries the same report and must not replace what the ledger already holds.
--
-- recorded_at is the control plane's clock and is indexed, because the rows can be
-- large and the retention job reads them by age.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_egress (
    lease_id TEXT PRIMARY KEY NOT NULL,
    node TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    listed INTEGER NOT NULL CHECK (listed IN (0, 1)),
    report TEXT NOT NULL
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX job_egress_recorded_at ON job_egress (recorded_at)
-- +billet:end
