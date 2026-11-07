-- migration 81: job_history_node, for PostgreSQL
--
-- The twin of migrations/0081_job_history_node.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE INDEX job_history_node_finished_idx ON job_history (node, finished_at);
-- +billet:end
