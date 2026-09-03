-- migration 30: lease_deregistered
--
-- leaseDeregisteredMigration records whether a lease's GitHub runner
-- registration has been removed. The runner-capacity count keys on it rather
-- than on phase, because phase cannot tell a completed-job teardown (RemoveRunner
-- done, safe to stop counting) from an ambiguous-launch custody teardown or a
-- reaped-but-still-registered runner (RemoveRunner not done, must keep counting
-- or a replacement double-schedules the live runner). Defaults 0; set once,
-- monotonically, when RemoveRunner succeeds.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN deregistered INTEGER NOT NULL DEFAULT 0 CHECK (deregistered IN (0,1))
-- +billet:end
