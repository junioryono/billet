-- migration 48: release_watermark
--
-- THE NEWEST RELEASE THAT HAS SERVED THIS LEDGER, so an older one can be refused.
--
-- A binary refuses a schema it has never heard of, and that is the only thing
-- that stood between an installed deployment and a downgrade. It is sound only
-- when the two releases differ in a migration; most do not, so a host handed an
-- older billet — a stale pin converged by hand, an archive restored beside the
-- wrong binary, an installer run against a machine that had already moved on —
-- opened the ledger a newer release had been writing and served it. Nothing
-- said so, because nothing anywhere recorded which release had been here.
--
-- A SINGLETON, like the deployment binding one migration back, because it is one
-- deployment-wide fact. It is written by the CONTROL PLANE and only when the
-- running release is provably newer than what is recorded: an operator command
-- run from a newer binary records nothing, or a `billet check` from a laptop
-- would fence the server out of its own next restart. It never moves backwards
-- on its own; a deliberate downgrade lowers it through the host upgrade's
-- maintenance handle, and the ledger snapshot taken before that step keeps the
-- higher mark for a rollback.
--
-- nodes.highest_release is the same idea per host and it AUTHORISES NOTHING. A
-- node that comes back on an older release is the ordinary shape of a rollback
-- the rollout coordinator infers from exactly that registration, so a refusal
-- there would break the mechanism that makes a failed upgrade recoverable. The
-- column lets `billet status` say a host is running something older than it
-- once did, and leaves the reading to a person.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE release_watermark (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	release     TEXT NOT NULL CHECK (release <> ''),
	recorded_at TEXT NOT NULL
) STRICT
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN highest_release TEXT NOT NULL DEFAULT ''
-- +billet:end
