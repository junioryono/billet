-- migration 15: issued_certs, for PostgreSQL
--
-- The twin of migrations/0015_issued_certs.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE issued_certs (
			serial     text PRIMARY KEY,
			node       text NOT NULL,
			-- enrolled | issued | renewed: how this credential came to exist,
			-- which is what an operator reads when deciding what to take back.
			source     text NOT NULL,
			not_after  text NOT NULL,
			issued_at  text NOT NULL
		);
-- +billet:end

-- +billet:statement
CREATE INDEX idx_issued_certs_node ON issued_certs (node);
-- +billet:end
