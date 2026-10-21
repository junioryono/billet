-- migration 64: tier_releases
--
-- HOW MANY LEASES OF EACH TIER HAVE CLOSED, as a generation every
-- terminalization advances in its own transaction (#140). A listener publishes
-- GitHub's assigned work less the runners covering it, and a drain that reads
-- a quiet ledger beside that count calls the deployment drained when it is
-- none. A lease released after the count was taken may have been one of those
-- runners, so the listener reads this generation in the same snapshot as its
-- runner count and publishes it beside the count, and the drain trusts the
-- count only while the generation has not moved. A time cannot do this: a
-- wall clock that steps back, or a successor controller whose clock runs
-- behind, dates a release before a count it invalidated.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE tier_releases (
    tier TEXT PRIMARY KEY NOT NULL,
    generation INTEGER NOT NULL
) STRICT
-- +billet:end
