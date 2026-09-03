-- Certificates this deployment issued, and the ones it has taken back.
--
-- TWO REVOCATION MECHANISMS, and both are here because neither is sufficient
-- alone: a serial names one credential, which is the right granularity because a
-- node name is legitimately re-issued to a replacement machine; a per-node cutoff
-- reaches the credentials billet never recorded, which is the only handle a
-- deployment upgraded from a version that did not record serials has.

-- name: RecordIssuedCert :exec
-- Write down a credential at the moment it is handed out.
--
-- DO NOTHING rather than DO UPDATE: a serial identifies one credential, so a
-- second insert under the same serial is a retry rather than a new fact.
INSERT INTO issued_certs (serial, node, source, not_after, issued_at)
VALUES (@serial, @node, @source, @not_after, @issued_at)
ON CONFLICT (serial) DO NOTHING;

-- name: RevokeCert :exec
-- Withdraw one certificate, idempotently.
--
-- Revoking twice is not an error: an operator who is not sure whether the first
-- attempt landed must be able to run it again.
INSERT INTO revoked_certs (serial, node, reason, revoked_at)
VALUES (@serial, @node, @reason, @revoked_at)
ON CONFLICT (serial) DO NOTHING;

-- name: CertRevocation :one
-- Whether this serial has been withdrawn.
--
-- ABSENCE IS sql.ErrNoRows, which the caller separates from a read error. A
-- database fault must not answer "not revoked": that would make an unreadable
-- ledger equivalent to an empty one, which is the whole check switched off by a
-- transient fault.
SELECT CAST(1 AS BIGINT) FROM revoked_certs WHERE serial = @serial;

-- name: ListRevokedCerts :many
-- What has been withdrawn, newest first.
SELECT serial, node, reason, revoked_at FROM revoked_certs ORDER BY revoked_at DESC;

-- name: LiveCertsFor :many
-- The credentials one node holds that are neither expired nor already revoked.
SELECT i.serial, i.node, i.source, i.not_after, i.issued_at
  FROM issued_certs i
  LEFT JOIN revoked_certs r ON r.serial = i.serial
 WHERE i.node = @node AND r.serial IS NULL AND i.not_after > @now
 ORDER BY i.issued_at DESC;

-- name: RecordNodeRevocation :exec
-- Refuse every certificate for this name minted before an instant.
--
-- UPSERT, because a node revoked twice moves its cutoff forward. The reason and
-- the timestamp move with it, so the row describes the revocation in force
-- rather than the first one ever recorded.
INSERT INTO node_revocations (node, revoked_before, reason, revoked_at)
VALUES (@node, @revoked_before, @reason, @revoked_at)
ON CONFLICT (node) DO UPDATE SET
  revoked_before = excluded.revoked_before,
  reason         = excluded.reason,
  revoked_at     = excluded.revoked_at;

-- name: NodeRevocationCutoff :one
-- The instant before which every certificate for this name is refused.
--
-- sql.ErrNoRows means no cutoff was ever recorded for the node, which is not the
-- same as a cutoff of zero and is why this is not a COALESCE.
SELECT revoked_before FROM node_revocations WHERE node = @node;
