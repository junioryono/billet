# internal/alloc

The capacity allocator: escrow, leases and their state machine, placement, floors, the compute barrier, quarantine, force operations and enrollment records. Load `billet-capacity` and `billet-state` before changing anything here.

- billet never promises a machine it does not have, and hands capacity back only on proof the compute is gone.
- The headroom check and the insert are one transaction; moving the check outside it once produced 28 grants against a ceiling of 4 (`TestConcurrentReservationsNeverOvercommit`).
- `validTransitions` is the lease state machine, and a terminal phase has no successor.
- Every lease write presents its epoch, and a stale one is refused with `ErrFenced`.
- This package's SQL lives in `internal/state/queries`, and its production files reach no network, run no subprocess and import no upper layer (`ledgerwriters`, which exempts tests).

Gates: `make check`; `BILLET_TEST_LEDGER=postgres go test ./internal/alloc` with `BILLET_TEST_POSTGRES_DSN` runs the same tests against PostgreSQL.
