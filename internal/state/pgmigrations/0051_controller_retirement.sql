-- migration 51: controller_retirement, for PostgreSQL
--
-- The twin of migrations/0051_controller_retirement.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE controller_retirement (
	deployment    text PRIMARY KEY,
	retiring      text NOT NULL,
	survivor      text NOT NULL,
	run           text NOT NULL,
	state         text NOT NULL CHECK (state IN ('reserved', 'intent', 'done')),
	transition_id text NOT NULL,
	reserved_at   text NOT NULL,
	updated_at    text NOT NULL,
	completed_by  text NOT NULL DEFAULT '',
	completed_at  text NOT NULL DEFAULT ''
);
-- +billet:end
