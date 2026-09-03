-- migration 18: node_revocations
--
-- A NODE CAN BE REVOKED WITHOUT ENUMERATING WHAT IT HOLDS.
--
-- Revocation by serial reaches only credentials billet wrote down, and there are
-- two ways for one to exist outside that set: a deployment upgraded from a
-- version that did not record serials, and a name that was issued more than once
-- before it did — the admission trail keeps one row per node and overwrites it,
-- so the earlier certificate is unrecoverable.
--
-- A CUTOFF NEEDS NO LIST. Revoking a node records the moment; any certificate
-- for that name minted before it is refused on sight, whether or not billet has
-- ever seen it. A replacement issued afterwards has a later NotBefore and works,
-- which is what keeps this a revocation rather than a ban on the name.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE node_revocations (
			node           TEXT PRIMARY KEY,
			-- Every certificate for this node valid from before this instant is
			-- refused. Stored as the certificate's own NotBefore basis, so the
			-- comparison is against a fact of the credential rather than a clock.
			revoked_before TEXT NOT NULL,
			reason         TEXT NOT NULL DEFAULT '',
			revoked_at     TEXT NOT NULL
		) STRICT
-- +billet:end
