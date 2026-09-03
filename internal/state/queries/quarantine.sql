-- Quarantine and custody: capacity held because compute could not be confirmed
-- gone.
--
-- NOTHING HERE FREES CAPACITY ON A TIMEOUT. A quarantined lease is released by a
-- proof -- an inventory taken under the registration this deployment is talking
-- to, a node reporting a destroy, or an operator asserting a machine is never
-- coming back. Every statement that terminalizes one is fenced by something that
-- postdates the observation it acts on.
--
-- THE PHASES ARE PARAMETERS, NOT LITERALS. They are Go constants and passing them
-- means there is no second spelling of the vocabulary to drift from.

-- name: ListHeldLeases :many
-- Every operator-visible proof obligation, oldest first.
--
-- WITH ITS HOST'S CURRENT PROCESS BESIDE THE HOLDER'S, so the report can say
-- whether the process that took the obligation is the one the deployment talks
-- to now. LEFT JOIN, because a lease may name a host the ledger has never
-- registered, and that lease is held all the same.
SELECT l.id, l.tier, COALESCE(l.node, l.target_node, '') AS node, l.phase, l.vcpu,
       l.memory, l.held_at, l.force_release, l.holder_incarnation,
       n.incarnation AS node_incarnation, n.live AS node_live,
       n.last_seen_at AS node_seen_at
  FROM leases l
  LEFT JOIN nodes n ON n.name = COALESCE(l.node, l.target_node)
 WHERE l.phase IN (@custody, @teardown, @quarantine)
 ORDER BY l.held_at, l.id;

-- name: ListRunningLeasesWithReplacedHolder :many
-- Every lease in a running phase whose holding process is not the one its host
-- registered with, oldest renewal first.
--
-- THE SHAPE AN OPERATOR ONCE HAD NOTHING TO READ ABOUT: a completion bound to a
-- process that died, its lease renewed by the listener and reported held by
-- nobody. The process that was given the work is not the one this host's commands
-- reach; the lease is still charged, and what settles it is its expiry and the
-- host's inventory. INCARNATIONS, NOT EPOCHS: the epoch moves on every
-- registration and the same process registers again after a control-plane
-- restart, so comparing epochs would report every surviving lease as replaced
-- after each one. An empty incarnation on either side was never recorded and is
-- not compared.
SELECT l.id, l.tier, COALESCE(l.node, l.target_node, '') AS node, l.phase, l.vcpu,
       l.memory, l.holder_incarnation, l.heartbeat_at,
       n.incarnation AS node_incarnation, n.live AS node_live,
       n.last_seen_at AS node_seen_at
  FROM leases l
  JOIN nodes n ON n.name = COALESCE(l.node, l.target_node)
 WHERE l.phase IN (@launching, @online, @busy)
   AND l.holder_incarnation <> '' AND n.incarnation <> ''
   AND n.incarnation <> l.holder_incarnation
 ORDER BY l.heartbeat_at, l.id;

-- name: ListQuarantinedLeases :many
-- The leases holding capacity for compute nobody has accounted for, oldest
-- first.
--
-- WHAT AN OPERATOR LOOKS AT WHEN CAPACITY IS MISSING: a quarantined lease is the
-- one thing that shrinks a fleet without anything having failed.
SELECT id, tier, COALESCE(node, target_node, '') AS node, vcpu, memory, heartbeat_at
  FROM leases WHERE phase = @phase ORDER BY heartbeat_at;

-- name: ListQuarantinedLeaseIDsOn :many
-- The quarantined leases attributed to one host, optionally only those that
-- expired before an instant.
--
-- THE EMPTY SETTLED VALUE MEANS UNFILTERED, expressed in SQL rather than by
-- building the statement two ways: a node asking which of its containers
-- something is still waiting for must see EVERY quarantine, and a young one is
-- exactly the one it must not destroy. A narrowing applied in one branch and
-- forgotten in another is how that goes wrong.
SELECT id FROM leases
 WHERE phase = @phase AND COALESCE(node, target_node) = CAST(@node AS TEXT)
   AND (CAST(@settled AS TEXT) = '' OR expires_at < CAST(@settled AS TEXT));

