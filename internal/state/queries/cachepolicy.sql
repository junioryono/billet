-- name: DeleteCacheBlock :exec
-- Re-enable one cache kind for one explicit scope by removing its block.
DELETE FROM cache_blocks
 WHERE kind = @kind AND scope_type = @scope_type AND owner = @owner
   AND repository = @repository;

-- name: UpsertCacheBlock :exec
-- Disable one cache kind for one explicit scope, refreshing when it was decided.
INSERT INTO cache_blocks (kind, scope_type, owner, repository, disabled_at)
VALUES (@kind, @scope_type, @owner, @repository, @disabled_at)
ON CONFLICT(kind, scope_type, owner, repository) DO UPDATE SET disabled_at = excluded.disabled_at;

-- name: CountCacheBlocks :one
-- Blocks covering one cache of one repository: its organisation's or its own,
-- for that cache or for every cache.
--
-- ONE QUERY FOR BOTH SCOPES AND BOTH KINDS, because the answer is "is any of
-- them blocked" and separate reads could straddle a write that added one after
-- another read said no.
SELECT COUNT(*) FROM cache_blocks
 WHERE (kind = @kind OR kind = '*') AND owner = @owner AND
       ((scope_type = 'org' AND repository = '') OR
        (scope_type = 'repository' AND repository = @repository));

-- name: ListCacheBlocks :many
-- Every block, for `billet cache status`.
SELECT kind, scope_type, owner, repository, disabled_at FROM cache_blocks
 ORDER BY owner, repository, kind;
