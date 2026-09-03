-- migration 42: rollout_converged_digest, for PostgreSQL
--
-- The twin of migrations/0042_rollout_converged_digest.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE rollout_nodes ADD COLUMN converged_digest text NOT NULL DEFAULT '';
-- +billet:end
