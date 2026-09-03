-- migration 14: join_tokens, for PostgreSQL
--
-- The twin of migrations/0014_join_tokens.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE join_tokens (
			token_sha256   text PRIMARY KEY,
			note           text NOT NULL DEFAULT '',
			uses_remaining bigint NOT NULL,
			created_at     text NOT NULL,
			expires_at     text NOT NULL
		);
-- +billet:end

-- +billet:statement
ALTER TABLE node_enrollments ADD COLUMN source text NOT NULL DEFAULT 'enrolled';
-- +billet:end