-- name: TerminalizeQuarantinedLease :exec
-- Settle a quarantined lease with the caller's outcome, bumping the fence.
--
-- THE OUTCOME IS THE CALLER'S. A listener resolving one after a completion knows
-- the job finished; a node cleaning up compute it could not account for, and an
-- operator forcing a machine that is never coming back, both know the opposite.
-- Recording every one of them as failed puts a lie in the history of a job
-- GitHub reported completed.
UPDATE leases SET phase = @phase, epoch = epoch + 1 WHERE id = @id;

-- name: TerminalizeQuarantinedLeaseWithReason :exec
-- Fail a quarantined lease whose guest an inventory did not contain.
--
-- SEPARATE FROM THE STATEMENT ABOVE because it also records WHY, and that reason
-- may be the provisional marker a later GitHub completion is allowed to correct.
UPDATE leases
   SET phase = 'failed', epoch = epoch + 1, failure_reason = @failure_reason
 WHERE id = @id;

-- name: FenceQuarantinedLease :exec
-- Settle a quarantined lease, refusing one whose epoch moved.
--
-- THE EPOCH IS IN THE WHERE, unlike the two above: this is the operator's force
-- path, which read the lease and then decided, so the write must not land on a
-- row something else has since taken over.
UPDATE leases SET phase = @phase, epoch = epoch + 1 WHERE id = @id AND epoch = @epoch;

-- name: RequestForceRelease :exec
-- Ask a live custody holder to give up a lease.
--
-- IT DOES NOT TERMINALIZE. Custody and teardown have a process that still
-- believes it owns the proof obligation; this makes its next heartbeat return
-- ErrForceRelease so that process drops its record and releases the lease
-- itself, rather than the ledger changing underneath it.
--
-- THE OPERATOR'S ASSERTION IS RECORDED AS THE REASON, in the same statement,
-- unless a reason is already there: the holder releases the lease as failed
-- and the history would otherwise carry a failure nothing explains.
UPDATE leases
   SET force_release = 1,
       failure_reason = CASE WHEN failure_reason = '' THEN CAST(@reason AS TEXT)
                             ELSE failure_reason END
 WHERE id = @id AND epoch = @epoch;

-- name: ReadLeaseSettlement :one
-- What settling one lease against a completion needs to know.
SELECT phase, epoch, failure_reason FROM leases WHERE id = @id;

-- name: CorrectProvisionalLease :exec
-- Replace inventory's provisional verdict with GitHub's own.
--
-- ONLY INVENTORY'S PROVISIONAL VERDICT MAY BE CORRECTED, which the caller checks
-- by comparing failure_reason: an operator or a node can independently fail a
-- quarantined lease at a later epoch too, so epoch order is a fence and never
-- provenance.
UPDATE leases SET phase = @phase, failure_reason = '' WHERE id = @id;

-- name: CorrectProvisionalHistory :exec
-- The same correction, in the history row.
UPDATE job_history SET conclusion = @conclusion, failure_reason = '' WHERE lease_id = @lease_id;

-- name: ExpireLease :exec
-- Move a lease's expiry, which is how a test drives the real reaper.
UPDATE leases SET expires_at = @expires_at WHERE id = @id;

-- name: UpsertNodeInventory :exec
-- Record what a host last said it was running.
--
-- EVIDENCE, NOT PROOF. The node lists its provider and THEN posts, so
-- received_at is when the report ARRIVED and never when the snapshot was taken --
-- a launch can be dispatched to that host immediately afterwards. Nothing may
-- read this as clearance.
INSERT INTO node_inventory (node, node_epoch, received_at, running)
VALUES (@node, @node_epoch, @received_at, @running)
ON CONFLICT(node) DO UPDATE SET
     node_epoch = excluded.node_epoch,
     received_at = excluded.received_at,
     running = excluded.running;

-- name: ListNodeInventories :many
-- What every known host last said it was running.
--
-- NULLABLE RATHER THAN COALESCED TO A SENTINEL. A host with no row and a host
-- reporting a number must not arrive as the same type, or the distinction has to
-- be recovered by comparing against a magic value -- which is how an absent
-- report becomes a zero somewhere downstream. The LEFT JOIN is what produces the
-- nulls, and the caller compares i.node_epoch against n.epoch, because a report
-- from before the host reconnected is not this host's current word.
SELECT n.name, n.live, n.epoch, i.node_epoch, i.running, i.received_at
  FROM nodes n
  LEFT JOIN node_inventory i ON i.node = n.name
 ORDER BY n.name;
