-- The two lease listings whose phase set is a Go slice, and the one write that
-- settles a forced lease.
--
-- THE PHASE LISTS HERE ARE LITERALS, WHICH IS THE ONE PLACE THIS CONVERSION HAD
-- TO ACCEPT A SECOND SOURCE OF TRUTH. Everywhere else in these files a phase is
-- passed as a parameter from its Go constant, so no spelling can drift. These two
-- cannot: the sets are Go SLICES, whose length may change, and sqlc.slice() is
-- not available on the SQLite engine -- so a variable-arity `IN (?, ...)` has no
-- generated form.
--
-- TestTheForceDestroyStatementIsWhatTheGoSliceSays and
-- TestTheDrainStatementIsWhatTheGoSliceSays compare each WHOLE STATEMENT against
-- one assembled from its Go slice -- not the extracted list, and not the
-- extracted WHERE, because both of those ignore what surrounds them: `0 = 1 AND`
-- in front, or a `LIMIT 0` behind, empties the query while the compared fragment
-- is byte-identical. Which means a changed PROJECTION fails these too, and
-- should. A phase added to the Go slice alone is a lease an
-- operator approved destroying that the query never offers; one added to the SQL
-- alone is a lease destroyed without anybody having decided it may be.

-- name: ListForceDestroyCandidates :many
-- The running compute an operator could destroy, oldest first.
--
-- CUSTODY, TEARDOWN AND QUARANTINE ARE DELIBERATELY ABSENT from the phase list.
-- Their compute is held by a NODE rather than by a listener, and billet already
-- has the operation for them: `billet leases release --force` sets a request the
-- holder observes on its next heartbeat, so the ledger never changes underneath a
-- process that still believes it owns the proof obligation.
--
-- AN EMPTY FILTER MEANS UNFILTERED, expressed in SQL rather than by building the
-- query two ways: a narrowing applied in one branch and forgotten in another is
-- how a force reaches a tier the operator did not name.
--
-- run_id IS CAST BEFORE IT IS COALESCED, AND THAT ORDER IS THE WHOLE POINT.
-- run_id is a bigint, so coalescing it with an empty string asks PostgreSQL to
-- read that string as a bigint, and it refuses: invalid input syntax for type
-- bigint. SQLite coerces and says nothing, which is how the statement survived
-- being written that way. Casting first makes both arguments text on either
-- engine. The OUTER cast is a second, unrelated requirement: without it sqlc
-- types the whole expression interface{}, which takes whatever a caller passes.
--
-- The prose here avoids a quoted empty string on purpose: sqlc copies a query's
-- leading comment into the generated doc and rewrites the quotes as it goes, so
-- the explanation would arrive there saying something slightly different.
--
-- It generated cleanly and passed every check up to execution, which is what the
-- conformance run against a real PostgreSQL is for.
SELECT id, tier, COALESCE(node, target_node, '') AS node, phase,
       CAST(COALESCE(CAST(run_id AS TEXT), '') AS TEXT) AS run_id,
       COALESCE(request_id, 0) AS request_id,
       CAST(CASE WHEN held_at = '' THEN created_at ELSE held_at END AS TEXT) AS since
  FROM leases
 WHERE phase IN ('assigned','launching','online','busy')
   AND (CAST(@tier AS TEXT) = '' OR tier = CAST(@tier AS TEXT))
   AND (CAST(@node AS TEXT) = '' OR COALESCE(node, target_node, '') = CAST(@node AS TEXT))
 ORDER BY CASE WHEN held_at = '' THEN created_at ELSE held_at END, id;

-- name: TerminateForcedLease :exec
-- Settle a lease whose compute an operator destroyed.
--
-- FENCED, because the phase was read inside this transaction and a lease can
-- acquire a live holder between a backend being asked to stop a guest and its
-- capacity being returned.
UPDATE leases
   SET phase = @phase, epoch = epoch + 1, failure_reason = @failure_reason
 WHERE id = @id AND epoch = @epoch;

-- name: ListOutstandingLeases :many
-- Everything the deployment is still holding, for a drain.
--
-- THE PHASE LIST IS WHAT COMPUTE MEANS HERE: phases that imply compute exists or
-- that GitHub can still route to it. `capacity` is included even though nothing
-- is running under it, because a listener is advertising that slot and has
-- promised it to GitHub.
--
-- IT CANNOT SEE COMPUTE WHOSE LEASE HAS ALREADY GONE, which is why a drain has a
-- SECOND barrier that asks the hosts directly. See internal/state/queries/
-- barrier.sql.
SELECT id, tier, COALESCE(node, target_node, '') AS node, phase,
       CAST(COALESCE(CAST(run_id AS TEXT), '') AS TEXT) AS run_id,
       CAST(CASE WHEN held_at = '' THEN created_at ELSE held_at END AS TEXT) AS since,
       deregistered
  FROM leases
 WHERE phase IN ('capacity','assigned','launching','online','busy',
                 'custody','teardown','quarantine')
 ORDER BY CASE WHEN held_at = '' THEN created_at ELSE held_at END, id;
