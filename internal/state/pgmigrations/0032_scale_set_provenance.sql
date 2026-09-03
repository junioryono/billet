-- migration 32: scale_set_provenance, for PostgreSQL
--
-- The twin of migrations/0032_scale_set_provenance.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE scale_set (
			org          text NOT NULL,
			runner_group text NOT NULL,
			label        text NOT NULL,
			scale_set_id bigint NOT NULL CHECK (scale_set_id > 0),
			created_at   text NOT NULL,
			PRIMARY KEY (org, runner_group, label)
		);
-- +billet:end
