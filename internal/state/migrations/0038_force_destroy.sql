-- migration 38: force_destroy
--
-- forceDestroyMigration records an operator's explicit decision to destroy
-- compute that is still running a job.
--
-- IT IS THE ONLY THING IN BILLET THAT MAY FAIL A BUILD ON PURPOSE, and its shape
-- is what stops it becoming an implicit one. Every other teardown either ends
-- work GitHub has already concluded or leaves running work alone; this ends a job
-- GitHub will not requeue, so the record has to say who decided, why, and against
-- exactly which leases — before anything is destroyed.
--
-- THE TARGET SET IS FIXED WHEN THE REQUEST IS TAKEN, and that is the safety
-- property rather than an implementation convenience. A standing "destroy
-- running compute" flag would destroy every job launched after it, which is the
-- shape of a timer authorising a teardown — the drain deadline that used to
-- destroy running jobs. Enumerating the leases up front bounds the request,
-- makes it idempotent across a control-plane restart (those rows are terminal by
-- the time it is re-observed), and produces the diagnostic an operator has to
-- read before confirming.
--
-- AT MOST ONE REQUEST IS OPEN, enforced by a partial unique index rather than by a
-- check in Go: two concurrent forces would each enumerate a set the other was
-- midway through destroying, and neither diagnostic would describe what happened.
--
-- admission_generation is the seal this was authorised against. A force is
-- refused unless an operator has already sealed the deployment, because
-- enumerating a target set while work is still being admitted races new jobs into
-- the gap between the diagnostic and the confirmation. Recording which seal makes
-- it visible afterwards that the seal did not move underneath the request.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE force_destroy (
			generation           INTEGER PRIMARY KEY CHECK (generation > 0),
			admission_generation INTEGER NOT NULL CHECK (admission_generation >= 0),
			state                TEXT NOT NULL CHECK (state IN ('requested','completed')),
			reason               TEXT NOT NULL,
			actor                TEXT NOT NULL,
			requested_at         TEXT NOT NULL,
			completed_at         TEXT NOT NULL
		) STRICT
-- +billet:end

-- +billet:statement
CREATE UNIQUE INDEX force_destroy_open
		     ON force_destroy (state) WHERE state = 'requested'
-- +billet:end

-- +billet:statement
CREATE TABLE force_destroy_target (
			generation        INTEGER NOT NULL,
			lease_id          TEXT NOT NULL CHECK (length(trim(lease_id)) > 0),
			tier              TEXT NOT NULL,
			node              TEXT NOT NULL,
			run_id            TEXT NOT NULL,
			scheduler_request INTEGER NOT NULL,
			phase             TEXT NOT NULL,
			state             TEXT NOT NULL CHECK (state IN ('pending','destroyed','failed')),
			detail            TEXT NOT NULL,
			PRIMARY KEY (generation, lease_id)
		) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX force_destroy_target_pending
		     ON force_destroy_target (state, tier)
-- +billet:end
