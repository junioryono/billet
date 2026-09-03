-- migration 11: node_liveness, for PostgreSQL
--
-- The twin of migrations/0011_node_liveness.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN live bigint NOT NULL DEFAULT 0 CHECK (live IN (0, 1));
-- +billet:end
