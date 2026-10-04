# Measured facts

Part of the `billet-backup-restore` skill: what was measured, dated where it was recorded.

- The first rehearsal failed at `billet server --upgrade-probe` under the service account with permission denied on the App key.
- `VACUUM INTO`: refused in a transaction, refused on the query-only pool, refuses an existing destination, creates 0644.
- `O_RDWR` on a `0444` kernel: permission denied; chmod and fsync through a read-only descriptor succeed; a no-op chmod on a read-only mount: EROFS.
- A test hook failing every directory sync never reached the reuse branch's own flush, which is how deleting it survived a mutation run.
- Kernel lock mutation: removing the reaper's skip alone leaves the test green; removing it and widening the pattern turns it red.
