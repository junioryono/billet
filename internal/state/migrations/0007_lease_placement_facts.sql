-- migration 7: lease_placement_facts
--
-- placementFactsMigration corrects migration 6's backfill and records the
-- remaining placement fact.
--
-- Migration 6 defaulted EVERY pre-existing lease to 'linux', including macOS
-- ones. That is not a safe default in the direction its own comment claimed:
-- an unbound macOS lease relabelled Linux would be PERMITTED onto a Linux-only
-- host, even though its durable macos_slot proves what it is.
--
-- The backfill reads macos_slot, which is authoritative only for leases written
-- after migration 5 — that migration added the column defaulting to 0 and did
-- not backfill it either, so a macOS lease predating it is indistinguishable
-- from a Linux one and this UPDATE cannot repair it. Nothing here can: the
-- information was never recorded. What protects those rows is the allocator
-- refusing to place any lease with no recorded provider, which every pre-v7
-- lease is; they fail closed rather than being guessed at.
--
-- Corrected by appending rather than by editing migration 6: the checksum guard
-- exists precisely to stop an applied migration changing underneath a database,
-- and "nobody has run it yet" is the argument that erodes that discipline.
--
-- provider joins target_node, macos_slot and guest_os as a placement fact
-- recorded on the row. A Firecracker lease cannot run on a Tart host, so Bind
-- has to be able to compare — and re-deriving it from the live catalog is what
-- lets a tier redefined mid-flight reclassify a lease that is already running.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
UPDATE leases SET guest_os = 'macos' WHERE macos_slot = 1
-- +billet:end

-- +billet:statement
ALTER TABLE leases ADD COLUMN provider TEXT NOT NULL DEFAULT ''
-- +billet:end
