-- migration 27: cache_interception_policy
--
-- Cache interception is accelerated data access rather than availability. A
-- central deny list lets an operator take one organisation or repository out of
-- the local path without replacing configuration on every compute host.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE cache_interception_blocks (
			scope_type  TEXT NOT NULL CHECK (scope_type IN ('org','repository')),
			owner       TEXT NOT NULL CHECK (length(trim(owner)) > 0 AND owner = lower(owner)),
			repository  TEXT NOT NULL DEFAULT '' CHECK (repository = lower(repository)),
			disabled_at TEXT NOT NULL,
			PRIMARY KEY (scope_type, owner, repository),
			CHECK ((scope_type = 'org' AND repository = '') OR
			       (scope_type = 'repository' AND length(trim(repository)) > 0))
		) STRICT
-- +billet:end
