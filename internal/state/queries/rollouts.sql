-- Rollout queries: one durable fleet decision, and where every component of it
-- has got to.
--
-- THE STATEMENTS HERE ARE internal/rollout's, NOT internal/state's, and that is
-- deliberate: the ledger's SQL lives in one directory so the guards that police
-- it -- the prepare-against-the-migrated-schema check, the read/write
-- classification, the ASCII rule and the wildcard ban -- cover every domain
-- rather than only the ones that happened to be converted first.
-- internal/rollout binds them through state.ReadQueries / state.WriteQueries.

-- name: HighestRolloutGeneration :one
-- The highest generation any rollout has ever carried.
--
-- CAST(COALESCE(...)) because sqlc types a bare MAX() -- and a bare
-- COALESCE(MAX(), 0) -- as interface{}. Zero on an empty table, so the caller's
-- +1 makes the first rollout generation 1, which the table's CHECK requires.
SELECT CAST(COALESCE(MAX(generation), 0) AS BIGINT) FROM rollouts;

-- name: InsertRollout :exec
-- Record one fleet decision.
--
-- finished_at and terminal_reason are written empty rather than parameterised:
-- a rollout being inserted has not finished, and there is no value a caller
-- could legitimately pass.
INSERT INTO rollouts
     (id, generation, channel, target_version, target_digest, policy,
      controller_phase, prior_version, state, created_by, created_at,
      finished_at, terminal_reason)
VALUES (@id, @generation, @channel, @target_version, @target_digest, @policy,
        @controller_phase, @prior_version, @state, @created_by, @created_at,
        '', '');

-- name: InsertRolloutNode :exec
-- Enrol one host in a rollout, at the phase every host starts in.
--
-- The set of hosts is a SNAPSHOT taken when the rollout starts, so this is only
-- ever called from Start: a host that registers later is running whatever it was
-- installed with and is not part of a decision taken before it existed.
INSERT INTO rollout_nodes
     (rollout_id, node, phase, attempts, next_attempt_at, blocker,
      prior_release, rollback_result, exempt_reason, updated_at,
      dispatch_epoch, converged_digest)
VALUES (@rollout_id, @node, @phase, 0, '', '', '', '', '', @updated_at, 0, '');

-- name: ReadRolloutInState :one
-- The rollout in one state.
--
-- A :one AND ONLY SAFE FOR 'open', which is the single state carrying a partial
-- unique index (rollouts_open). Its one caller passes StateOpen; a caller that
-- passed 'completed' would get an arbitrary one of many rows, which is why the
-- state stays a parameter naming the Go constant rather than a literal here that
-- would read as a guarantee this statement cannot make.
SELECT id, generation, channel, target_version, target_digest, policy,
       controller_phase, prior_version, state, created_by, created_at,
       finished_at, terminal_reason
  FROM rollouts WHERE state = @state;

-- name: ListRolloutHistory :many
-- Rollouts newest first, for the operator's report.
SELECT id, generation, channel, target_version, target_digest, policy,
       controller_phase, prior_version, state, created_by, created_at,
       finished_at, terminal_reason
  FROM rollouts ORDER BY generation DESC LIMIT CAST(@max_rows AS BIGINT);

-- name: ReadNewestRolloutForTarget :one
-- The newest rollout, in any state, to one manifest digest: what an automatic
-- start consults before it would restart bytes an operator abandoned.
SELECT id, generation, channel, target_version, target_digest, policy,
       controller_phase, prior_version, state, created_by, created_at,
       finished_at, terminal_reason
  FROM rollouts WHERE target_digest = @target_digest ORDER BY generation DESC LIMIT 1;

-- name: ReadRolloutControllerPhase :one
-- Where the control plane itself has got to in one rollout.
SELECT controller_phase FROM rollouts WHERE id = @id;

-- name: AdvanceRolloutController :exec
-- Move the control plane's own phase, refusing if it moved underneath us.
--
-- THE PHASE IS IN THE WHERE as well as the SET. The caller read it inside this
-- transaction and validated the transition; repeating it here means the write
-- can never land on a row it did not read.
UPDATE rollouts SET controller_phase = @controller_phase
 WHERE id = @id AND controller_phase = @expect_phase;

-- name: FinishRollout :exec
-- Close a rollout, refusing one that is no longer open.
UPDATE rollouts
   SET state = @state, finished_at = @finished_at, terminal_reason = @terminal_reason
 WHERE id = @id AND state = @expect_state;

-- name: ReadRolloutNodeProgress :one
-- One host's phase and attempt count, read against the row a write will act on.
SELECT phase, attempts FROM rollout_nodes
 WHERE rollout_id = @rollout_id AND node = @node;

-- name: AdvanceRolloutNode :exec
-- Move one host through the rollout state machine.
--
-- THE THREE CASE COLUMNS ARE WRITE-ONCE-OR-KEEP, not optional parameters: a
-- prior release, a dispatch epoch and a converged digest are each recorded by
-- whichever pass first learns them, and every later pass passes nothing and must
-- not erase what is there. Expressed in SQL rather than by building the
-- statement two ways, because a branch applied in one path and forgotten in
-- another is how a rollback loses the release it was meant to return to.
UPDATE rollout_nodes
   SET phase = @phase, attempts = @attempts, next_attempt_at = @next_attempt_at,
       blocker = @blocker, rollback_result = @rollback_result,
       exempt_reason = @exempt_reason,
       prior_release = CASE WHEN CAST(@prior_release AS TEXT) = ''
                            THEN prior_release ELSE CAST(@prior_release AS TEXT) END,
       dispatch_epoch = CASE WHEN CAST(@dispatch_epoch AS BIGINT) = 0
                            THEN dispatch_epoch ELSE CAST(@dispatch_epoch AS BIGINT) END,
       converged_digest = CASE WHEN CAST(@converged_digest AS TEXT) = ''
                              THEN converged_digest ELSE CAST(@converged_digest AS TEXT) END,
       updated_at = @updated_at
 WHERE rollout_id = @rollout_id AND node = @node AND phase = @expect_phase;

-- name: ListRolloutNodes :many
-- Where every host in one rollout has got to, in a stable order.
SELECT node, phase, attempts, next_attempt_at, blocker, prior_release,
       rollback_result, exempt_reason, updated_at, dispatch_epoch,
       converged_digest
  FROM rollout_nodes WHERE rollout_id = @rollout_id ORDER BY node;

-- name: ListRolloutNodePhases :many
-- Just the phase of every host in one rollout, for the completion check.
--
-- SEPARATE FROM ListRolloutNodes because the completion check reads every host
-- on every pass and needs two columns of the eleven; the projection is the
-- difference between a cheap question and the whole table.
SELECT node, phase FROM rollout_nodes
 WHERE rollout_id = @rollout_id ORDER BY node;
