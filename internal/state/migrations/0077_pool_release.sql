-- migration 77: pool_release
--
-- A POOL HOST IS RELEASED ONLY ON PROOF (ADR-030, #553), and the proof waits for the
-- host to drain: a person asks for the release, the host is taken out of placement at
-- once, and the order is sealed, its node's certificates revoked and the source asked
-- to take the host back only once the ledger shows it holds nothing.
--
-- release_requested_at is when the release was asked for, empty until it is; it is
-- the moment placement stopped, so an inventory the node reports after it is one
-- nothing could have been placed behind. It never moves back: a release once asked
-- for is not withdrawn, and a node's registration keeps the host out of placement
-- while it stands.
--
-- release_reason is what the person who asked gave, for `billet pool` and the log.
--
-- The states this uses (sealed, released, gone) were listed by migration 75.

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN release_requested_at TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN release_reason TEXT NOT NULL DEFAULT ''
-- +billet:end
