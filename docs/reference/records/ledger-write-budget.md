# The ledger's write budget

What a lease renewal, a purchase and a placement cost against the ledger's single writer slot. Measured twice:
- **2026-10-08,** on a laptop, which decided that billet did not batch heartbeats and found placement growing with the fleet squared;
- **2026-10-25,** at the target fleet [ADR-021](../decisions/adr-021-fleet-envelope.md) commits to, on server hardware with SQLite and with a PostgreSQL on its own machine, each on network block storage and on NVMe ([below](#the-target-fleet-on-server-hardware-2026-10-25)). It reversed the first decision for the listener's renewals and closed the placement finding.

## Why it was measured

Every write to the ledger takes its one writer slot: SQLite's write lock, or PostgreSQL's advisory lock under a one-connection writer pool. In steady state a lease is renewed by one party, once a third of its TTL (every 30 seconds at the default 90), and each renewal is a write transaction of its own (`alloc.Heartbeat`). The question #356 left open was whether a fleet's renewals could fill the slot, so that scheduling queues behind them, and whether a `HeartbeatMany` (one transaction renewing many leases, with a fencing result per lease and a new wire route behind a protocol bump) is worth its cost.

## How

The benchmarks are in the tree and run against either engine: `scripts/gates/bench.sh` on SQLite, and the same with `BILLET_TEST_LEDGER=postgres BILLET_TEST_POSTGRES_DSN=…` on PostgreSQL. The setup matches a running control plane in the ways that matter:

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

Superseded on 2026-10-25 for the listener's renewals, which are now batched; [below](#the-target-fleet-on-server-hardware-2026-10-25) is why. The reasoning against a batch over the wire still holds for a node's renewals.

At 500 concurrent leases the steady-state renewals are 16.7 transactions a second. Two ways to put that against the slot:

- **Throughput**, at the slowest contended time per renewal: 0.22% of what the slot can do on SQLite and 3.1% on PostgreSQL.
- **Occupancy**, at the slowest measured hold per renewal (what `billet_ledger_write_held_seconds` reports): 0.20% and 2.2%.

The throughput fractions are the larger, and the thresholds below use them. On that measure renewals would take a quarter of the slot at about 57,000 concurrent leases on SQLite and about 4,000 on PostgreSQL.

Steady state is the floor, not the whole of it:

- While a lease is launching the node renews it too, so it briefly has two renewers.
- Tending a lease the node holds in custody adds a renewal per tend pass.

Counting every lease twice still leaves PostgreSQL at 6.2% of the slot's throughput (4.4% of its occupancy) at 500 leases, and halves the threshold to about 2,000.

A `HeartbeatMany` would cost:

- a new route and a protocol-version bump;
- a fencing answer per lease, each of which a node must act on separately;
- a batch window that delays every renewal behind it.

None of that buys anything at these sizes, so billet keeps one transaction per renewal.

It is to be reopened when either of two things happens:

- `billet_ledger_write_held_seconds` shows the writer slot busy more than a quarter of the time on a deployment where renewals are most of its writes;
- a PostgreSQL deployment approaches 2,000 concurrent leases.

## What it found instead: placement grows with the fleet squared

A purchase over 128 hosts holds the writer slot for about 30 ms on either engine on this hardware, against 0.3 ms (SQLite) over 4. That fixture has 1,024 jobs placed and 3,072 two-vCPU slots free across its 128 hosts. A CPU profile of the 128-host case puts 72% of the time in `placer.next`, called from `placer.total` inside `headroomWithPlacer`. Headroom is counted by placing the tier's jobs one at a time until none fits, and each placement scans every host. So the count costs the hosts times the placeable free slots, which grows with the square of a fleet that has room to spare. A fleet that is nearly full scans less, and every other write waits behind the purchase for however long it takes. That, not renewal, is the write-budget problem to fix first. The fix counts headroom without placing each job; it has its own pull request, and is held to the greedy count it replaces.

Closed: the one-pass count landed the same day for a tier whose candidates all charge the same, and on 2026-10-25 for every tier, one preference rank at a time. Placement now grows linearly with the fleet ([below](#the-target-fleet-on-server-hardware-2026-10-25)).

## The target fleet on server hardware, 2026-10-25

### Why it was measured again

Issue #528 asked what fleet one control plane must schedule, and for that target to be measured on server hardware, with PostgreSQL on its own machine rather than in Docker on a laptop. [ADR-021](../decisions/adr-021-fleet-envelope.md) sets the target: 256 hosts and 4,096 open leases, 14 purchases a second sustained, a 256-job matrix started within ten seconds, six writes in each job's life after its purchase, and 137 renewals a second. It also sets what each may take of the writer slot. A laptop could not answer that, because macOS's `fsync` does not wait for the disk, so a commit there costs almost nothing. On the server hardware below, a commit is most of what a write costs.

### How

The benchmarks are the 2026-10-08 set, extended:
- `BenchmarkPlacement` now also runs at 256 hosts, still at eight leases a host.
- `BenchmarkPlacementTarget` is a purchase on the target fleet: 256 hosts with room for 32 two-vCPU jobs each, holding 4,096 leases over four tiers.
- `BenchmarkPlacementMixed` is the same purchase for a tier that falls back from those hosts to one ec2 node, whose cheapest fitting shape (8 vCPU) costs more than the tier asks (2 vCPU).
- `BenchmarkPlacementFloors` is the same purchase while two other tiers each have 32 instances of their reserved floor outstanding.
- `BenchmarkLeaseLife` is a job's six writes after its purchase: assigned, bound, launching, online, busy and released, each its own transaction.
- `BenchmarkHeartbeat` and `BenchmarkHeartbeatContended` run at the 2026-10-08 size (500 leases) and at the target (4,096).
- `BenchmarkHeartbeatMany` is a listener's pass over the target's 4,096 leases, 256 to a transaction, reported as slot time per lease.

Each ran three times for two seconds; a range below is the three runs, and a single number is their median. "Before" is main at `0b2b0452` with only the benchmarks added; "after" is the change that closed #528. The test binary ran on the bench host and opened the ledger it names:

| Configuration | Ledger | Bench host | `fdatasync`, median |
|---|---|---|---|
| SQLite on gp3 | the bench host's EBS gp3 volume (300 GiB, 3,000 IOPS, 125 MB/s, ext4) | c7i.4xlarge, Xeon Platinum 8488C, 16 vCPU | 3.6 ms |
| SQLite on tmpfs | `/dev/shm` on the same host: no commit reaches a disk, which leaves billet's own work | c7i.4xlarge | none |
| SQLite on NVMe | the instance store of an m6id.xlarge (ext4, `noatime`) | m6id.xlarge, Xeon Platinum 8375C, 4 vCPU | 0.14 ms |
| PostgreSQL 18 on gp3 | its own m7i.xlarge (4 vCPU, 16 GiB, `shared_buffers` 4 GiB) on a 100 GiB gp3 volume, in the bench host's availability zone | c7i.4xlarge | not measured there |
| PostgreSQL 18 on NVMe | its own m6id.xlarge, its data on the instance store | c7i.4xlarge | 0.14 ms |

The `fdatasync` column is a probe, 300 4 KiB writes each followed by `fdatasync`, on the volume the ledger used.

### What it found

**A commit is what a write costs on network block storage.** One renewal held the slot 3.8 to 4.1 ms on SQLite on gp3 and 3.0 to 3.8 ms on PostgreSQL on gp3, against 0.09 ms of billet's own work (tmpfs). On NVMe it held the slot 0.25 ms (SQLite) and 1.1 to 1.4 ms (PostgreSQL, a network round trip per statement). The 2026-10-08 laptop's 0.1 ms was its `fsync`, not the ledger.

**One renewal at a time was half the slot at the target, on either engine on gp3**, before anything was scheduled: 137 renewals a second at about 4 ms each. A listener's pass now renews its leases 256 to a transaction, in one statement that answers each lease exactly as its own renewal would. Per lease, that holds the slot for:

| | SQLite gp3 | SQLite tmpfs | SQLite NVMe | PostgreSQL gp3 | PostgreSQL NVMe |
|---|---|---|---|---|---|
| One renewal, its own transaction | 3.83 | 0.093 | 0.248 | 3.64 | 1.31 |
| One renewal in a batch | 0.040 | 0.022 | 0.028 | 0.037 | 0.031 |

**A purchase grows linearly with the fleet, and a tier with a remote fallback no longer costs ten times as much.** Mean slot time of one purchase, in milliseconds (each benchmark reports the slot it held, summed, over the purchases it made):

| Purchase | SQLite gp3 | SQLite tmpfs | SQLite NVMe | PostgreSQL gp3 | PostgreSQL NVMe |
|---|---|---|---|---|---|
| 4 hosts, before → after | 4.08 → 4.12 | 0.38 → 0.26 | 0.64 → 0.46 | 6.76 → 5.29 | 3.33 → 2.48 |
| 32 hosts | 4.87 → 4.32 | 0.86 → 0.51 | 1.36 → 0.84 | 7.33 → 5.06 | 3.62 → 2.64 |
| 128 hosts | 6.77 → 5.06 | 2.44 → 1.29 | 3.42 → 1.81 | 7.99 → 5.21 | 4.84 → 3.12 |
| 256 hosts | 8.39 → 6.16 | 4.64 → 2.32 | 6.26 → 3.05 | 8.49 → 5.92 | 5.88 → 3.64 |
| the target fleet | 11.99 → 7.48 | 8.31 → 3.73 | 10.70 → 4.91 | 10.36 → 6.01 | 7.75 → 4.17 |
| the target, a tier falling back to ec2 | 69.04 → 7.51 | 64.78 → 3.80 | 95.62 → 4.94 | 68.28 → 6.88 | 65.14 → 3.75 |
| the target, two floors outstanding | 14.08 → 9.32 | 10.19 → 5.71 | 13.66 → 7.35 | 12.59 → 8.08 | 10.21 → 5.89 |

From 128 to 256 hosts, twice the fleet, billet's own work (tmpfs) went from 1.29 to 2.32 ms, 1.8 times: linear. The same doubling cost 1.9 times before this change, and on 2026-10-08, before the one-pass count, four times the fleet (32 to 128 hosts) cost twelve times. What moved it:
- headroom is counted one preference rank at a time, so only a rank whose hosts charge differently is placed job by job, over its own hosts. Every host of a host-backed backend charges a tier the same, so only a rank of several remote nodes with different cheapest shapes is placed job by job, and that rank costs its nodes times the jobs they hold;
- the placeable hosts are read once a transaction rather than once for every tier asked about;
- the floors' reads are skipped where no other tier keeps a floor, and where one does, they come from the same pass as the hosts' usage;
- the deployment's commitment is summed from that pass rather than read again;
- choosing a host works each candidate's room out once rather than three times.

**A job's six writes after its purchase are most of the slot at the target** on network block storage: 24.0 ms a job on SQLite on gp3 and 30.6 ms on PostgreSQL on gp3, against 1.3 ms of work (tmpfs), 2.6 ms on SQLite on NVMe and 12.6 ms on PostgreSQL on NVMe. Nothing here changed them.

### The target's arithmetic

The share of the writer slot each kind of write takes at the target: 13.7 purchases, 13.7 jobs' six writes and 137 renewals a second. Then a 256-job matrix arriving at once: its purchases and the five writes each of its jobs makes as it starts (its release comes at its end), in the slot the steady state leaves free.

| | Purchases | A job's writes | Renewals | Steady state | A 256-job matrix started in |
|---|---|---|---|---|---|
| SQLite gp3, before | 16.4% | 32.8% | 54.8% | **104%** | never |
| SQLite gp3, after | 10.2% | 32.7% | 0.5% | 43.5% | **12.4 s** |
| SQLite tmpfs, after | 5.1% | 1.7% | 0.3% | 7.1% | 1.3 s |
| SQLite NVMe, after | 6.7% | 3.5% | 0.4% | 10.6% | 2.0 s |
| PostgreSQL gp3, before | 14.1% | 39.7% | 51.3% | **105%** | never |
| PostgreSQL gp3, after | 8.2% | 41.7% | 0.5% | **50.4%** | **16.2 s** |
| PostgreSQL NVMe, after | 5.7% | 17.3% | 0.4% | 23.4% | 4.9 s |

Against the budget ADR-021 set before these were measured:
- **Renewals within a quarter of the slot:** met everywhere, at under 1%.
- **A purchase within 10 ms on average:** met on every configuration, for the target, a tier falling back to ec2, and two floors outstanding. The slowest mean is 9.3 ms, with floors on SQLite on gp3. A benchmark reports a mean, not a distribution of single purchases.
- **The steady state within half the slot:** met everywhere except PostgreSQL on gp3, at 50.4% (the three runs of a job's writes put it between 49.9% and 50.6%). Four fifths of that is a job's own six commits, at a network round trip and a gp3 `fdatasync` each. Storage that commits faster is what brings it down: on NVMe the same PostgreSQL is at 23%.
- **A 256-job matrix started within ten seconds:** met on NVMe (2.0 s SQLite, 4.9 s PostgreSQL), missed on gp3 (12.4 s SQLite, 16.2 s PostgreSQL). There, too, a job's own commits are most of it: five of a job's six writes come as it starts.

Before this change, neither engine on gp3 could have scheduled the target at all: renewals alone took half the slot, and the steady state needed more than all of it.

### Measured and not adopted

- **A covering index for the usage aggregation.** A partial index on a lease's host, its vCPU, memory and macOS slot removed the sort from the per-host usage query: 1.25 ms to 0.48 ms on the target's ledger, in SQLite's own command line on the M2 Max. It barely moved the floors' count (1.38 to 1.12 ms). That is about a millisecond of a purchase, against a migration, its PostgreSQL twin, and the index's upkeep on every lease write.
- **Reading usage by host and tier for every purchase.** It let the floors count from the same pass, but four tiers' rows for each host cost a purchase with no floor more than the second pass saved: the target purchase on tmpfs went from 4.4 to 5.3 ms. Usage is read by tier only where some tier keeps a floor.
- **Packing the renewals the way the destinations are packed.** Cutting one record off the front, with the rest carried, copies the string once for every record. For 256 renewals that took 30 ms a statement in SQLite's command line on the M2 Max, against 3.5 ms when the string is cut in halves.
