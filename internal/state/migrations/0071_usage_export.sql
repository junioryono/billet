-- migration 71: usage_export
--
-- WHAT LETS A BILLING SYSTEM READ EACH FINISHED LEASE EXACTLY ONCE (#559).
--
-- usage_seq is the lease's place in the order leases finished in, written once,
-- in the transaction that closes the lease. finished_at cannot be a cursor: it is
-- the controller's wall clock, which steps, and which a failover replaces with
-- another machine's, so a lease can close with an earlier time than one an
-- exporter has already read. Zero means the lease has not finished.
--
-- usage_sequence is the last number handed out, a row and not a sequence or a
-- MAX, BECAUSE THE ROW'S LOCK IS WHAT MAKES THE ORDER SAFE TO READ. The number is
-- taken by updating this row in the closing transaction, so a second closer on
-- PostgreSQL (an operator's force-destroy beside the controller) waits for the
-- first to commit: a reader paging past N can never see N+1 commit before N. A
-- PostgreSQL sequence hands out numbers outside the transaction, and a reader
-- would page past the one still uncommitted and never see it. A rollback gives
-- the number back with everything else.
--
-- github_target is the GitHub path of the owner whose runner the lease held,
-- copied from the lease as it closes. Migration 64 put it on the lease only, and
-- the row that outlives the lease had nothing but a tier label, which an operator
-- may rename or give to another target (#93). Empty means no registration was
-- minted for the lease, or the row was written before this migration.
--
-- THE BACKFILL NUMBERS WHAT HAS ALREADY FINISHED, oldest first, so a deployment's
-- history is exportable and not only what ends after this upgrade. Order by the
-- clock is the best a past row offers, and it is written once: nothing has read
-- these numbers yet, so nothing can have read past one.
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
CREATE TABLE usage_sequence (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    last INTEGER NOT NULL CHECK (last >= 0)
) STRICT
-- +billet:end

-- +billet:statement
UPDATE job_history
   SET usage_seq = numbered.n
  FROM (SELECT lease_id, ROW_NUMBER() OVER (ORDER BY finished_at, lease_id) AS n
          FROM job_history
         WHERE finished_at IS NOT NULL AND finished_at != '') AS numbered
 WHERE numbered.lease_id = job_history.lease_id
-- +billet:end

-- +billet:statement
INSERT INTO usage_sequence (id, last)
VALUES (1, (SELECT COALESCE(MAX(usage_seq), 0) FROM job_history))
-- +billet:end

-- +billet:statement
CREATE UNIQUE INDEX job_history_usage_seq ON job_history (usage_seq) WHERE usage_seq > 0
-- +billet:end
