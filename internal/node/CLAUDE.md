# internal/node

The node runtime: it turns leases into compute, renews them, serves the caches, and holds custody of what it cannot account for. Load `billet-capacity` before changing anything here, and `billet-storage-and-cache` for the caches.

- Compute billet cannot account for is held in custody and renewed, never resold; it is released only on proof it is gone.
- A cleanup obligation is owed to the compute, not to the lease.
- `Recover` runs at every registration, so what it finds includes this process's own jobs.
- A drain waits for compute, not for entries: a running entry whose instance is proved ended stops counting.

Gates: `make check`.
