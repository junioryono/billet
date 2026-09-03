-- name: DeleteCacheBlock :exec
-- Re-enable interception for one explicit scope by removing its block.
DELETE FROM cache_interception_blocks
 WHERE scope_type = @scope_type AND owner = @owner AND repository = @repository;

-- name: UpsertCacheBlock :exec
-- Disable interception for one explicit scope, refreshing when it was decided.
INSERT INTO cache_interception_blocks (scope_type, owner, repository, disabled_at)
VALUES (@scope_type, @owner, @repository, @disabled_at)
ON CONFLICT(scope_type, owner, repository) DO UPDATE SET disabled_at = excluded.disabled_at;

-- name: CountCacheBlocks :one
-- Blocks covering one repository: its organisation's, or its own.
--
-- ONE QUERY FOR BOTH SCOPES, because the answer is "is either blocked" and two
-- reads could straddle a write that added the org block after the repository
-- read said no.
SELECT COUNT(*) FROM cache_interception_blocks
 WHERE owner = @owner AND ((scope_type = 'org' AND repository = '') OR
                           (scope_type = 'repository' AND repository = @repository));
