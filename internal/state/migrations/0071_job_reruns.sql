-- migration 71: job_reruns
--
-- WHAT BILLET ASKED GITHUB TO RUN AGAIN, AND WHY IT DID OR DID NOT (#541).
--
-- job_reruns holds one request per attempt of a run, and the key is the rule: an
-- attempt is asked about at most once, however many of its jobs billet disrupted,
-- because one request re-runs them all. A row is written as `asking` BEFORE the
-- request leaves, in the transaction that decided it, and settled after the answer.
-- That order is what a crash needs: an `asking` row is a request that may have
-- landed, and only GitHub's record of the run can say, so it is settled by
-- reading the run rather than by asking again.
--
-- tries bounds it. A request that keeps failing settles `failed` and is reported,
-- and never retries forever. detail is GitHub's decoded message, or the class of
-- a transport failure: nothing a guest wrote.
--
-- job_history carries the outcome on the job, the row an operator and a report
-- read. rerun_decision is empty until decided, from a vocabulary closed in Go
-- rather than by a CHECK, for the reason migration 35 gives for a disruption:
-- SQLite cannot extend a column CHECK in place. rerun_attempt is the attempt the
-- job was part of when it was decided, zero for none; GitHub numbers them from 1.
--
-- The index serves the metric, which reads the decisions of the last day, and is
-- partial because a decision is rare.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_reruns (
    repo TEXT NOT NULL,
    run_id INTEGER NOT NULL CHECK (run_id > 0),
    run_attempt INTEGER NOT NULL CHECK (run_attempt > 0),
    kind TEXT NOT NULL,
    github_job_id INTEGER NOT NULL DEFAULT 0 CHECK (github_job_id >= 0),
    state TEXT NOT NULL,
    tries INTEGER NOT NULL DEFAULT 0 CHECK (tries >= 0),
    created_at TEXT NOT NULL,
    settled_at TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo, run_id, run_attempt)
) STRICT
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN rerun_decision TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN rerun_decided_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN rerun_attempt INTEGER NOT NULL DEFAULT 0 CHECK (rerun_attempt >= 0)
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_rerun_decided_idx ON job_history(rerun_decided_at)
			WHERE rerun_decision != ''
-- +billet:end
