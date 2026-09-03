-- migration 35: job_disruption
--
-- WHAT BILLET'S OWN INFRASTRUCTURE DID TO A LEASE, and what GitHub concluded
-- about the job that lease was running. Two facts, recorded separately, so that
-- "the failure was the node rather than your code" is something an operator
-- READS rather than something billet asserts.
--
-- `disruption` is a token from a closed vocabulary written AT THE MOMENT THE
-- DISRUPTION IS OBSERVED, in the transaction that observes it — never
-- reconstructed at completion from a snapshot. That distinction is the whole
-- design: `nodes.live` is 0 for the entire fleet immediately after a control
-- plane starts (ForgetEveryNode), so a verdict derived later from liveness
-- blames an unreachable host for every failure replayed across a restart.
--
-- SEPARATE FROM failure_reason, which is NOT available for this. A lease
-- carrying a failure reason is turned into outcome=failed, discard=true,
-- phase=teardown by node.adoptWithObservation — so borrowing that column to
-- record an attribution would destroy a guest that is still running a job.
--
-- `result` is GitHub's own conclusion, and `result_at` is when this control
-- plane learned it. result_at rather than finished_at because finished_at is
-- written when the LEASE terminalizes, which stays NULL for as long as a
-- destroy is retrying — and a job whose teardown is wedged is exactly one worth
-- reporting.
--
-- NO CHECK ON THE VOCABULARY, unlike phase. SQLite cannot extend a column CHECK
-- in place, so one here would make every future evidence source a rebuild of
-- two unbounded tables — what migration 20 had to do for phase. The set is
-- closed in Go by alloc.Disruption.Valid(), which every new OBSERVATION goes
-- through (an archive carries whatever is already on the row, including a token
-- a newer binary wrote), and every reader renders an unrecognised token verbatim
-- rather than dropping it.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN disruption TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN disrupted_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN disruption TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN disrupted_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN result TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN result_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- PARTIAL, because job_history is unbounded and a disruption is rare.
-- The report windows on result_at, so that is what it orders by.
-- +billet:statement
CREATE INDEX job_history_disrupted_idx ON job_history(result_at)
			WHERE disruption != ''
-- +billet:end
