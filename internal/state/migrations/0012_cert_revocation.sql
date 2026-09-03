-- migration 12: cert_revocation
--
-- A CERTIFICATE CAN BE TAKEN BACK, which until now it could not.
--
-- The wire's whole admission decision is "an operator issued this host a
-- certificate", and it lasts a year. A decommissioned machine, or one whose key
-- leaked, could rejoin and be handed work — including a JIT credential that
-- registers a runner against the organisation — and the only remedy was to
-- rotate the CA, which invalidates every node at once.
--
-- KEYED ON SERIAL, not on node name. A name can be re-issued to a replacement
-- machine deliberately, and revoking the name would refuse the replacement too.
-- The serial identifies the one credential being withdrawn.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE revoked_certs (
			serial     TEXT PRIMARY KEY,
			node       TEXT NOT NULL,
			reason     TEXT NOT NULL DEFAULT '',
			revoked_at TEXT NOT NULL
		)
-- +billet:end

-- +billet:statement
CREATE INDEX idx_revoked_certs_node ON revoked_certs (node)
-- +billet:end
