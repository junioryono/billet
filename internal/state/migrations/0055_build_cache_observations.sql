-- migration 55: build_cache_observations
--
-- WHAT EACH BUILD CACHE DID FOR A JOB, beside what the image store and the
-- Actions cache did (migration 49). Sticky disks, the Git mirror, and the Bazel
-- and Go content-addressed caches (#226) each get one column on the lease and
-- one on its history row, from one closed vocabulary the node writes from what
-- it saw: warm (the cache already held something the job used), cold (the job
-- used it and it held nothing the job used), disabled (the kill switch refused
-- it), unavailable (the store failed and the job went on without it), unused
-- (the session ended and the guest never asked). The empty string is the zero
-- value and means nothing was observed, which is also what a session a
-- restarted node recovered says about a cache it cannot vouch for.
--
-- THE FIRST OBSERVATION IS KEPT, as migration 49's are. The node reports these
-- once, when the job's session ends, so there is one observation to keep.
--
-- NO CHECK ON THE VOCABULARY, for the reason migration 35 gives: SQLite cannot
-- extend a column CHECK in place. The set is closed in Go by
-- alloc.BuildCache.Valid.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN sticky_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN git_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN bazel_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN go_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN sticky_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN git_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN bazel_cache TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN go_cache TEXT NOT NULL DEFAULT ''
-- +billet:end
