-- migration 46: codebuild_registration_sweep, for PostgreSQL
--
-- The twin of migrations/0046_codebuild_registration_sweep.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN codebuild_jit_path text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN codebuild_region text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
CREATE TABLE credential_sweeps (
	region        text NOT NULL,
	path          text NOT NULL,
	swept_at      text NOT NULL,
	removed       bigint NOT NULL CHECK (removed >= 0),
	removed_total bigint NOT NULL CHECK (removed_total >= 0),
	kept          bigint NOT NULL CHECK (kept >= 0),
	unaccounted   bigint NOT NULL CHECK (unaccounted >= 0),
	foreign_names bigint NOT NULL CHECK (foreign_names >= 0),
	error         text NOT NULL DEFAULT '',
	PRIMARY KEY (region, path)
);
-- +billet:end
