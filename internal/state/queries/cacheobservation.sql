-- Cache observations: what the cache did for one job, as the node saw it.
--
-- AN OBSERVATION, NEVER A VERDICT, and from a CLOSED vocabulary the node writes
-- from what it saw rather than what the tier intended -- alloc.ImageCache and
-- alloc.ActionsCache hold the sets, and every new observation goes through their
-- Valid() before it reaches a statement here. The empty string is the zero value
-- and means nothing was observed.
--
-- THE FIRST OBSERVATION IS KEPT, and the guard is in the statement rather than in
-- a branch above it: each column is written only while it is empty, so a repeat
-- from a node retrying a lost response changes nothing and a later, different
-- observation cannot replace what the guest first saw. The generation moves with
-- the image-cache token so a report never shows a generation with nothing to
-- attribute it to. TestBothCacheObservationStatementsKeepTheFirst pins the guard
-- on both statements.

-- name: RecordLeaseCacheObservation :exec
-- Write the observation onto the live lease, fenced on its epoch.
UPDATE leases
   SET image_cache      = CASE WHEN image_cache = '' THEN CAST(@image_cache AS TEXT)
                               ELSE image_cache END,
       cache_generation = CASE WHEN image_cache = '' THEN CAST(@cache_generation AS TEXT)
                               ELSE cache_generation END,
       actions_cache    = CASE WHEN actions_cache = '' THEN CAST(@actions_cache AS TEXT)
                               ELSE actions_cache END
 WHERE id = @id AND epoch = @epoch;

-- name: RecordHistoryCacheObservation :exec
-- And onto its history row, the moment it is observed.
--
-- WRITTEN NOW, NOT LEFT TO archive, for the reason a disruption is: a lease
-- terminalizes whenever its teardown finally succeeds, which can be hours after
-- the job ended or never. Unfenced, like RecordHistoryDisruption: the row is
-- keyed by the lease and the observation is about the job, not about who holds
-- the lease. A lease that never reached Assign has no history row and this
-- updates nothing, which is correct: it ran no job.
UPDATE job_history
   SET image_cache      = CASE WHEN image_cache = '' THEN CAST(@image_cache AS TEXT)
                               ELSE image_cache END,
       cache_generation = CASE WHEN image_cache = '' THEN CAST(@cache_generation AS TEXT)
                               ELSE cache_generation END,
       actions_cache    = CASE WHEN actions_cache = '' THEN CAST(@actions_cache AS TEXT)
                               ELSE actions_cache END
 WHERE lease_id = @lease_id;
