-- migration 66: tier_releases, for PostgreSQL
--
-- The twin of migrations/0066_tier_releases.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE tier_releases (
    tier text PRIMARY KEY NOT NULL,
    generation bigint NOT NULL
);
-- +billet:end
