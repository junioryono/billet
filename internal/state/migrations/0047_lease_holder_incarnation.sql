-- migration 47: lease_holder_incarnation
--
-- WHICH PROCESS HOLDS THE COMPUTE, named by the node's INCARNATION rather than by
-- its registration epoch — and the difference is the whole reason there are two
-- columns here rather than one.
--
-- A node process mints one incarnation for its whole life and presents it on
-- every request; the plane keeps it in memory and nothing durable did. The
-- registration epoch is a different thing: it moves on EVERY registration, and
-- the same process registers again whenever a control plane restarts or forgets
-- it. So "the epoch moved" is what an ordinary restart looks like, and a report
-- built on it would call every surviving lease's holder replaced after each one.
-- The incarnation is what actually changes when the process does.
--
-- nodes.incarnation is what the host presented at its current registration.
-- leases.holder_incarnation is the node's incarnation when a process took the
-- compute — at Bind, when it launched it, and again when a process moved the
-- lease into custody or teardown, when it holds it. Empty means "recorded by a
-- binary that did not write it" or a host that never presented one (the
-- loopback wire), and a report renders that as unknown rather than as anybody.
--
-- THE CASE THIS EXISTS FOR: a completion bound to a process that died kept its
-- capacity charged, `billet leases` showed nothing held, and nothing an
-- operator could read said the holder had been replaced. With these two columns
-- a report can compare them and say so. It authorises nothing: what frees such
-- a lease is still its expiry unrenewed and the host's inventory.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN incarnation TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN holder_incarnation TEXT NOT NULL DEFAULT ''
-- +billet:end
