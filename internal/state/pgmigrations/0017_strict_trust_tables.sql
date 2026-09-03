-- migration 17: strict_trust_tables, for PostgreSQL
--
-- The twin of migrations/0017_strict_trust_tables.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE revoked_certs_new (
			serial     text PRIMARY KEY,
			node       text NOT NULL,
			reason     text NOT NULL DEFAULT '',
			revoked_at text NOT NULL
		);
-- +billet:end

-- +billet:statement
INSERT INTO revoked_certs_new (serial, node, reason, revoked_at)
		 SELECT serial, node, reason, revoked_at FROM revoked_certs;
-- +billet:end

-- +billet:statement
DROP TABLE revoked_certs;
-- +billet:end

-- +billet:statement
ALTER TABLE revoked_certs_new RENAME TO revoked_certs;
-- +billet:end

-- +billet:statement
CREATE INDEX idx_revoked_certs_node ON revoked_certs (node);
-- +billet:end

-- +billet:statement
CREATE TABLE node_enrollments_new (
			name         text PRIMARY KEY,
			fingerprint  text NOT NULL,
			csr_pem      text NOT NULL,
			cert_pem     text NOT NULL DEFAULT '',
			state        text NOT NULL CHECK (state IN ('pending','approved','denied')),
			requested_at text NOT NULL,
			decided_at   text NOT NULL DEFAULT '',
			source       text NOT NULL DEFAULT 'enrolled'
		);
-- +billet:end

-- +billet:statement
INSERT INTO node_enrollments_new
		   (name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source)
		 SELECT name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source
		   FROM node_enrollments;
-- +billet:end

-- +billet:statement
DROP TABLE node_enrollments;
-- +billet:end

-- +billet:statement
ALTER TABLE node_enrollments_new RENAME TO node_enrollments;
-- +billet:end

-- +billet:statement
CREATE TABLE join_tokens_new (
			token_sha256   text PRIMARY KEY,
			note           text NOT NULL DEFAULT '',
			uses_remaining bigint NOT NULL,
			created_at     text NOT NULL,
			expires_at     text NOT NULL
		);
-- +billet:end

-- +billet:statement
INSERT INTO join_tokens_new
		   (token_sha256, note, uses_remaining, created_at, expires_at)
		 SELECT token_sha256, note, uses_remaining, created_at, expires_at FROM join_tokens;
-- +billet:end

-- +billet:statement
DROP TABLE join_tokens;
-- +billet:end

-- +billet:statement
ALTER TABLE join_tokens_new RENAME TO join_tokens;
-- +billet:end
