-- migration 71: job_test_results
--
-- WHAT A JOB'S TESTS DID, KEPT PAST ITS GUEST REPORT, AND THE COMMIT THEY RAN ON
-- (#536). A guest report's suites are deleted with it at server.job_detail_retention,
-- and it names no case's duration or status, so nothing could say which test fails
-- most, which is slowest, or which failed and passed on one commit.
--
-- job_test_results is one row a lease, outliving the lease and written in the same
-- transaction as the guest report that carried it, so it is fenced on the same epoch
-- and the first report wins for both. Like a guest report it is THE GUEST'S OWN,
-- UNVERIFIED VIEW: the job is root in its VM, so `data` is kept for a reader and is
-- never an input to a decision. It is the node's encoding (base64 so it is TEXT on
-- both engines, at most 64 KiB before it), under the `codec` that wrote it, and the
-- control plane never decodes it.
--
-- encoded_bytes IS BEFORE data, AND IS A COLUMN AT ALL, so a retention totalling the
-- table's size reads each row's first page and never walks a row's overflow chain.
-- recorded_at is the control plane's clock and is indexed with the lease, because
-- the rows are read newest first in pages and deleted oldest first by that key.
--
-- head_sha and head_branch say which commit the job ran on, from GitHub's REST
-- record of it, which is where the queue time also comes from. The scale-set message
-- carries neither. Written once with the rest of the job's identity; the empty string
-- means not recorded, which every row from before this reads as. They are for people
-- to read and for ranking tests by commit, and decide nothing.
--
-- run_attempt is which attempt of its run the job was, from the same record: zero is
-- not recorded. A failure and a pass in one attempt are two jobs that ran side by
-- side, two legs of a matrix sharing a display name, and say nothing of a flaky test;
-- a re-run is a new attempt.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN head_sha TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN head_branch TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN run_attempt INTEGER NOT NULL DEFAULT 0 CHECK (run_attempt >= 0)
-- +billet:end

-- +billet:statement
CREATE TABLE job_test_results (
    lease_id TEXT PRIMARY KEY NOT NULL,
    recorded_at TEXT NOT NULL,
    codec INTEGER NOT NULL CHECK (codec >= 1),
    encoded_bytes INTEGER NOT NULL CHECK (encoded_bytes >= 0),
    data TEXT NOT NULL
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX job_test_results_recorded_at ON job_test_results (recorded_at, lease_id)
-- +billet:end
