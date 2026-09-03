-- migration 9: lease_provider_list, for PostgreSQL
--
-- The twin of migrations/0009_lease_provider_list.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN providers text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN chosen_provider text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
UPDATE leases SET providers = provider WHERE providers = '';
-- +billet:end

-- +billet:statement
UPDATE leases SET chosen_provider = provider WHERE node IS NOT NULL AND chosen_provider = '';
-- +billet:end
