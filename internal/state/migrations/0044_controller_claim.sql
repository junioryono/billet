-- migration 44: controller_claim
--
-- WHO IS THE CONTROLLER, AND WHICH GENERATION OF IT.
--
-- The exclusive lock on the state directory excludes a second control plane on
-- THIS HOST, which is the whole of the problem while the ledger is a file and
-- half of it once the ledger is a database two machines can reach. This row is
-- the durable half: it names the holder, so a refusal can say who has it, and it
-- carries an epoch that only ever goes up.
--
-- THE EPOCH IS NOT A FENCE YET, and saying so is the point. Nothing reads it,
-- and nothing is bound to it; the controller election is what makes a lost
-- leadership unable to write. It exists now because adding a column later is a
-- migration and adding it here is free, and because a claim that recorded no
-- generation would have to be replaced rather than extended.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE controller_claim (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	holder     TEXT NOT NULL,
	epoch      INTEGER NOT NULL CHECK (epoch > 0),
	claimed_at TEXT NOT NULL
) STRICT
-- +billet:end
