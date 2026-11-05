-- The orders a pool of rented bare metal placed (ADR-028). Every move is one way,
-- so each update names the state it moves from and reports how many rows moved.

-- name: InsertPoolOrder :exec
INSERT INTO pool_orders (order_id, source, scope, shape, location, arch, state, first_attempt_at, updated_at)
VALUES (@order_id, @source, @scope, @shape, @location, @arch, 'ordering', @first_attempt_at, @updated_at);

-- name: ListPoolOrders :many
SELECT order_id, source, scope, shape, location, arch, state, host_id, node_name, reason, first_attempt_at, updated_at
FROM pool_orders
ORDER BY first_attempt_at, order_id;

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
