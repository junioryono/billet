-- migration 4: job_history
--
-- Retained after a lease is reaped, for the dashboard and for answering
-- "why did this queue".
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE job_history (
				lease_id    TEXT PRIMARY KEY,
				tier        TEXT NOT NULL,
				node        TEXT,
				run_id      INTEGER,
				job_id      INTEGER,
				repo        TEXT,
				conclusion  TEXT,
				queued_at   TEXT NOT NULL,
				started_at  TEXT,
				finished_at TEXT
			) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_queued_idx ON job_history(queued_at)
-- +billet:end
