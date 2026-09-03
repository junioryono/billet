-- migration 13: node_enrollment
--
-- A NODE ASKS TO JOIN AND AN OPERATOR SAYS YES, which is a decision that needs
-- somewhere to sit between the two.
--
-- Admission used to be entirely out of band: an operator ran `billet ca issue`
-- and copied a bundle to the machine. That works and it is not discoverable — a
-- node that is powered on and pointed at a control plane appears nowhere, so the
-- operator has to already know it exists, and the thing they compare to decide
-- it is the right machine is a file they copied rather than anything the node
-- proved.
--
-- KEYED ON NAME, WITH THE FINGERPRINT AS THE FACT BEING APPROVED. The name is
-- how an operator refers to a host; the fingerprint is what makes approving it
-- mean something, because it is the one value both ends can display and a human
-- can compare out of band.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE node_enrollments (
			name         TEXT PRIMARY KEY,
			fingerprint  TEXT NOT NULL,
			csr_pem      TEXT NOT NULL,
			cert_pem     TEXT NOT NULL DEFAULT '',
			state        TEXT NOT NULL CHECK (state IN ('pending','approved','denied')),
			requested_at TEXT NOT NULL,
			decided_at   TEXT NOT NULL DEFAULT ''
		)
-- +billet:end
