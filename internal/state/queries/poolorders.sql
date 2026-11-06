-- The orders a pool of rented bare metal placed (ADR-030). Every move is one way,
-- so each update names the state it moves from and reports how many rows moved.

-- name: InsertPoolOrder :exec
INSERT INTO pool_orders (order_id, source, scope, shape, location, arch, state, first_attempt_at, updated_at)
VALUES (@order_id, @source, @scope, @shape, @location, @arch, 'ordering', @first_attempt_at, @updated_at);

-- name: ListPoolOrders :many
SELECT order_id, source, scope, shape, location, arch, state, host_id, node_name, reason, first_attempt_at, updated_at,
       node_fingerprint, node_cert_serial, join_hold
FROM pool_orders
ORDER BY first_attempt_at, order_id;

-- name: ReadPoolOrder :one
SELECT order_id, source, scope, shape, location, arch, state, host_id, node_name, reason, first_attempt_at, updated_at,
       node_fingerprint, node_cert_serial, join_hold
FROM pool_orders
WHERE order_id = @order_id;

-- name: MarkPoolOrderPlaced :execrows
UPDATE pool_orders SET state = 'placed', host_id = @host_id, reason = '', updated_at = @updated_at
WHERE order_id = @order_id AND state = 'ordering';

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
