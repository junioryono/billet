-- migration 60: job_guest_reports
--
-- WHAT THE AGENT INSIDE A JOB'S GUEST TOLD THE NODE, kept beside what the host
-- measured (migration 57) and never mixed into it. The node collects the
-- agent's batches while the job runs and, when the compute is destroyed,
-- reports them once as one opaque blob with what the node itself counted about
-- the channel. The job is root in its VM, so everything the agent said is the
-- guest's own unverified view: kept for a reader, and never an input to a
-- decision about capacity, fencing, identity, custody or destruction.
--
-- One row per lease, outliving the lease the way job_usage does. `data` is the
-- node's encoding of the batches (base64 so it is TEXT on both engines, at most
-- 256 KiB before it), with the `codec` that wrote it; the control plane never
-- decodes it. agent_version and agent_schema are what the agent said in its
-- hello, empty and zero when it said nothing. accepted and refused count the
-- batches the node took and would not take, and dropped_bytes what it took and
-- could not keep within the bound. hello, final_seen and node_restarted are
-- 0 or 1. first_received_at and last_received_at are on the NODE'S clock, and
-- NULL when no batch arrived, which is not a time.
--
-- THE FIRST REPORT IS KEPT, for job_usage's reason: a retry after a lost answer
-- carries the same bytes and must not replace what the ledger already holds.
--
-- recorded_at is the control plane's clock and is indexed, because the rows
-- are large and a retention job prunes them by age.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_guest_reports (
    lease_id TEXT PRIMARY KEY NOT NULL,
    node TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    agent_version TEXT NOT NULL,
    agent_schema INTEGER NOT NULL CHECK (agent_schema >= 0),
    codec INTEGER NOT NULL CHECK (codec >= 1),
    data TEXT NOT NULL,
    accepted INTEGER NOT NULL CHECK (accepted >= 0),
    refused INTEGER NOT NULL CHECK (refused >= 0),
    dropped_bytes INTEGER NOT NULL CHECK (dropped_bytes >= 0),
    hello INTEGER NOT NULL CHECK (hello IN (0, 1)),
    final_seen INTEGER NOT NULL CHECK (final_seen IN (0, 1)),
    node_restarted INTEGER NOT NULL CHECK (node_restarted IN (0, 1)),
    first_received_at TEXT,
    last_received_at TEXT,
    CHECK ((first_received_at IS NULL) = (last_received_at IS NULL))
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX job_guest_reports_recorded_at ON job_guest_reports (recorded_at)
-- +billet:end
