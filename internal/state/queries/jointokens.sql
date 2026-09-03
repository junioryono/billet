-- Join tokens: the short-lived credential that lets a machine ASK to enroll.
--
-- The token is never stored. What the table holds is its sha256, which is why
-- every statement here takes a hash rather than a secret.

-- name: InsertJoinToken :exec
-- Mint one join token.
INSERT INTO join_tokens (token_sha256, note, uses_remaining, created_at, expires_at)
VALUES (@token_sha256, @note, @uses_remaining, @created_at, @expires_at);

-- name: SpendJoinToken :execresult
-- Check a token and consume one use, in one statement.
--
-- CHECK AND DECREMENT TOGETHER, so two machines racing on a single-use token
-- cannot both be admitted: this matches only while a use remains and the token
-- is unexpired, and whichever commits second changes no rows. The caller reads
-- RowsAffected, which is why this is :execresult and not :exec -- an :exec
-- returns nothing to distinguish "spent" from "there was nothing to spend".
UPDATE join_tokens SET uses_remaining = uses_remaining - 1
 WHERE token_sha256 = @token_sha256 AND uses_remaining > 0 AND expires_at > @now;

-- name: ListJoinTokens :many
-- What is outstanding, without the secrets.
SELECT note, uses_remaining, created_at, expires_at FROM join_tokens
 ORDER BY created_at DESC;

-- name: ListJoinTokenHashes :many
-- Every key the table holds, for the test that says the secret is not among
-- them.
SELECT token_sha256 FROM join_tokens;

-- name: LiveJoinTokenExists :one
-- Whether a token with this hash could possibly be spent right now.
--
-- IT AUTHORISES NOTHING. Someone else may spend the last use a microsecond
-- later, and the enrollment transaction is what actually decides; this exists
-- only to keep a caller with no usable token off the single writer connection.
SELECT EXISTS(SELECT 1 FROM join_tokens
               WHERE token_sha256 = @token_sha256 AND uses_remaining > 0
                 AND expires_at > @now);
