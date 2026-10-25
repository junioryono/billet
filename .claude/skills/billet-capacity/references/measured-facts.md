# Measured facts

Part of the `billet-capacity` skill: what was measured, dated where it was recorded.

- 28 grants against a ceiling of 4 with the headroom check outside the transaction.
- First real long poll: about 88 seconds against a 90-second lease TTL.
- Cadence from `DefaultLeaseTTL` under a shorter configured TTL: advertised capacity climbed to six times the budget.
- 700ms mutex stall versus a 300ms TTL reproduces `Running() = 0, want 1` in `TestACancellationInsideTheLongPollStillDrains`.
- A live node destroys a stray 45ms after its lease goes terminal, which is why the compute-barrier e2e test stops the control plane first and sets the reaper tick to an hour.
- A stopped host kept receiving placements for about 4.5 minutes before the withdraw route existed.
- Docker's `--filter name=X` is a substring match: `billet-abc` returns `billet-abcdef`; `Find` compares exactly afterwards.
- The shutdown grace was 90 seconds against a 10-minute node command timeout, so ordinary slow destroys tripped it; a bound must be larger than the longest legitimate operation under it.
- A CodeBuild holder-gone reproduction found a restart re-adopting a running teardown as custody (refused on every tend) and a teardown outcome that lived only in the dead process's memory; both fixed, both in `internal/e2e/holdergone_test.go`.
- At the target fleet (256 hosts, 4,096 open leases, ADR-021), on 2026-10-25, one renewal a transaction took more than half the writer slot on either engine on EBS gp3; a listener's pass renewing 256 leases a transaction holds it 0.022 to 0.040 ms a lease. Placing every job one at a time for a tier falling back to a remote shape held it 64.8 to 95.6 ms a purchase; counting a preference rank at a time holds it 3.8 to 7.5 ms. Every number is in `docs/reference/records/ledger-write-budget.md`.
- A job's six writes after its purchase (assigned, bound, launching, online, busy, released) are most of the slot at the target on network block storage: 24 ms a job on SQLite on gp3 and 31 ms on PostgreSQL on gp3, which is four fifths of PostgreSQL's 50% (2026-10-25).
