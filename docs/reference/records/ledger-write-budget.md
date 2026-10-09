# The ledger's write budget

What a lease renewal, a purchase and a placement cost against the ledger's single writer slot, measured on 2026-10-08, and the decision it settles: billet does not batch heartbeats.

## Why it was measured

Every write to the ledger takes its one writer slot: SQLite's write lock, or PostgreSQL's advisory lock under a one-connection writer pool. In steady state a lease is renewed by one party, once a third of its TTL (every 30 seconds at the default 90), and each renewal is a write transaction of its own (`alloc.Heartbeat`). The question #356 left open was whether a fleet's renewals could fill the slot, so that scheduling queues behind them, and whether a `HeartbeatMany` (one transaction renewing many leases, with a fencing result per lease and a new wire route behind a protocol bump) is worth its cost.

## How

The benchmarks are in the tree and run against either engine: `make bench` on SQLite, and the same with `BILLET_TEST_LEDGER=postgres BILLET_TEST_POSTGRES_DSN=…` on PostgreSQL. The setup matches a running control plane in the ways that matter:

- The ledger holds the controller claim, so every write reads the claim's epoch inside its transaction, as production writes do.
- Setup is outside the timed region.
- A purchase is timed without the release that follows it.
- `slot-µs/write` is how long each measured write held the writer slot, taken from the ledger's own observer (the one `billet_ledger_write_held_seconds` is fed from).
- `BenchmarkPlaneDispatch` in `internal/nodeplane` measures a command's round trip through the plane, in process.

Each benchmark was run three times for two seconds, and the ranges below are the three runs.

- Hardware: an Apple M2 Max laptop, 12 cores.
- SQLite: the bundled driver on its SSD.
- PostgreSQL: 16.15 in Docker on the same laptop, over loopback through Docker's VM.

How a PostgreSQL server on its own hardware, storage and network compares is not measured here. The fleet is 16 hosts with room for 32 two-vCPU jobs each, and 500 open leases to renew. The purchase benchmarks run over four tiers with 50 leases each. Placement runs over 4, 32 and 128 hosts, with two leases per host per tier.

## What it found

| Operation | SQLite: time | SQLite: slot held | PostgreSQL: time | PostgreSQL: slot held |
|---|---|---|---|---|
| One renewal (`BenchmarkHeartbeat`) | 110 to 126 µs | 102 to 117 µs | 1.63 to 1.74 ms | 1.14 to 1.23 ms |
| Renewals from twelve goroutines at once (`BenchmarkHeartbeatContended`, time per renewal) | 112 to 130 µs | 104 to 120 µs | 1.69 to 1.87 ms | 1.19 to 1.31 ms |
| One purchase (`BenchmarkEscrow`) | 0.95 to 0.97 ms | 0.94 to 0.95 ms | 3.79 to 4.05 ms | 3.29 to 3.51 ms |
| One headroom read (`BenchmarkHeadroom`, the reader pool) | 0.81 to 0.82 ms | none | 2.69 to 2.91 ms | none |
| One purchase over 4 hosts (`BenchmarkPlacement`) | 0.32 ms | 0.30 to 0.31 ms | 3.20 to 3.42 ms | 2.75 to 2.90 ms |
| over 32 hosts | 2.48 to 2.57 ms | 2.46 to 2.56 ms | 5.12 to 5.53 ms | 4.63 to 4.99 ms |
| over 128 hosts | 29.6 to 30.5 ms | 29.6 to 30.4 ms | 32.4 to 33.4 ms | 31.8 to 32.8 ms |

The contended renewal is the slot's throughput: about 7,700 renewals a second on SQLite and about 530 on PostgreSQL, at the slowest of the three runs. A command's round trip through the plane to one host and back, in process and without the HTTP between them, is 4.3 µs, and to eight hosts answering in parallel 40 µs.

## The decision: no heartbeat batching

At 500 concurrent leases the steady-state renewals are 16.7 transactions a second. That is 0.22% of the slot on SQLite and 3.1% on PostgreSQL, taking the slowest contended run. Renewals would take a quarter of the slot at about 57,000 concurrent leases on SQLite and about 4,000 on PostgreSQL.

Steady state is the floor, not the whole of it:

- While a lease is launching the node renews it too, so it briefly has two renewers.
- Tending a lease the node holds in custody adds a renewal per tend pass.

Counting every lease twice still leaves PostgreSQL at 6.2% at 500 leases, and halves the threshold to about 2,000.

A `HeartbeatMany` would cost:

- a new route and a protocol-version bump;
- a fencing answer per lease, each of which a node must act on separately;
- a batch window that delays every renewal behind it.

None of that buys anything at these sizes, so billet keeps one transaction per renewal.

It is to be reopened when either of two things happens:

- `billet_ledger_write_held_seconds` shows the writer slot busy more than a quarter of the time on a deployment where renewals are most of its writes;
- a PostgreSQL deployment approaches 2,000 concurrent leases.

## What it found instead: placement grows with the fleet squared

A purchase over 128 hosts holds the writer slot for about 30 ms on either engine, against 0.3 ms (SQLite) over 4. A CPU profile of the 128-host case puts 72% of the time in `placer.next`, called from `placer.total` inside `headroomWithPlacer`. Headroom is counted by placing the tier's jobs one at a time until none fits, and each placement scans every host, so the count costs the free slots times the hosts, which grows with the square of the fleet. Every purchase on a fleet that size holds the slot for that long, and every other write waits behind it. That, not renewal, is the write-budget problem to fix first. The fix counts headroom without placing each job; it has its own pull request, and is held to the greedy count it replaces.
