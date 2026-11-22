-- migration 85: stranded_recoveries
--
-- WHAT BILLET DID TO RECOVER A JOB GITHUB STOPPED OFFERING (#911), for a tier that
-- asks with retry.stranded.
--
-- A recovery is two requests in order: cancel the job's run, then, once GitHub
-- records that attempt completed, ask it to re-run the run's failed jobs, which
-- runs the cancelled job again (measured 2026-11-22). The second is never sent
-- before the first is proved, and a row holds where between them a recovery is,
-- because a restart between them must neither cancel twice nor leave a run
-- cancelled and never re-run. state is `cancelling` (the cancel may have been sent),
-- `rerunning` (the run's attempt is proved completed and the re-run may have been
-- sent), or what it came to: `recovered`, `superseded` (the run moved past the
-- attempt by something other than billet), `failed` or `expired`. The vocabulary is
-- closed in Go rather than by a CHECK, for the reason migration 35 gives: SQLite
-- cannot extend a column CHECK in place.
--
-- The key is the rule: one recovery per attempt of a run. ended_at is when the
-- stranded launch's lease ended, from which every cancel asks the ledger again
-- whether the tier has since been assigned what may be the job. tries counts the
-- sends of the current request and bounds them. detail is billet's own text around GitHub's
-- decoded message: nothing a guest wrote.
--
-- stranded_tiers is when each tier started asking, as rerun_tiers is for re-runs: a
-- tier recovers only the launches that ended after it asked, so turning it on
-- never cancels a run that stranded before, and a tier that stops asking loses its
-- row.
--
-- The indexes serve the pass, which reads every recovery still between its
-- requests and every launch decided stranded that a recovery may start from, and
-- are partial because both are rare.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE stranded_recoveries (
    repo TEXT NOT NULL,
    run_id INTEGER NOT NULL CHECK (run_id > 0),
    run_attempt INTEGER NOT NULL CHECK (run_attempt > 0),
    tier TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    job_name TEXT NOT NULL,
    ended_at TEXT NOT NULL,
    state TEXT NOT NULL,
    tries INTEGER NOT NULL DEFAULT 0 CHECK (tries >= 0),
    created_at TEXT NOT NULL,
    settled_at TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo, run_id, run_attempt)
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX stranded_recoveries_open_idx ON stranded_recoveries(created_at)
    WHERE state IN ('cancelling', 'rerunning')
-- +billet:end

-- +billet:statement
CREATE TABLE stranded_tiers (
    tier TEXT PRIMARY KEY NOT NULL,
    asked_since TEXT NOT NULL
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_stranded_idx ON job_history(stranded_at)
    WHERE stranded = 'stranded'
-- +billet:end
