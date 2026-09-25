-- migration 54: cache_blocks_by_kind
--
-- THE KILL SWITCH COVERS EVERY CACHE, ONE KIND AT A TIME. Migration 27's deny
-- list could take an organisation or repository out of the Actions cache only;
-- the Docker image store, sticky disks and the Git, Bazel and Go caches each
-- need the same switch (#226). A block now names the cache it covers, and '*'
-- covers every cache.
--
-- Every existing block is carried as kind 'actions', which is exactly what it
-- meant when it was written, so an upgrade changes no decision.
--
-- NO CHECK ON THE KIND VOCABULARY, for the reason migration 35 gives: SQLite
-- cannot extend a column CHECK in place. The set is closed in Go by
-- config.CacheKind.Valid and state's own '*'.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE cache_blocks (
			kind        TEXT NOT NULL CHECK (length(trim(kind)) > 0),
			scope_type  TEXT NOT NULL CHECK (scope_type IN ('org','repository')),
			owner       TEXT NOT NULL CHECK (length(trim(owner)) > 0 AND owner = lower(owner)),
			repository  TEXT NOT NULL DEFAULT '' CHECK (repository = lower(repository)),
			disabled_at TEXT NOT NULL,
			PRIMARY KEY (kind, scope_type, owner, repository),
			CHECK ((scope_type = 'org' AND repository = '') OR
			       (scope_type = 'repository' AND length(trim(repository)) > 0))
		) STRICT
-- +billet:end

-- +billet:statement
INSERT INTO cache_blocks (kind, scope_type, owner, repository, disabled_at)
		SELECT 'actions', scope_type, owner, repository, disabled_at
		  FROM cache_interception_blocks
-- +billet:end

-- +billet:statement
DROP TABLE cache_interception_blocks
-- +billet:end
