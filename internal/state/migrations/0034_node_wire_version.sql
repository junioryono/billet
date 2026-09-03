-- migration 34: node_wire_version
--
-- A NODE'S BUILD AND THE WIRE IT NEGOTIATED, recorded so an operator can see
-- which hosts are still holding an old protocol open — and so a later release
-- can tell when it is safe to stop supporting one. The plane's own map cannot
-- answer that: `billet status` runs as a separate process through OpenAdmin and
-- has no view of a running control plane's memory.
--
-- `node_release` rather than `release`, because RELEASE is a SQLite keyword
-- (RELEASE SAVEPOINT) and an unquoted column of that name is a trap for every
-- hand-written query afterwards.
--
-- ZERO AND EMPTY MEAN NOT RECORDED, which is exactly what a row written by an
-- older binary leaves behind, and what every reader renders as unknown. Nothing
-- authorises anything from these columns, so there is no invariant for that zero
-- to weaken.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN node_release TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN wire_min INTEGER NOT NULL DEFAULT 0 CHECK (wire_min >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN wire_max INTEGER NOT NULL DEFAULT 0 CHECK (wire_max >= 0)
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN wire_version INTEGER NOT NULL DEFAULT 0
			CHECK (wire_version >= 0)
-- +billet:end
