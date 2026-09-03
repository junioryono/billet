-- migration 44: controller_claim, for PostgreSQL
--
-- The twin of migrations/0044_controller_claim.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE controller_claim (
	id         bigint PRIMARY KEY CHECK (id = 1),
	holder     text NOT NULL,
	epoch      bigint NOT NULL CHECK (epoch > 0),
	claimed_at text NOT NULL
);
-- +billet:end
