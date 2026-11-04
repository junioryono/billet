-- migration 71: usage_export
--
-- WHAT LETS A BILLING SYSTEM READ EACH FINISHED LEASE EXACTLY ONCE (#559).
--
-- usage_seq is the lease's place in the order leases were numbered in, written
-- once, in the transaction that closes the lease. finished_at cannot be a cursor:
-- it is the controller's wall clock, which steps, and which a failover replaces
-- with another machine's, so a lease can close with an earlier time than one an
-- exporter has already read. Zero means the lease has not finished, or finished
-- before this migration and is still waiting for its number.
--
-- usage_sequence is the last number handed out, a row and not a sequence or a
-- MAX, BECAUSE THE ROW'S LOCK IS WHAT MAKES THE ORDER SAFE TO READ, whatever else
-- serializes the writers. The number is taken by updating this row in the closing
-- transaction, so a second closer waits for the first to commit: a reader paging
-- past N can never see N+1 commit before N. A PostgreSQL sequence hands out
-- numbers outside the transaction, so this order would rest entirely on the
-- writers' advisory lock, and a reader would page past the one still uncommitted
-- the day anything took a number without it. A rollback gives the number back with
-- everything else. THE ROW EXISTS ONCE A LEASE HAS FINISHED:
-- a ledger with no history has none, because a row in any table is what tells a
-- used ledger from a pristine one (PeekLedger), and a restore decides by that.
--
-- usage_mark is random, drawn in the transaction that writes the number and written
-- once with it, BECAUSE A LEASE AND A NUMBER DO NOT NAME A PLACE IN ONE HISTORY. A
-- ledger restored from a backup numbers again from where the backup left off: a lease
-- that was waiting can take the same number it took before the restore, with the same
-- finished_at, and a lease the exporter never read can take a lower one. A cursor
-- carries the mark, so one from the other history is refused and never read past.
--
-- github_finished_at is GitHub's own time for a job's end, from the completion message,
-- which with github_runner_assigned_at gives a job's length on one clock. Billet's
-- started_at and result_at are when the messages ARRIVED, and a real JobStarted arrived
-- 46 and 47 seconds after its job started (2026-11-04), once in one delivery with the
-- job's completion: a job GitHub ran for 243 seconds read as 197. Those two pairs are
-- not interchangeable, so a usage record says which it read.
--
-- pending_completions keeps those two times beside the rest of what a completion said,
-- because a controller that dies between recording the delivery and recording its result
-- writes that result on its next start, and result_at is then the restart: a two-minute
-- job would read as however long the controller was down.
--
-- github_target is the GitHub path of the owner whose runner the lease held,
-- copied from the lease as it closes. Migration 64 put it on the lease only, and
-- the row that outlives the lease had nothing but a tier label, which an operator
-- may rename or give to another target (#93). Empty means no registration was
-- minted for the lease, or the row was written before this migration.
--
-- NOTHING IS NUMBERED HERE, AND THAT IS THE POINT. Rewriting every finished row in a
-- migration took 69 seconds for a million rows on PostgreSQL and 2m20s for five
-- million on SQLite (a loaded test host, 2026-11-04), against a startup budget of 30
-- seconds: a ledger past that fails the same migration on every restart and never
-- starts. The control plane numbers what had already finished in small batches
-- instead, which job_history_usage_unnumbered finds, and a lease that closes after
-- this migration is numbered as it closes. Numbers are a place in the order of being
-- numbered, not of finishing, so a late batch breaks nothing a reader relies on.
--
-- WHAT IS LEFT HERE STILL READS THE HISTORY, ONCE: the two index builds, and on SQLite
-- the new CHECK. That cost 6.8 seconds for a million rows on SQLite and 3.9 on
-- PostgreSQL on the same host, so the budget is reached at about four million rows.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_target TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN usage_seq INTEGER NOT NULL DEFAULT 0 CHECK (usage_seq >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN usage_mark TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_finished_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_runner_assigned_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pending_completions ADD COLUMN job_finished_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE TABLE usage_sequence (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    last INTEGER NOT NULL CHECK (last >= 0)
) STRICT
-- +billet:end

-- +billet:statement
CREATE UNIQUE INDEX job_history_usage_seq ON job_history (usage_seq) WHERE usage_seq > 0
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_usage_unnumbered ON job_history (finished_at, lease_id)
 WHERE usage_seq = 0 AND finished_at IS NOT NULL AND finished_at != ''
-- +billet:end
