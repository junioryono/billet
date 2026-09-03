-- migration 27: cache_interception_policy, for PostgreSQL
--
-- The twin of migrations/0027_cache_interception_policy.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE cache_interception_blocks (
			scope_type  text NOT NULL CHECK (scope_type IN ('org','repository')),
			owner       text NOT NULL CHECK (length(trim(owner)) > 0 AND owner = lower(owner)),
			repository  text NOT NULL DEFAULT '' CHECK (repository = lower(repository)),
			disabled_at text NOT NULL,
			PRIMARY KEY (scope_type, owner, repository),
			CHECK ((scope_type = 'org' AND repository = '') OR
			       (scope_type = 'repository' AND length(trim(repository)) > 0))
		);
-- +billet:end
