-- Which process is this deployment's controller, and which generation of it.
--
-- THE ROW IS THE RECORD, NOT THE EXCLUSION. What actually stops a second
-- control plane is a lock: the exclusive hold on the state directory when the
-- ledger is a file, and a session-scoped advisory lock when it is a database two
-- machines can reach. This table is what lets a refusal SAY WHO HAS IT, and what
-- carries the epoch forward.
--
-- A row read from here is never permission to proceed, and that is the whole
-- discipline of the table: it is written after the exclusion is taken, and it is
-- read to REFUSE rather than to allow. A claim decided from a row would be
-- deciding from what is present rather than from what is proved, which is the
-- mistake `ca retire` took three rounds to stop making.

-- name: ReadControllerClaim :one
-- Who holds it, and at which generation.
--
-- TWO READERS, AND THE FENCE IS THE ONE ON THE HOT PATH. DB.Tx re-reads this row
-- inside every write transaction and refuses when the epoch has moved, which is
-- what makes a controller that lost its exclusion unable to write rather than
-- merely able to notice; the other reader is a refusal explaining who holds the
-- claim it could not take. One statement serves both because the fence's own
-- diagnostic wants the holder anyway, and reading it from the same consistent
-- row costs nothing.
--
-- sql.ErrNoRows MEANS DIFFERENT THINGS TO THE TWO OF THEM, and neither collapses
-- it into the other. To the diagnostic it is a fresh deployment nothing has ever
-- claimed, which is ordinary. To the fence it is a process that DID claim and
-- can no longer find the record, which is a refusal.
SELECT holder, epoch, claimed_at FROM controller_claim WHERE id = 1;

-- name: ClaimController :one
-- Record this process as the controller and advance the generation.
--
-- THE EPOCH ONLY EVER GOES UP, and it is computed from the row rather than
-- supplied: a caller that passed one could pass the same one twice, and two
-- controllers agreeing on a number is exactly what a fencing token must never
-- allow. The first claim is 1, because the column refuses zero and a zero epoch
-- would be indistinguishable from a row nothing has written.
INSERT INTO controller_claim (id, holder, epoch, claimed_at)
VALUES (1, @holder, 1, @claimed_at)
ON CONFLICT (id) DO UPDATE SET
    holder     = excluded.holder,
    epoch      = controller_claim.epoch + 1,
    claimed_at = excluded.claimed_at
RETURNING epoch;
