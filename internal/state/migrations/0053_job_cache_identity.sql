-- migration 53: job_cache_identity
--
-- WHAT GITHUB SAID ABOUT THE JOB, KEPT WHERE A CACHE AUTHORITY CAN READ IT.
--
-- A cache may publish only for a job proved to run on its repository's default
-- branch under an event GitHub itself lets write there (#226). The evidence for
-- that is GitHub's own scale-set message: JobStarted binds a pool member to the
-- job it consumed and names that job's owner, repository, workflow ref and
-- event, and JobCompleted names them again. The binding row kept only the job's
-- ids, so the identity was gone by the time anything needed it.
--
-- pool_runners gains the identity JobStarted carried, written with the binding
-- it belongs to. pending_completions gains the identity the COMPLETION carried,
-- because a completion restored after a restart is compared against the binding
-- and a comparison with a copy of the binding proves nothing: before this
-- column, a restored completion had no job id at all.
--
-- THE EMPTY STRING MEANS NOT RECORDED, and a reader treats it as could not tell,
-- never as a match. Rows written before this migration carry it, so a binding or
-- a completion from an older release can never authorise a publication.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN job_owner TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN job_repository TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN job_workflow_ref TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN job_event TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_id TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_owner TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_repository TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_workflow_ref TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_event TEXT NOT NULL DEFAULT ''
-- +billet:end
