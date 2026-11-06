-- Join tokens: the short-lived credential that lets a machine ASK to enroll.
--
-- The token is never stored. What the table holds is its sha256, which is why
-- every statement here takes a hash rather than a secret.

-- name: InsertJoinToken :exec
-- Mint one join token. pool_order is the order a pool minted it for, and empty for
-- one an operator minted (migration 76).
INSERT INTO join_tokens (token_sha256, note, uses_remaining, created_at, expires_at, pool_order)
VALUES (@token_sha256, @note, @uses_remaining, @created_at, @expires_at, @pool_order);

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

-- name: ReadJoinTokenOrder :one
-- The pool order a token was minted for, spent or not, and whether it could still be
-- spent: a host polls with the token it booted with long after the admission spent
-- it, and its poll is still the order's. Empty for an operator's token;
-- sql.ErrNoRows for no such token.
SELECT pool_order,
       CAST(CASE WHEN uses_remaining > 0 AND expires_at > @now THEN 1 ELSE 0 END AS BIGINT) AS live
  FROM join_tokens WHERE token_sha256 = @token_sha256;

-- name: SpendPoolJoinToken :execresult
-- Consume one use of a token, only while it is live AND minted for this order, in
-- one statement, so a token of one order never admits another's host.
UPDATE join_tokens SET uses_remaining = uses_remaining - 1
 WHERE token_sha256 = @token_sha256 AND pool_order = @pool_order
   AND uses_remaining > 0 AND expires_at > @now;

-- name: RetirePoolJoinTokens :exec
-- Spend every use left on an order's tokens, once a node has joined from it: each
-- send minted one, and only the one that booted the host was ever needed.
UPDATE join_tokens SET uses_remaining = 0
 WHERE pool_order = @pool_order AND uses_remaining > 0;
