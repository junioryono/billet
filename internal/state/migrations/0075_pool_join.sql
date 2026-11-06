-- migration 75: pool_join
--
-- HOW A POOL HOST JOINS FROM ITS ORDER (ADR-030, #553).
--
-- join_tokens.pool_order is the order a token was minted for, empty for a token an
-- operator minted. Every send of an order mints one, and a resend under EC2's
-- ClientToken returns the host that booted with the first send's, so any live token
-- of the order admits it, and the admission retires the rest. A token bound to an
-- order enrolls only the name that order gives its node: the name comes from the
-- order, never from the request.
--
-- node_enrollments.pool_order is the order an enrollment came in under, so a person
-- approving it links the host to its node in the same transaction, as the automatic
-- admission does.
--
-- pool_orders.node_fingerprint and node_cert_serial are the key admitted from the
-- order and the certificate issued to it: a retried enrollment with that key finds
-- itself admitted, and a release revokes that serial before the host is given back.
--
-- pool_orders.join_hold is why a person must look before the order admits anything:
-- an enrollment whose key, document or address contradicted what the source says
-- of the host, or a host whose tokens all lapsed with nothing admitted. It is a
-- column, not a state, because migration 74's states are frozen, and it is set once,
-- the first reason kept.

-- +billet:statement
ALTER TABLE join_tokens ADD COLUMN pool_order TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE INDEX join_tokens_pool_order_idx ON join_tokens (pool_order) WHERE pool_order != ''
-- +billet:end

-- +billet:statement
ALTER TABLE node_enrollments ADD COLUMN pool_order TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN node_fingerprint TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN node_cert_serial TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_orders ADD COLUMN join_hold TEXT NOT NULL DEFAULT ''
-- +billet:end
