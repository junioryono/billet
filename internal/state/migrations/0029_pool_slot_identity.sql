-- migration 29: pool_slot_identity
--
-- Desired-count growth creates physical runners before GitHub names individual
-- jobs. Give those lease-backed slots their own durable scheduler namespace;
-- sharing job_identities would make a valid external job id a possible alias.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE pool_slot_identities (
			lease_id    TEXT PRIMARY KEY CHECK (length(trim(lease_id)) > 0),
			internal_id INTEGER NOT NULL UNIQUE CHECK (internal_id < 0)
		) STRICT
-- +billet:end
