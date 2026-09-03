-- migration 34: node_wire_version, for PostgreSQL
--
-- The twin of migrations/0034_node_wire_version.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN node_release text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN wire_min bigint NOT NULL DEFAULT 0 CHECK (wire_min >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN wire_max bigint NOT NULL DEFAULT 0 CHECK (wire_max >= 0);
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN wire_version bigint NOT NULL DEFAULT 0
			CHECK (wire_version >= 0);
-- +billet:end
