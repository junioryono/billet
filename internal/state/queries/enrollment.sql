-- Node enrollment: what has asked to join, and what an operator decided.
--
-- THE PROJECTIONS ARE IN TABLE DECLARATION ORDER on purpose. sqlc returns the
-- model struct for a query that selects exactly the table's columns in exactly
-- its order, and a Row type of its own for anything else -- so writing the three
-- full reads the same way gives all three one shared type and the adapter one
-- mapping function instead of three that must be kept identical.

-- name: ReadEnrollment :one
-- One request, by the name it claims.
SELECT name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source
  FROM node_enrollments WHERE name = @name;

-- name: ReadEnrollmentFingerprint :one
-- Just the key a name is currently admitted as.
--
-- SEPARATE FROM ReadEnrollment because the caller is about to overwrite the row
-- and needs only what it is displacing; reading the CSR and the certificate to
-- compare one column is the whole credential in memory for nothing.
SELECT fingerprint FROM node_enrollments WHERE name = @name;

-- name: InsertEnrollment :exec
-- Record a request for a name nothing holds.
INSERT INTO node_enrollments (name, fingerprint, csr_pem, state, requested_at)
VALUES (@name, @fingerprint, @csr_pem, @state, @requested_at);

-- name: ReplaceDeniedEnrollment :exec
-- Let a different key take a name a denial is holding.
--
-- ONLY FOR A DENIED ROW, which the caller checks: pending and approved still
-- hold the name, because an operator who compared a fingerprint yesterday must
-- not be approving a different machine today under a name they already trust. A
-- denial holds nothing, because the enrolling process keeps its private key in
-- memory while it waits for a human -- so a reboot loses the key and, without
-- this, the machine that comes back is refused forever.
UPDATE node_enrollments
   SET fingerprint = @fingerprint, csr_pem = @csr_pem, cert_pem = '', state = @state,
       source = 'enrolled', requested_at = @requested_at, decided_at = ''
 WHERE name = @name;

-- name: DecideEnrollment :exec
-- Record an operator's approval or denial.
UPDATE node_enrollments
   SET state = @state, cert_pem = @cert_pem, decided_at = @decided_at
 WHERE name = @name;

-- name: UpsertIssuedEnrollment :exec
-- Record a certificate handed out directly by `billet ca issue`.
--
-- IT OVERWRITES, unlike the wire, and that is deliberate: issuing is a
-- deliberate operator act and refusing would leave a name unusable after a
-- machine was rebuilt. csr_pem is left alone by the update because this path
-- never had one; requested_at likewise keeps the first admission's timestamp.
INSERT INTO node_enrollments
     (name, fingerprint, csr_pem, cert_pem, state, source, requested_at, decided_at)
VALUES (@name, @fingerprint, '', @cert_pem, @state, 'issued', @requested_at, @decided_at)
ON CONFLICT (name) DO UPDATE SET
  fingerprint = excluded.fingerprint,
  cert_pem    = excluded.cert_pem,
  state       = excluded.state,
  source      = excluded.source,
  decided_at  = excluded.decided_at;

-- name: ListEnrollments :many
-- Everything that has asked to join.
SELECT name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source
  FROM node_enrollments ORDER BY requested_at;

-- name: ListEnrollmentsInState :many
-- Everything in one state.
--
-- A SECOND STATEMENT RATHER THAN one query whose state filter is skipped when
-- the parameter is empty, so each has one plan and the branch is visible in the
-- Go that chooses it. An unfiltered
-- read of this table is the operator's whole pending list; a filtered one is a
-- lookup, and folding them into one query hides which is running.
SELECT name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source
  FROM node_enrollments WHERE state = @state ORDER BY requested_at;
