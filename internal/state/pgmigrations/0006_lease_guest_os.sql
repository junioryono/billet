-- migration 6: lease_guest_os, for PostgreSQL
--
-- The twin of migrations/0006_lease_guest_os.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN guest_os text NOT NULL DEFAULT 'linux';
-- +billet:end
