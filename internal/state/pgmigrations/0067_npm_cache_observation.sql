-- migration 67: npm_cache_observation
--
-- WHAT THE NPM CACHE DID FOR A JOB (ADR-020, #631), beside what each build cache
-- did (migration 55). One column on the lease and one on its history row, in the
-- build caches' closed vocabulary, which the node writes from what it saw: warm
-- (the store answered something the job asked for), cold (the job asked and the
-- store held nothing of it), disabled (the kill switch refused it), unavailable
-- (the store or the registry failed and the job went on without it), unused (the
-- tier had the cache and the guest never asked). The empty string is the zero
-- value and means nothing was observed, which is also what a session a restarted
-- node recovered says about a cache it cannot vouch for.
--
-- THE FIRST OBSERVATION IS KEPT, as migration 55's are: the node reports it once,
-- when the job's session ends.
--
-- NO CHECK ON THE VOCABULARY, for the reason migration 35 gives: SQLite cannot
-- extend a column CHECK in place. The set is closed in Go by
-- alloc.BuildCache.Valid.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN npm_cache text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN npm_cache text NOT NULL DEFAULT '';
-- +billet:end
