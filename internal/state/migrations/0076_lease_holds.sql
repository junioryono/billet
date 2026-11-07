-- migration 76: lease_holds
--
-- A FAILED JOB'S VM KEPT FOR SOMEONE TO LOOK AT (#533). A row says the destroy GitHub's
-- completion already asked for is being delayed, until when, and how that ended.
--
-- A HOLD DELAYS A TEARDOWN AND NEVER AUTHORISES ONE. The completion is what authorises it,
-- exactly as for a job nobody holds, and the destroy runs the unchanged path and its
-- proofs. A hold's lease stays charged, in its ordinary phase, renewed by the party that
-- renews it, so nothing here touches capacity.
--
-- until_at IS WRITTEN ONCE, on the row's first write, and DO NOTHING ON CONFLICT is what
-- keeps it there: a retry, a restart or a second completion cannot extend a hold past its
-- bound. ended_at and ended_by are empty while it holds, and say what ended it: the bound,
-- the last session closing, an operator, or a drain that chose to cut it. Times are the
-- control plane's clock, in the ledger's fixed-width format.
--
-- The partial index holds only what is holding, which is all `billet status` and a drain
-- read, so the history of ended holds costs them nothing.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE lease_holds (
    lease_id TEXT PRIMARY KEY NOT NULL,
    tier TEXT NOT NULL,
    held_at TEXT NOT NULL,
    until_at TEXT NOT NULL,
    ended_at TEXT NOT NULL DEFAULT '',
    ended_by TEXT NOT NULL DEFAULT ''
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX lease_holds_holding ON lease_holds (until_at) WHERE ended_at = ''
-- +billet:end
