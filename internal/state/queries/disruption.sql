-- Disruption: what billet's OWN infrastructure did to a lease while the job on
-- it may still have been running.
--
-- AN OBSERVATION, NEVER A VERDICT. Nothing here says a job failed because of a
-- disruption; what makes one interesting is reading it beside GitHub's own result
-- for the same job, which is why the two are recorded separately and neither is
-- stored as an answer.
--
-- THE GUARD IS WRITTEN TWICE, and it has to be: SQL has no way to compose a
-- predicate the way the Go constant it replaced did.
-- TestBothDisruptionStatementsPinTheirWholeStatement pins each statement's
-- COMPLETE text -- the locator as well as the guard -- because a guard that
-- drifts on one path is the path that attributes somebody's ordinary test failure
-- to billet, and a locator that widens is every lease in the deployment.
--
-- WHAT THE GUARD SAYS, clause by clause:
--
--   an empty disruption        The FIRST observation is the one kept. A later
--                              disruption is a consequence of the earlier one at
--                              least as often as it is a separate event, and only
--                              the earliest can still have been causal.
--
--   phase IN (...)             Phases in which a job COULD still have been
--                              running. Could, not was: `launching` admits a
--                              lease whose guest never started. `capacity` and
--                              `assigned` are absent because nothing was started
--                              under them, and attributing a build failure to one
--                              would attribute it to a machine that never ran the
--                              build. `quarantine` IS here, because quarantine
--                              exists precisely while compute is unconfirmed.
--
--   NOT EXISTS job_history     A disruption landing after GitHub has reported the
--   NOT EXISTS pending_...     job cannot have caused it. BOTH records are asked
--                              because pending_completions is written FIRST, when
--                              the listener makes the delivery durable, and
--                              job_history.result follows in a SECOND
--                              transaction: reading only the second leaves a
--                              committed interval in which a concurrent NodeGone
--                              or reclaim sees no result and attributes a job that
--                              had already finished. It also survives a crash
--                              between the two writes, since the pending row does
--                              and the column does not.

-- name: DisruptableLease :many
-- Whether one lease may still have a disruption recorded against it.
SELECT id FROM leases
 WHERE id = @id
   AND disruption = ''
   AND phase IN ('launching','online','busy','custody','teardown','quarantine')
   AND NOT EXISTS (SELECT 1 FROM job_history h
                    WHERE h.lease_id = leases.id AND h.result != '')
   AND NOT EXISTS (SELECT 1 FROM pending_completions p
                    WHERE p.lease_id = leases.id AND p.result != '');

-- name: DisruptableLeasesOnNode :many
-- Which of one host's leases may still have a disruption recorded against them.
--
-- COALESCE(node, target_node), the way the rest of this package attributes a
-- lease: a reservation aimed at a machine that is being given up on is as
-- disrupted as one bound to it.
SELECT id FROM leases
 WHERE COALESCE(node, target_node) = CAST(@node AS TEXT)
   AND disruption = ''
   AND phase IN ('launching','online','busy','custody','teardown','quarantine')
   AND NOT EXISTS (SELECT 1 FROM job_history h
                    WHERE h.lease_id = leases.id AND h.result != '')
   AND NOT EXISTS (SELECT 1 FROM pending_completions p
                    WHERE p.lease_id = leases.id AND p.result != '');

-- name: RecordLeaseDisruption :exec
-- Write the observation onto the live lease.
UPDATE leases SET disruption = @disruption, disrupted_at = @disrupted_at WHERE id = @id;

-- name: RecordHistoryDisruption :exec
-- And onto its history row.
--
-- WRITTEN NOW, NOT LEFT TO archive. A lease terminalizes whenever its teardown
-- finally succeeds, which can be hours after the job ended or never -- and a job
-- whose teardown is wedged on a host that vanished is exactly the one an operator
-- is looking for. A lease that never reached Assign has no history row and this
-- updates nothing, which is correct: it ran no job.
UPDATE job_history SET disruption = @disruption, disrupted_at = @disrupted_at
 WHERE lease_id = @lease_id;
