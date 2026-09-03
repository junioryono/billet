-- migration 36: pending_completion_lease_index, for PostgreSQL
--
-- The twin of migrations/0036_pending_completion_lease_index.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE INDEX pending_completions_lease_idx ON pending_completions(lease_id);
-- +billet:end
