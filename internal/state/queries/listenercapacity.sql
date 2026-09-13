-- name: RecordListenerCapacity :exec
INSERT INTO listener_capacity (tier, observed_at, snapshot)
VALUES (@tier, @observed_at, @snapshot)
ON CONFLICT (tier) DO UPDATE SET
    observed_at = excluded.observed_at,
    snapshot = excluded.snapshot;

-- name: ReadListenerCapacity :one
SELECT tier, observed_at, snapshot FROM listener_capacity WHERE tier = @tier;
