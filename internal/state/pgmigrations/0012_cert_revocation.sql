-- migration 12: cert_revocation, for PostgreSQL
--
-- The twin of migrations/0012_cert_revocation.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE revoked_certs (
			serial     text PRIMARY KEY,
			node       text NOT NULL,
			reason     text NOT NULL DEFAULT '',
			revoked_at text NOT NULL
		);
-- +billet:end

-- +billet:statement
CREATE INDEX idx_revoked_certs_node ON revoked_certs (node);
-- +billet:end
