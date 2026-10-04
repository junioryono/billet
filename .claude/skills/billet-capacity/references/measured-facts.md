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
