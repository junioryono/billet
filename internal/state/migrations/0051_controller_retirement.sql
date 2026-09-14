-- migration 51: controller_retirement
--
-- WHICH CONTROLLER OF THIS DEPLOYMENT IS RETIRING, OR HAS RETIRED, kept in the
-- ledger both controllers share.
--
-- A retirement is one controller of a PostgreSQL active-passive pair stopping
-- for good while the other keeps serving. Two converges, one per host, each
-- reading the other's fresh report, could each conclude that the other is the
-- running survivor and retire both; the reports say nothing about the decision
-- the other run is making at the same moment. This row is what the two contend
-- for: a request RESERVES it before it collects any evidence, inside one write
-- transaction, and the ledger's single writer decides which request holds it.
--
-- ONE ROW PER DEPLOYMENT, BY PRIMARY KEY. A row at `reserved` names the host
-- that is about to retire and the run that reserved it; `intent` says the
-- transition has begun on that host; `done` says it has completed, and a `done`
-- row REFUSES EVERY LATER REQUEST for the life of the deployment, because a
-- deployment that has retired one controller has no survivor for a second, and
-- because a request judged on reports alone could be judged on stale ones.
-- Nothing deletes the row as part of the transition: a refusal before `intent`
-- releases the row the same run inserted, and an operator's explicit abandonment
-- releases a `reserved` row, and that is all.
--
-- THE TRANSITION ID IS MINTED AT THE RESERVATION and never changes: the
-- retiring host's journal copies it, the guard's record names it, and the
-- survivor's completion of the row is admitted only against it, so a row from
-- one retirement can never be completed on the word of another's document.
--
-- WHO COMPLETED THE ROW is recorded too, because the write may be the retiring
-- host's own or, when that host can no longer open the ledger (an older binary
-- against a migrated schema, a rotated credential), the survivor's on the
-- retiring host's behalf. Empty means not yet completed.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE controller_retirement (
	deployment    TEXT PRIMARY KEY,
	retiring      TEXT NOT NULL,
	survivor      TEXT NOT NULL,
	run           TEXT NOT NULL,
	state         TEXT NOT NULL CHECK (state IN ('reserved', 'intent', 'done')),
	transition_id TEXT NOT NULL,
	reserved_at   TEXT NOT NULL,
	updated_at    TEXT NOT NULL,
	completed_by  TEXT NOT NULL DEFAULT '',
	completed_at  TEXT NOT NULL DEFAULT ''
) STRICT
-- +billet:end
