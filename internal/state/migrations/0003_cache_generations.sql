-- migration 3: cache_generations
--
-- Advisory only. Authoritative generation pointers live on the owning
-- node, because a commit here cannot be atomic with a remote snapshot.
-- This exists so the scheduler can prefer a node that already holds a
-- warm generation for the cache key a job will want.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE cache_generations (
				node       TEXT NOT NULL,
				store      TEXT NOT NULL,
				cache_key  TEXT NOT NULL,
				generation TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				PRIMARY KEY (node, store, cache_key)
			) STRICT
-- +billet:end
