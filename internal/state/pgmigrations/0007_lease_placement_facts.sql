-- migration 7: lease_placement_facts, for PostgreSQL
--
-- The twin of migrations/0007_lease_placement_facts.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
UPDATE leases SET guest_os = 'macos' WHERE macos_slot = 1;
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN provider text NOT NULL DEFAULT '';
-- +billet:end
