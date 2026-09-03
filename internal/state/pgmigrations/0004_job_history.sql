-- migration 4: job_history, for PostgreSQL
--
-- The twin of migrations/0004_job_history.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE job_history (
				lease_id    text PRIMARY KEY,
				tier        text NOT NULL,
				node        text,
				run_id      bigint,
				job_id      bigint,
				repo        text,
				conclusion  text,
				queued_at   text NOT NULL,
				started_at  text,
				finished_at text
			);
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_queued_idx ON job_history(queued_at);
-- +billet:end
