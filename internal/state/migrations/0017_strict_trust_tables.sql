-- migration 17: strict_trust_tables
--
-- STRICT, LIKE EVERY OTHER TABLE. SQLite's default typing accepts a string where
-- an integer belongs and stores it as one, so a bug that writes the wrong type
-- is discovered by a later reader rather than by the write. Three tables added
-- during the trust work were declared without it, and consistency here is worth
-- more than the tables are large: they hold credentials and admission decisions,
-- which are exactly the rows worth being strict about.
--
-- A rebuild, because STRICT is a property of the table declaration.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE revoked_certs_new (
			serial     TEXT PRIMARY KEY,
			node       TEXT NOT NULL,
			reason     TEXT NOT NULL DEFAULT '',
			revoked_at TEXT NOT NULL
		) STRICT
-- +billet:end

-- +billet:statement
INSERT INTO revoked_certs_new (serial, node, reason, revoked_at)
		 SELECT serial, node, reason, revoked_at FROM revoked_certs
-- +billet:end

-- +billet:statement
DROP TABLE revoked_certs
-- +billet:end

-- +billet:statement
ALTER TABLE revoked_certs_new RENAME TO revoked_certs
-- +billet:end

-- +billet:statement
CREATE INDEX idx_revoked_certs_node ON revoked_certs (node)
-- +billet:end

-- +billet:statement
CREATE TABLE node_enrollments_new (
			name         TEXT PRIMARY KEY,
			fingerprint  TEXT NOT NULL,
			csr_pem      TEXT NOT NULL,
			cert_pem     TEXT NOT NULL DEFAULT '',
			state        TEXT NOT NULL CHECK (state IN ('pending','approved','denied')),
			requested_at TEXT NOT NULL,
			decided_at   TEXT NOT NULL DEFAULT '',
			source       TEXT NOT NULL DEFAULT 'enrolled'
		) STRICT
-- +billet:end

-- +billet:statement
INSERT INTO node_enrollments_new
		   (name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source)
		 SELECT name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source
		   FROM node_enrollments
-- +billet:end

-- +billet:statement
DROP TABLE node_enrollments
-- +billet:end

-- +billet:statement
ALTER TABLE node_enrollments_new RENAME TO node_enrollments
-- +billet:end

-- +billet:statement
CREATE TABLE join_tokens_new (
			token_sha256   TEXT PRIMARY KEY,
			note           TEXT NOT NULL DEFAULT '',
			uses_remaining INTEGER NOT NULL,
			created_at     TEXT NOT NULL,
			expires_at     TEXT NOT NULL
		) STRICT
-- +billet:end

-- +billet:statement
INSERT INTO join_tokens_new
		   (token_sha256, note, uses_remaining, created_at, expires_at)
		 SELECT token_sha256, note, uses_remaining, created_at, expires_at FROM join_tokens
-- +billet:end

-- +billet:statement
DROP TABLE join_tokens
-- +billet:end

-- +billet:statement
ALTER TABLE join_tokens_new RENAME TO join_tokens
-- +billet:end
