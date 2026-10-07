# internal/server

One scale-set listener per tier and the scheduler: sessions and messages against GitHub, escrow, the pool, heartbeats, cleanup and drains. Load `billet-capacity` and `billet-github-protocol` before changing anything here.

- Capacity is bought only when a runner starts: per offer, per pool launch, and per unpromised assignment. Every purchase asks the admission order (`mayBuy`); the advertisement is never gated.
- Exactly one party renews a lease, and a heartbeat pass's budget starts after it holds the mutex.
- A timer never authorises a teardown, and only the force operation destroys running work (`TestOnlyTheForceOperationDestroysRunningWork`).
- A failed session is reopened in place; the tier keeps what it holds.
- `Run` starts nothing with a bare `go`: the tiers and the reaper are essential tasks of an `internal/supervise` group, the keep-alive, coordinator and starter background ones, and the group is joined on every path out of `Run`, so a lease is still renewed while a tier stopped by another's failure unwinds.
- A test of a lease that must survive a stall drives the listener's own loop (`driveRenewal`) on the allocator's clock, never by sleeping past a wall-clock TTL.

Gates: `make check`. The replay harness (`internal/replay`) is the macro check of anything that changes placement or purchase order.
