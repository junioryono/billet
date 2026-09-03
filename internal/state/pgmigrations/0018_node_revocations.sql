-- migration 18: node_revocations, for PostgreSQL
--
-- The twin of migrations/0018_node_revocations.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE node_revocations (
			node           text PRIMARY KEY,
			-- Every certificate for this node valid from before this instant is
			-- refused. Stored as the certificate's own NotBefore basis, so the
			-- comparison is against a fact of the credential rather than a clock.
			revoked_before text NOT NULL,
			reason         text NOT NULL DEFAULT '',
			revoked_at     text NOT NULL
		);
-- +billet:end
