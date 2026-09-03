-- migration 47: lease_holder_incarnation, for PostgreSQL
--
-- The twin of migrations/0047_lease_holder_incarnation.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN incarnation text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN holder_incarnation text NOT NULL DEFAULT '';
-- +billet:end
