# Measured facts

Part of the `billet-state` skill: what was measured, dated where it was recorded.

- `SQLITE_BUSY_SNAPSHOT` (517) appeared the moment operator commands were admitted alongside the plane, with deferred transactions.
- A real promotion (2026-09-04, `make promotion-rehearsal`): with `tcp_keepalives_idle=10`, `_interval=5`, `_count=3` on PostgreSQL 18, the standby was promoted 21 seconds after the leader was partitioned, the node re-registered with it 62 seconds after, and the healed old leader stopped on `ErrLeadershipLost`, was restarted by systemd and stood by within 10 seconds. Under `controllers: active-passive` the first controller logs `standing by` and then `promoted to`, never `claimed`.
- Leadership check cost: 8.3µs→15.8µs SQLite, 419µs→585µs loopback PostgreSQL, per empty write transaction.
- sqlc's two engines: byte-identical Go with `BIGINT` casts; modernc.org/sqlite binds `$N` positionally, a repeated `$1` once, and a `$2` appearing before `$1` correctly.
- A single em dash in a query comment shifted every parameter offset after it.
- Splitting the deployment bind from the epoch: the incumbent's next write came back "host-b now holds it at epoch 2".
- The lease deregistration column (migration 30), compute barrier (37), rollout (39-42), controller claim (44), deployment binding (45), CodeBuild sweep (46), holder incarnation (47), release watermark and highest node release (48), the charged shape, its price, the site and the cache outcome on `leases` and `job_history` (49), the reason a host's last dispatch was refused on `rollout_nodes` (50), the controller-retirement row (51), dated listener ownership and advertisement observations in `listener_capacity` (52; reporting only, never admission or release authority), GitHub's job id, workflow ref, job name and event on `job_history` plus the first write of its long-declared `repo` (56; each column write-once and only for the same job id, from `RecordJobIdentity`), and `job_usage` and `job_series` (57; what the host measured a job do, one row each per lease, the first report kept, the series base64 in TEXT because the twin translator declares no BLOB substitution).
