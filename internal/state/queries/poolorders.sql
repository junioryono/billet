-- The orders a pool of rented bare metal placed (ADR-030). Every move is one way,
-- so each update names the state it moves from and reports how many rows moved.

-- name: InsertPoolOrder :exec
INSERT INTO pool_orders (order_id, source, scope, shape, location, arch, state, first_attempt_at, updated_at)
VALUES (@order_id, @source, @scope, @shape, @location, @arch, 'ordering', @first_attempt_at, @updated_at);

-- name: ListPoolOrders :many
SELECT order_id, source, scope, shape, location, arch, state, host_id, node_name, reason, first_attempt_at, updated_at,
       node_fingerprint, node_cert_serial, join_hold, release_requested_at, release_reason, release_contradicted,
       clear_epoch, clear_dispatch, clear_since, clear_observed_at, clear_generation
FROM pool_orders
ORDER BY first_attempt_at, order_id;

-- name: ReadPoolOrder :one
SELECT order_id, source, scope, shape, location, arch, state, host_id, node_name, reason, first_attempt_at, updated_at,
       node_fingerprint, node_cert_serial, join_hold, release_requested_at, release_reason, release_contradicted,
       clear_epoch, clear_dispatch, clear_since, clear_observed_at, clear_generation
FROM pool_orders
WHERE order_id = @order_id;

-- name: MarkPoolOrderPlaced :execrows
UPDATE pool_orders SET state = 'placed', host_id = @host_id, reason = '', updated_at = @updated_at
WHERE order_id = @order_id AND state = 'ordering';

-- name: MarkUncertainPoolOrderFound :execrows
UPDATE pool_orders SET state = 'placed', host_id = @host_id, reason = '', updated_at = @updated_at
WHERE order_id = @order_id AND state = 'uncertain';

-- name: MarkPoolOrderUncertain :execrows
UPDATE pool_orders SET state = 'uncertain', reason = @reason, updated_at = @updated_at
WHERE order_id = @order_id AND state = 'ordering';

-- name: MarkPoolOrderRefused :execrows
UPDATE pool_orders SET state = 'refused', reason = @reason, updated_at = @updated_at
WHERE order_id = @order_id AND state = 'ordering';

-- name: MarkPoolOrderAbandoned :execrows
UPDATE pool_orders SET state = 'abandoned', reason = @reason, updated_at = @updated_at
WHERE order_id = @order_id AND state = 'uncertain';

-- name: LinkPoolOrderNode :execrows
-- The node that joined from a placed order, the key it was admitted with and the
-- certificate it was handed. Once: an order's node is never replaced. The hold is the
-- one the caller read, so a hold committed since moves nothing, and a link clears it:
-- a person's approval of a held order is what the hold waited for.
UPDATE pool_orders
   SET node_name = @node_name, node_fingerprint = @node_fingerprint, node_cert_serial = @node_cert_serial,
       join_hold = '', updated_at = @updated_at
 WHERE order_id = @order_id AND state = 'placed' AND node_name = '' AND join_hold = @join_hold;

-- name: HoldPoolOrderJoin :execrows
-- Hold an order's join for a person, with the first reason kept, only while a node may
-- still join from it, and only as the order stood when it was judged (@seen, its
-- updated_at then): a verdict reached before a person released a hold does not hold it
-- again after.
UPDATE pool_orders SET join_hold = @join_hold, updated_at = @updated_at
 WHERE order_id = @order_id AND join_hold = '' AND state = 'placed' AND node_name = '' AND updated_at = @seen;

-- name: ReleasePoolOrderJoin :execrows
-- A person releases an order's held join, so its host's next request is judged again.
UPDATE pool_orders SET join_hold = '', updated_at = @updated_at
 WHERE order_id = @order_id AND join_hold != '' AND state = 'placed' AND node_name = '';

-- name: HoldLapsedPoolJoin :execrows
-- HoldLapsedPoolJoins for one order, which a host whose token lapsed asks about: the
-- same proof that nothing can join from it, so a pool loop is not what it waits on.
UPDATE pool_orders SET join_hold = @join_hold, updated_at = @updated_at
 WHERE order_id = @order_id AND state = 'placed' AND node_name = '' AND join_hold = ''
   AND NOT EXISTS (SELECT 1 FROM join_tokens t
                    WHERE t.pool_order = pool_orders.order_id AND t.uses_remaining > 0 AND t.expires_at > @now)
   AND NOT EXISTS (SELECT 1 FROM node_enrollments e
                    WHERE e.pool_order = pool_orders.order_id AND e.state = 'pending');

