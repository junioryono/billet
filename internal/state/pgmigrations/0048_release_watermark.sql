-- migration 48: release_watermark, for PostgreSQL
--
-- The twin of migrations/0048_release_watermark.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE release_watermark (
	id          bigint PRIMARY KEY CHECK (id = 1),
	release     text NOT NULL CHECK (release <> ''),
	recorded_at text NOT NULL
);
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN highest_release text NOT NULL DEFAULT '';
-- +billet:end
