-- The newest release that has served this ledger.
--
-- A SCHEMA VERSION IS NOT A RELEASE VERSION. A binary refuses a migration it has
-- never heard of, and that was the only downgrade guard the ledger had; it is
-- silent for every pair of releases that share a schema, which is most of them.
-- This row is what lets an open say "a newer billet has been serving these rows"
-- whether or not a migration happened in between.
--
-- WRITTEN BY THE CONTROL PLANE ONLY, and only forwards. An operator command from
-- a newer binary records nothing, because a `billet check` run from a laptop
-- would otherwise fence the running server out of its own next restart. The one
-- write that lowers it is the host upgrade's deliberate downgrade, through the
-- maintenance handle, after the ledger snapshot that keeps the higher mark for a
-- rollback.
--
-- (The prose in this file is ASCII, like every query file: sqlc rewrites named
-- parameters on BYTE offsets, so one multi-byte character shifts every statement
-- after it and the parse error names neither the file nor the character.)

-- name: ReadReleaseWatermark :one
-- Which release last raised the mark, and when.
--
-- sql.ErrNoRows IS AN ORDINARY STATE. A ledger migrated before this table
-- existed carries no mark, and so does one no control plane has served since;
-- the caller reads an absent row as "nothing is known" and never as a release.
SELECT release, recorded_at FROM release_watermark WHERE id = 1;

-- name: SetReleaseWatermark :exec
-- Record the release now serving this ledger.
--
-- AN UPSERT, BECAUSE THE MARK MOVES. Forward on every control-plane open that
-- finds itself newer than the record, and backward exactly once, by a person, on
-- a downgrade they asked for by name. Which direction is allowed is decided in Go
-- under the writer's own transaction, where the comparison and the write are one
-- decision; the statement itself takes whatever it is handed.
INSERT INTO release_watermark (id, release, recorded_at)
VALUES (1, @release, @recorded_at)
ON CONFLICT (id) DO UPDATE SET
   release     = excluded.release,
   recorded_at = excluded.recorded_at;
