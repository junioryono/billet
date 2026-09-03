-- migration 31: admission, for PostgreSQL
--
-- The twin of migrations/0031_admission.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE admission (
			id         bigint PRIMARY KEY CHECK (id = 1),
			mode       text NOT NULL CHECK (mode IN ('open','sealed')),
			generation bigint NOT NULL CHECK (generation >= 0),
			provenance text NOT NULL CHECK (provenance IN ('','local-down','operator')),
			reason     text NOT NULL,
			actor      text NOT NULL,
			changed_at text NOT NULL
		);
-- +billet:end

-- +billet:statement
INSERT INTO admission (id, mode, generation, provenance, reason, actor, changed_at)
		 VALUES (1, 'open', 0, '', '', '', '');
-- +billet:end
