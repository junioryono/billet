-- migration 6: lease_guest_os
--
-- guestOSMigration records what a lease actually boots, for the same reason
-- placement is recorded: a host may be restricted to a subset of guest
-- operating systems, and the check happens at bind time — long after the tier
-- catalog that produced the lease may have changed underneath it.
--
-- 'linux' is the column default because it is the overwhelming majority case
-- and a real guest OS: an empty default would match no allowlist and strand
-- every lease written before the column existed.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN guest_os TEXT NOT NULL DEFAULT 'linux'
-- +billet:end
