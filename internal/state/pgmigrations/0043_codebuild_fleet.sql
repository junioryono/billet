-- migration 43: codebuild_fleet, for PostgreSQL
--
-- The twin of migrations/0043_codebuild_fleet.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN codebuild_fleet text NOT NULL DEFAULT '';
-- +billet:end
