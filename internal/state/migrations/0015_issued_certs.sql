-- migration 15: issued_certs
--
-- A CREDENTIAL CAN ONLY BE TAKEN BACK IF BILLET KNOWS IT EXISTS.
--
-- Revocation is by serial, and a serial names one certificate. That is the right
-- granularity — a node name is legitimately re-issued to a replacement machine,
-- so revoking the name would refuse the replacement too — but it only works if
-- every serial in circulation is written down. Renewal minted a fresh key and
-- serial, returned it, and recorded nothing, so a node that had renewed once
-- held a credential the control plane could not name. An operator revoking the
-- bundle they issued took back a serial nobody was presenting, was told it would
-- be refused on its next request, and the host carried on registering, binding
-- leases and asking for JIT runner registrations.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE issued_certs (
			serial     TEXT PRIMARY KEY,
			node       TEXT NOT NULL,
			-- enrolled | issued | renewed: how this credential came to exist,
			-- which is what an operator reads when deciding what to take back.
			source     TEXT NOT NULL,
			not_after  TEXT NOT NULL,
			issued_at  TEXT NOT NULL
		) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX idx_issued_certs_node ON issued_certs (node)
-- +billet:end
