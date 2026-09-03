-- migration 3: cache_generations, for PostgreSQL
--
-- The twin of migrations/0003_cache_generations.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
CREATE TABLE cache_generations (
				node       text NOT NULL,
				store      text NOT NULL,
				cache_key  text NOT NULL,
				generation text NOT NULL,
				updated_at text NOT NULL,
				PRIMARY KEY (node, store, cache_key)
			);
-- +billet:end
