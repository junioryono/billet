-- Which controller of this deployment is retiring, or has retired.
--
-- THE ROW IS A RESERVATION FIRST AND A RECORD AFTERWARDS. A retirement request
-- writes it before it collects any evidence about the other controller, inside
-- one write transaction, and the single writer decides which of two requests
-- holds it; from then on the row names the retiring host, the run that reserved
-- it and the transition id minted with it, and it advances `reserved` ->
-- `intent` -> `done` behind the retiring host's journal. A `done` row is never
-- deleted by the transition, because it is the durable word that this
-- deployment has no survivor for a second retirement.
--
-- EVERY MUTATION IS CONDITIONAL ON THE STATE IT LEAVES, and answers with rows
-- affected, so a caller that read a row and then wrote against it learns when
-- the row moved between the two rather than overwriting whatever is there now.
-- The state words reach the statements as parameters from the one vocabulary in
-- internal/state, never as literals a second copy could misspell.
--
-- (The prose in this file is ASCII, like every query file: sqlc rewrites named
-- parameters on BYTE offsets, so one multi-byte character shifts every statement
-- after it and the parse error names neither the file nor the character.)

-- name: ReadRetirement :one
-- The deployment's retirement row, if any.
--
-- sql.ErrNoRows IS POSITIVE ABSENCE AND NOTHING ELSE. A request that finds no
-- row may reserve; a completion that finds none may reconstruct from a
-- document; a `done` handling that finds none has met a ledger restored from
-- before the retirement and says so. A read that FAILS is none of those, and
-- the caller keeps the two apart.
SELECT deployment, retiring, survivor, run, state, transition_id,
       reserved_at, updated_at, completed_by, completed_at
  FROM controller_retirement
 WHERE deployment = @deployment;

-- name: InsertRetirement :exec
-- Write the deployment's row for the first time.
--
-- A PLAIN INSERT: the caller has read the row inside this same write
-- transaction and found none, and writers serialise (SQLite at BEGIN IMMEDIATE,
-- PostgreSQL on the advisory lock), so nothing reserves between the read and
-- this write. The primary key refuses if something somehow did, rather than
-- quietly replacing another host's reservation. The state is a parameter
-- because the same statement writes a `reserved` row for a request and a
-- `done` row for a completion that reconstructs a positively absent one.
INSERT INTO controller_retirement (
    deployment, retiring, survivor, run, state, transition_id,
    reserved_at, updated_at, completed_by, completed_at
) VALUES (
    @deployment, @retiring, @survivor, @run, @state, @transition_id,
    @reserved_at, @updated_at, @completed_by, @completed_at
);

-- name: AdoptRetirement :execresult
-- Bind a `reserved` row for this host to the run that is resuming it.
--
-- A request that crashed between its reservation and its journal's `intent`
-- left a row nobody is acting on; the SAME HOST's next request adopts it rather
-- than being refused by its own reservation. The filter is the whole rule: the
-- host must match and the state must still be `reserved`, so another host's
-- row and a row already at `intent` change no rows and the caller refuses.
UPDATE controller_retirement
   SET run = @run, updated_at = @updated_at
 WHERE deployment = @deployment AND retiring = @retiring AND state = @reserved;

-- name: AdvanceRetirementToIntent :execresult
-- Move this host's reservation to `intent`, after the journal's `intent`.
--
-- THE JOURNAL IS WRITTEN FIRST, so a crash between the two leaves a `reserved`
-- row beside a journal at `intent`, which the resume validates and advances
-- through this same statement. The transition id is in the filter because the
-- row's id was minted at the reservation and the journal copied it: a journal
-- carrying another id is not this reservation's.
UPDATE controller_retirement
   SET state = @intent, run = @run, updated_at = @updated_at
 WHERE deployment = @deployment AND retiring = @retiring
   AND transition_id = @transition_id AND state = @reserved;

-- name: CompleteRetirement :execresult
-- Move this host's row to `done`, on the retiring host's own word or the
-- survivor's completion of it.
--
-- FROM `intent` OR FROM `reserved`, never from anything else: the caller has
-- read the row and decided which branch of the completion it is in, and this
-- filter holds the write to that reading. The reservation time is compared as
-- well as the id, because a ledger restored from between the two `intent`
-- writes carries a `reserved` row whose id the document must still equal and
-- whose reservation binds it to this request.
UPDATE controller_retirement
   SET state = @done, completed_by = @completed_by, completed_at = @completed_at,
       updated_at = @updated_at
 WHERE deployment = @deployment AND retiring = @retiring AND survivor = @survivor
   AND transition_id = @transition_id AND reserved_at = @reserved_at
   AND (state = @reserved OR state = @intent);

-- name: DeleteReservedRetirement :execresult
-- Release a `reserved` row this run holds.
--
-- THE ONLY DELETE, AND IT IS SCOPED TO THE RUN: a request refused after its
-- own insert releases what it inserted, an operator's abandonment releases the
-- row the named run holds, and neither can touch an adopted row under another
-- run, another host's row, or a row that has reached `intent`.
DELETE FROM controller_retirement
 WHERE deployment = @deployment AND retiring = @retiring
   AND run = @run AND state = @reserved;