-- name: HoldLapsedPoolJoins :execrows
-- Hold every placed order nothing joined from that has no live token left and no
-- request waiting for a person: its host can no longer join, and without a hold the
-- pool would count it on its way forever.
UPDATE pool_orders SET join_hold = @join_hold, updated_at = @updated_at
 WHERE state = 'placed' AND node_name = '' AND join_hold = ''
   AND NOT EXISTS (SELECT 1 FROM join_tokens t
                    WHERE t.pool_order = pool_orders.order_id AND t.uses_remaining > 0 AND t.expires_at > @now)
   AND NOT EXISTS (SELECT 1 FROM node_enrollments e
                    WHERE e.pool_order = pool_orders.order_id AND e.state = 'pending');

-- name: RequestPoolOrderRelease :execrows
-- A person asks for a joined host's release: the moment placement stops. Once, and only
-- of a placed order.
UPDATE pool_orders SET release_requested_at = @now, release_reason = @reason, updated_at = @now
 WHERE order_id = @order_id AND state = 'placed' AND release_requested_at = '';

-- name: SealPoolOrder :execrows
-- The release's seal, written in the transaction that read its proof: nothing joins
-- from the order or is placed on its host again. A joined order is sealed only once a
-- release was asked for; one nothing joined from is sealed by the asking. The node is
-- the one the proof was read for, so a link committed since seals nothing.
UPDATE pool_orders
   SET state = 'sealed', release_requested_at = @requested_at, release_reason = @reason, updated_at = @now
 WHERE order_id = @order_id AND state = 'placed' AND node_name = @node_name
   AND (node_name = '' OR release_requested_at != '');

-- name: ContradictPoolRelease :execrows
-- The order's node said it is back or runs something (ADR-030): it arrived to register,
-- or an inventory of its current registration reported compute. Its release is proved
-- by no decommission recorded before this; the decommission itself is left as the
-- person recorded it, for the readers that are not the release's. An order linked to
-- its node only by its release is contradicted at the link instead (RequestPoolRelease),
-- because what its node said before cannot be read back. Every pool order the node is
-- the node of counts, contradicted already or not, so a caller can tell a pool node from
-- any other.
--
-- AND IT ENDS THE ORDER'S CLEARANCE RUN, in this one statement, so no caller can record a
-- contradiction and leave the run standing: a run that reaches proved therefore began
-- after the last contradiction, and is proof for the order whatever release_contradicted
-- says (migration 80).
UPDATE pool_orders SET release_contradicted = 1, clear_since = '', clear_observed_at = '',
       clear_generation = clear_generation + 1
 WHERE node_name = @node AND node_name != '' AND state IN ('placed', 'sealed');

-- name: ContradictWaitingPoolRelease :exec
-- The order's node arrived to register while its release still waits on it (ADR-030):
-- ContradictPoolRelease for a placed order alone. A sealed order is not contradicted by
-- an arrival: the seal revoked the node's certificates, so a node that comes back, as one
-- proved idle by its own answers and never stopped will, is refused and can never be
-- handed work, and its arrival says nothing of the compute the seal proved absent.
UPDATE pool_orders SET release_contradicted = 1, clear_since = '', clear_observed_at = '',
       clear_generation = clear_generation + 1
 WHERE node_name = @node AND node_name != '' AND state = 'placed';

-- name: ListPoolClearanceTargets :many
-- The orders whose node the control plane asks what it runs, for a release that proves
-- itself without the deployment's seal (migration 80): placed, a release asked for, a node
-- linked, and nothing in the ledger holding that node. A sealed order is never asked: its
-- node's certificates are revoked, a failed ask would end the run its proof is read from.
SELECT p.order_id, p.node_name, p.clear_generation FROM pool_orders p
 WHERE p.state = 'placed' AND p.release_requested_at != '' AND p.node_name != ''
   AND NOT EXISTS (SELECT 1 FROM leases l
                    WHERE COALESCE(l.node, l.target_node) = p.node_name AND l.phase NOT IN ('done','failed'))
 ORDER BY p.order_id;

-- name: RecordPoolClearanceRun :execrows
-- One empty answer to the order's clearance, taken against the fence it names: the run's
-- first answer is kept only while the fence it was taken under still holds, so a run is
-- continuous across one epoch and one launch-dispatch generation or it starts again.
UPDATE pool_orders
   SET clear_since = CASE WHEN clear_epoch = @epoch AND clear_dispatch = @dispatch AND clear_since != ''
                          THEN clear_since ELSE @recorded END,
       clear_epoch = @epoch, clear_dispatch = @dispatch, clear_observed_at = @asked
 WHERE order_id = @order_id AND state = 'placed' AND release_requested_at != '' AND node_name = @node
   AND clear_generation = @generation;

