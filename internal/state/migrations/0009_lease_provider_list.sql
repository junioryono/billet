-- migration 9: lease_provider_list
--
-- providerListMigration splits "which backends may this lease run on" from
-- "which one is it on".
--
-- The single `provider` column answered both, which quietly made a tier's
-- backend a property of its reservation: a lease was pinned before anything knew
-- where it would run, so one `runs-on` label could never span a machine at home
-- and a cloud. `providers` is the ordered list the tier accepted, copied at
-- reserve; `chosen_provider` is filled in at bind.
--
-- Existing rows carry their single provider into BOTH columns. That is the
-- honest reading of an old row: it was reserved for exactly one backend, and if
-- it is already bound, that is the backend it is on. A bound row would be
-- indistinguishable from an unbound one otherwise, and the placement check fails
-- closed on a lease it cannot verify.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN providers TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN chosen_provider TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
UPDATE leases SET providers = provider WHERE providers = ''
-- +billet:end

-- Only rows that are actually placed get a chosen backend. An unbound row
-- has not chosen anything, and writing one would make the column mean
-- "was reserved for" again — the exact conflation being undone.
-- +billet:statement
UPDATE leases SET chosen_provider = provider WHERE node IS NOT NULL AND chosen_provider = ''
-- +billet:end
