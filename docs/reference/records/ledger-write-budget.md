# The ledger's write budget

What a lease renewal, a purchase and a placement cost against the ledger's single writer slot, measured on 2026-10-08, and the decision it settles: billet does not batch heartbeats.

## Why it was measured

Every write to the ledger takes its one writer slot: SQLite's write lock, or PostgreSQL's advisory lock under a one-connection writer pool. A lease is renewed by exactly one party, once a third of its TTL (every 30 seconds at the default 90), and each renewal is a write transaction of its own (`alloc.Heartbeat`). The question #356 left open was whether a fleet's renewals could fill the slot, so that scheduling queues behind them, and whether a `HeartbeatMany` (one transaction renewing many leases, with a fencing result per lease and a new wire route behind a protocol bump) is worth its cost.

## How

The benchmarks are in the tree and run against either engine: `go test -run '^$' -bench . ./internal/alloc/` on SQLite, and the same with `BILLET_TEST_LEDGER=postgres BILLET_TEST_POSTGRES_DSN=…` on PostgreSQL. `BenchmarkPlaneDispatch` in `internal/nodeplane` measures the node wire's command round trip. Each was run three times for two seconds; the ranges below are the three runs.

- Hardware: an Apple M2 Max laptop, 12 cores, with nothing else heavy running.
- SQLite: the bundled driver on the laptop's SSD.
- PostgreSQL: 16.15 in Docker on the same laptop, over loopback through Docker's VM. A server on its own host is likely faster per transaction, so these numbers are the pessimistic side.

The fleet is 16 hosts with room for 32 two-vCPU jobs each, and 500 open leases to renew. The purchase benchmarks run over four tiers with 50 leases each. Placement runs over 4, 32 and 128 hosts, with two leases per host per tier.

## What it found

| Operation | SQLite | PostgreSQL |
|---|---|---|
| One renewal (`BenchmarkHeartbeat`) | 112 to 124 µs | 1.40 to 1.47 ms |
| One renewal, twelve renewing at once (`BenchmarkHeartbeatContended`, per renewal) | 109 to 122 µs | 1.81 to 1.90 ms |
| One purchase and its release (`BenchmarkEscrow`) | 1.24 to 1.29 ms | 5.0 to 6.2 ms |
| One headroom read (`BenchmarkHeadroom`, the reader pool) | 0.83 to 0.87 ms | 2.7 to 3.2 ms |
| A purchase over 4 hosts (`BenchmarkPlacement`) | 0.53 to 0.59 ms | 4.7 to 5.0 ms |
| over 32 hosts | 2.6 to 2.8 ms | 6.5 to 6.9 ms |
| over 128 hosts | 30.0 to 30.4 ms | 34.5 to 35.7 ms |

The contended renewal is the slot's throughput: about 8,500 renewals a second on SQLite and about 540 on PostgreSQL. A command's round trip through the plane to one host and back, without the HTTP between them, is 4.3 µs, and to eight hosts answering in parallel 40 µs. The wire is not where the time goes.

## The decision: no heartbeat batching

At 500 concurrent leases the renewals are 16.7 transactions a second. That is 0.2% of the slot on SQLite and 3.2% on PostgreSQL. Renewals would take a quarter of the slot at about 62,000 concurrent leases on SQLite and about 3,900 on PostgreSQL. Both are far beyond any deployment billet has run.

A `HeartbeatMany` would cost:

- a new route and a protocol-version bump;
- a fencing answer per lease, each of which a node must act on separately;
- a batch window that delays every renewal behind it.

None of that buys anything at these sizes. So billet keeps one transaction per renewal.

It is to be reopened when either of two things happens:

- `billet_ledger_write_held_seconds` shows the writer slot busy more than a quarter of the time, on a deployment where renewals are most of its writes;
- a PostgreSQL deployment approaches 3,000 concurrent leases.

The metric came with #433, and is the measurement to watch.

## What it found instead: placement grows with the fleet squared

A purchase over 128 hosts holds the writer slot for 30 ms, against half a millisecond over 4. A profile of the 128-host case puts 72% of the time in `placer.next`, called from `placer.total` inside `headroomWithPlacer`. Headroom is counted by placing the tier's jobs one at a time until none fits, and each placement scans every host. So the count costs the free slots times the hosts, which grows with the square of the fleet. A deployment of a hundred hosts taking a burst of a hundred purchases would hold the slot for three seconds doing arithmetic. That, not renewal, is the write-budget problem to fix first. The fix counts headroom without placing each job; it has its own pull request, and is held to the greedy count it replaces.