-- name: EndPoolClearanceRun :exec
-- An answer that was not empty, or not an answer, or not taken where the run could stand:
-- the run is over. Only while the order waits on its node: a sealed order's run is what
-- its proof is read from, and an answer that arrives after the seal does not end it (a
-- contradiction still does, through ContradictPoolRelease).
UPDATE pool_orders SET clear_since = '', clear_observed_at = ''
 WHERE order_id = @order_id AND state = 'placed';

-- name: ClearPoolReleaseContradiction :exec
-- A fresh decommission on the compute barrier's proof, in its own transaction: what the
-- node said before it no longer outweighs a proof taken since. A sealed order stays
-- contradicted, because its node's certificates are revoked and nothing proves it again.
UPDATE pool_orders SET release_contradicted = 0
 WHERE node_name = @node AND node_name != '' AND state = 'placed' AND release_contradicted = 1;

-- name: MarkPoolOrderReleased :execrows
-- The source took the sealed host back.
UPDATE pool_orders SET state = 'released', updated_at = @updated_at
 WHERE order_id = @order_id AND state = 'sealed';

-- name: MarkPoolOrderGone :execrows
-- The source said, in its own terminal answer, that the released host is gone.
UPDATE pool_orders SET state = 'gone', reason = @reason, updated_at = @updated_at
 WHERE order_id = @order_id AND state = 'released';

-- name: ClosePoolOrder :execrows
-- A person closes an order whose host's release is proved (sealed) or done (released),
-- on their word that the source took it back; the reason records that it was them.
UPDATE pool_orders SET state = 'gone', reason = @reason, updated_at = @updated_at
 WHERE order_id = @order_id AND state IN ('sealed', 'released');

-- name: ForceClosePoolOrder :execrows
-- A person closes an order whose host's release was asked for and never proved, on their
-- word that they took the host back by hand: the node that died, or lost the credential
-- it would prove itself with. Only one a release was asked for of and still linked to the
-- node the caller read, so a seal or another closure committed since moves nothing.
UPDATE pool_orders SET state = 'gone', reason = @reason, updated_at = @updated_at
 WHERE order_id = @order_id AND state = 'placed' AND release_requested_at != '' AND node_name = @node_name;

-- name: CountPoolOrderAdmissions :one
-- Everything that would show a node joined from an order: an enrollment approved under
-- it, a request approved under its name, and a certificate ever issued for its name.
SELECT CAST((SELECT COUNT(*) FROM node_enrollments e
              WHERE (e.pool_order = @order_id OR e.name = @node_name) AND e.state = 'approved')
          + (SELECT COUNT(*) FROM issued_certs c WHERE c.node = @node_name) AS BIGINT);

-- name: ListPoolIdleHosts :many
-- The joined pool hosts that hold nothing, with when each last held something: the end of
-- its newest job and its node's admission, the later of which the scaler reads as the
-- start of its idleness (ADR-030). A host whose release was asked for, held for a person,
-- or not placed is none of them. COALESCE(node, target_node), as everywhere: an escrow
-- aimed at a host has committed its room.
SELECT p.order_id, p.node_name, p.arch,
       CAST(COALESCE((SELECT MAX(h.finished_at) FROM job_history h WHERE h.node = p.node_name), '') AS TEXT) AS last_finished,
       CAST(COALESCE((SELECT MAX(e.decided_at) FROM node_enrollments e
                       WHERE e.name = p.node_name AND e.state = 'approved'), '') AS TEXT) AS joined
  FROM pool_orders p
 WHERE p.state = 'placed' AND p.release_requested_at = '' AND p.node_name != '' AND p.join_hold = ''
   AND NOT EXISTS (SELECT 1 FROM leases l
                    WHERE COALESCE(l.node, l.target_node) = p.node_name AND l.phase NOT IN ('done','failed'))
 ORDER BY p.order_id;

-- name: ReadPoolIdleHost :one
-- One order's row of ListPoolIdleHosts, read again in the transaction that asks for its
-- release, so a host that took work since is not proposed.
SELECT p.order_id, p.node_name, p.arch,
       CAST(COALESCE((SELECT MAX(h.finished_at) FROM job_history h WHERE h.node = p.node_name), '') AS TEXT) AS last_finished,
       CAST(COALESCE((SELECT MAX(e.decided_at) FROM node_enrollments e
                       WHERE e.name = p.node_name AND e.state = 'approved'), '') AS TEXT) AS joined
  FROM pool_orders p
 WHERE p.order_id = @order_id AND p.state = 'placed' AND p.release_requested_at = '' AND p.node_name != ''
   AND p.join_hold = ''
   AND NOT EXISTS (SELECT 1 FROM leases l
                    WHERE COALESCE(l.node, l.target_node) = p.node_name AND l.phase NOT IN ('done','failed'));
