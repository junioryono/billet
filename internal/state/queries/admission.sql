-- name: ReadAdmission :one
-- The deployment's admission state.
--
-- The row is inserted by the migration that creates the table, so sql.ErrNoRows
-- means something is wrong with the ledger rather than that admission is open.
-- ReadAdmission in admission.go is where that distinction is made.
SELECT mode, generation, provenance, reason, actor, changed_at
  FROM admission WHERE id = 1;

-- name: SetAdmission :exec
-- One admission transition, refusing if the generation moved.
--
-- THE GENERATION IS IN THE WHERE as well as the SET: the caller has already
-- compared it inside this transaction, and repeating it here means a write can
-- never land on a row it did not read.
UPDATE admission
   SET mode = @mode, generation = @generation, provenance = @provenance,
       reason = @reason, actor = @actor, changed_at = @changed_at
 WHERE id = 1 AND generation = @expect;

-- name: ListAdmissionRows :many
-- Every row of the admission table, for the pristine check a restore depends on.
--
-- ALL OF THEM, not one: proving a ledger is untouched means proving there is
-- exactly the single row its migration inserted, and a query that selected
-- `WHERE id = 1` could not see a second one.
SELECT id, mode, generation, provenance, reason, actor, changed_at FROM admission;
