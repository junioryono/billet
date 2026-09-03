-- migration 45: deployment_binding, for PostgreSQL
--
-- The twin of migrations/0045_deployment_binding.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE deployment_binding (
	id            bigint PRIMARY KEY CHECK (id = 1),
	deployment_id text NOT NULL CHECK (deployment_id <> ''),
	bound_at      text NOT NULL
);
-- +billet:end
