-- migration 13: node_enrollment, for PostgreSQL
--
-- The twin of migrations/0013_node_enrollment.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE node_enrollments (
			name         text PRIMARY KEY,
			fingerprint  text NOT NULL,
			csr_pem      text NOT NULL,
			cert_pem     text NOT NULL DEFAULT '',
			state        text NOT NULL CHECK (state IN ('pending','approved','denied')),
			requested_at text NOT NULL,
			decided_at   text NOT NULL DEFAULT ''
		);
-- +billet:end
