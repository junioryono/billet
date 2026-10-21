-- migration 64: runner_github_target, for PostgreSQL
--
-- The twin of migrations/0064_runner_github_target.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN github_target text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN github_target text NOT NULL DEFAULT '';
-- +billet:end
